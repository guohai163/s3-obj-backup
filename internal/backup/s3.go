package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type remoteBucket struct {
	name   string
	client *minio.Client
}

func (s *Service) client(ctx context.Context, connectionID int64) (*minio.Client, error) {
	var c Connection
	var encrypted []byte
	err := s.db.QueryRow(ctx, `SELECT endpoint,region,path_style,credentials FROM connections WHERE id=$1`, connectionID).
		Scan(&c.Endpoint, &c.Region, &c.PathStyle, &encrypted)
	if err != nil {
		return nil, err
	}
	secret, err := s.unseal(encrypted, c.Endpoint)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, err
	}
	lookup := minio.BucketLookupDNS
	if c.PathStyle {
		lookup = minio.BucketLookupPath
	}
	return minio.New(endpoint.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(secret.AccessKey, secret.SecretKey, secret.SessionToken),
		Secure: endpoint.Scheme == "https", Region: c.Region, BucketLookup: lookup, Transport: s.transport,
		MaxRetries: 3,
	})
}

func (s *Service) bucket(ctx context.Context, id int64) (remoteBucket, error) {
	var name string
	var connectionID int64
	err := s.db.QueryRow(ctx, "SELECT name,connection_id FROM buckets WHERE id=$1", id).Scan(&name, &connectionID)
	if err != nil {
		return remoteBucket{}, err
	}
	client, err := s.client(ctx, connectionID)
	return remoteBucket{name: name, client: client}, err
}

func (b remoteBucket) stat(ctx context.Context, key string) (minio.ObjectInfo, error) {
	return b.client.StatObject(ctx, b.name, key, minio.StatObjectOptions{})
}

func (b remoteBucket) list(ctx context.Context, prefix string, visit func(Object) error) error {
	for entry := range b.client.ListObjectsIter(ctx, b.name, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if entry.Err != nil {
			return entry.Err
		}
		if entry.Key == "" || !strings.HasPrefix(entry.Key, prefix) {
			return errors.New("S3 returned an invalid key or a key outside the requested prefix")
		}
		if err := visit(Object{Key: entry.Key, Size: entry.Size, Modified: entry.LastModified, ETag: entry.ETag}); err != nil {
			return objectError(entry.Key, err)
		}
	}
	return ctx.Err()
}

func objectMissing(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchObject", "NotFound":
		return true
	default:
		return false
	}
}

func sameObject(listed Object, info minio.ObjectInfo) bool {
	return listed.ETag != "" && !listed.Modified.IsZero() && !info.LastModified.IsZero() &&
		listed.ETag == info.ETag && listed.Size == info.Size &&
		listed.Modified.Truncate(time.Second).Equal(info.LastModified.Truncate(time.Second))
}

func (b remoteBucket) checkedDelete(ctx context.Context, o Object) error {
	current, err := b.stat(ctx, o.Key)
	if err != nil {
		return err
	}
	if !sameObject(o, current) {
		return errChanged
	}
	// R2 and some MinIO deployments lack atomic conditional DeleteObject.
	// The operator keeps writers paused from enumeration through deletion.
	return b.client.RemoveObject(ctx, b.name, o.Key, minio.RemoveObjectOptions{})
}

func (b remoteBucket) get(ctx context.Context, key, etag string) (io.ReadCloser, minio.ObjectInfo, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetMatchETag(etag); err != nil {
		return nil, minio.ObjectInfo{}, err
	}
	body, info, _, err := (minio.Core{Client: b.client}).GetObject(ctx, b.name, key, opts)
	return body, info, err
}

func (b remoteBucket) refreshObject(ctx context.Context, key string) (Object, error) {
	for entry := range b.client.ListObjectsIter(ctx, b.name, minio.ListObjectsOptions{
		Prefix: key, Recursive: true, MaxKeys: 1,
	}) {
		if entry.Err != nil {
			return Object{}, entry.Err
		}
		// An exact key sorts before any longer key with that prefix.
		if entry.Key == key {
			return Object{Key: key, Size: entry.Size, Modified: entry.LastModified, ETag: entry.ETag}, nil
		}
		break
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	return Object{}, errChanged
}

var errSourceUnstable = errors.New("source object changed or disappeared during backup")

func (b remoteBucket) openCopySource(ctx context.Context, listed Object, move bool) (io.ReadCloser, minio.ObjectInfo, Object, error) {
	// Refresh normal backups only. Moves must use the object selected before deletion.
	const attempts = 3
	for attempt := 1; ; attempt++ {
		body, info, err := b.get(ctx, listed.Key, listed.ETag)
		if err == nil {
			if sameObject(listed, info) {
				return body, info, listed, nil
			}
			body.Close()
			err = errChanged
		}
		response := minio.ToErrorResponse(err)
		if move || !(errors.Is(err, errChanged) || objectMissing(err) ||
			response.StatusCode == http.StatusPreconditionFailed || response.Code == "PreconditionFailed") {
			return nil, minio.ObjectInfo{}, Object{}, fmt.Errorf("read source: %w", err)
		}
		if attempt == attempts {
			return nil, minio.ObjectInfo{}, Object{}, errSourceUnstable
		}
		listed, err = b.refreshObject(ctx, listed.Key)
		if errors.Is(err, errChanged) {
			return nil, minio.ObjectInfo{}, Object{}, errSourceUnstable
		}
		if err != nil {
			return nil, minio.ObjectInfo{}, Object{}, fmt.Errorf("refresh source listing: %w", err)
		}
	}
}

func uploadOptions(info minio.ObjectInfo) minio.PutObjectOptions {
	options := minio.PutObjectOptions{
		ContentType: info.ContentType, ContentEncoding: info.Metadata.Get("Content-Encoding"),
		ContentDisposition: info.Metadata.Get("Content-Disposition"),
		ContentLanguage:    info.Metadata.Get("Content-Language"), CacheControl: info.Metadata.Get("Cache-Control"),
		Expires: info.Expires, UserMetadata: map[string]string{}, SendContentMd5: true,
		PartSize: 32 << 20, NumThreads: 1,
	}
	for name := range info.Metadata {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			options.UserMetadata[name[len("x-amz-meta-"):]] = info.Metadata.Get(name)
		}
	}
	return options
}

func (s *Service) copyObject(ctx context.Context, t Task, src, dst remoteBucket, o Object, key string, c *Counts) error {
	var sourceETag, targetETag string
	var sourceSize int64
	var sourceModified, targetModified time.Time
	err := s.db.QueryRow(ctx, `SELECT source_etag,source_size,source_modified,target_etag,target_modified
   FROM copies WHERE task_id=$1 AND object_key=$2`, t.ID, o.Key).
		Scan(&sourceETag, &sourceSize, &sourceModified, &targetETag, &targetModified)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && !t.Settings.Move && o.ETag != "" && o.ETag == sourceETag &&
		o.Size == sourceSize && o.Modified.Truncate(time.Microsecond).Equal(sourceModified) {
		info, err := dst.stat(ctx, key)
		if err == nil && info.ETag == targetETag && info.Size == sourceSize && info.LastModified.Equal(targetModified) {
			c.Skipped++
			return nil
		}
		if err != nil && !objectMissing(err) {
			return fmt.Errorf("check destination: %w", err)
		}
	}

	source, info, o, err := src.openCopySource(ctx, o, t.Settings.Move)
	if err != nil {
		return err
	}
	defer source.Close()

	var input io.Reader = source
	hash := sha256.New()
	if t.Settings.Move {
		input = io.TeeReader(input, hash)
	}
	opts := uploadOptions(info)
	// Increase part size for large objects so the upload stays within the S3 part count.
	if need := uint64(o.Size)/9999 + 1; need > opts.PartSize {
		opts.PartSize = (need + (1 << 20) - 1) &^ ((1 << 20) - 1)
	}
	uploaded, err := dst.client.PutObject(ctx, dst.name, key, input, o.Size, opts)
	if err != nil {
		return fmt.Errorf("upload destination: %w", err)
	}
	target, err := dst.stat(ctx, key)
	if err != nil {
		return fmt.Errorf("check uploaded destination: %w", err)
	}
	if uploaded.ETag == "" || target.ETag != uploaded.ETag || target.Size != o.Size || target.LastModified.IsZero() {
		return errors.New("uploaded destination failed verification")
	}
	if t.Settings.Move {
		readback, readInfo, err := dst.get(ctx, key, target.ETag)
		if err != nil {
			return fmt.Errorf("read back destination: %w", err)
		}
		digest := sha256.New()
		size, err := io.Copy(digest, readback)
		readback.Close()
		if err != nil {
			return fmt.Errorf("verify destination bytes: %w", err)
		}
		if size != o.Size || readInfo.ETag != target.ETag || !readInfo.LastModified.Equal(target.LastModified) ||
			!bytes.Equal(hash.Sum(nil), digest.Sum(nil)) {
			return errors.New("destination bytes failed SHA-256 verification")
		}
	}
	_, err = s.db.Exec(ctx, `INSERT INTO copies(task_id,object_key,source_etag,source_size,source_modified,target_etag,target_modified)
   VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(task_id,object_key) DO UPDATE SET
   source_etag=excluded.source_etag,source_size=excluded.source_size,source_modified=excluded.source_modified,
   target_etag=excluded.target_etag,target_modified=excluded.target_modified`,
		t.ID, o.Key, o.ETag, o.Size, o.Modified.Truncate(time.Microsecond), target.ETag, target.LastModified)
	if err != nil {
		return err
	}
	c.Copied++
	if t.Settings.Move {
		if err := src.checkedDelete(ctx, o); err != nil {
			return fmt.Errorf("verified copy exists, source deletion failed: %w", err)
		}
		c.Deleted++
	}
	return nil
}
