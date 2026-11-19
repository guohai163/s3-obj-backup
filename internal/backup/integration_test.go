package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestBackupWorkflow(t *testing.T) {
	database, endpoint := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_S3_ENDPOINT")
	if database == "" || endpoint == "" {
		t.Skip("set TEST_DATABASE_URL and TEST_S3_ENDPOINT for the PostgreSQL + S3 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schema := fmt.Sprintf("s3test_%d", time.Now().UnixNano())
	db, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	dsn, err := url.Parse(database)
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", schema)
	dsn.RawQuery = q.Encode()

	access, secret := os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY")
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := minio.New(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4(access, secret, ""), Secure: u.Scheme == "https",
		Region: "us-east-1", BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceName, targetName := strings.ReplaceAll(schema, "_", "-")+"-source", strings.ReplaceAll(schema, "_", "-")+"-target"
	for _, name := range []string{sourceName, targetName} {
		if err := remote.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			for o := range remote.ListObjectsIter(ctx, name, minio.ListObjectsOptions{Recursive: true}) {
				if o.Err == nil {
					remote.RemoveObject(ctx, name, o.Key, minio.RemoveObjectOptions{})
				}
			}
			remote.RemoveBucket(ctx, name)
		}()
	}
	const password = "only-a-test-password"
	cfg := Config{DatabaseURL: dsn.String(), EncryptionKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		AdminUser: "admin", AdminPassword: password, ViewerUser: "viewer", ViewerPassword: password}
	s, err := Open(ctx, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Opening the same database again must leave the existing schema usable.
	secondary, err := Open(ctx, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	secondary.Close()
	// Overwrite two fixture objects between LIST and the conditional GET.
	// The first settles after one update. The second keeps changing.
	var recovered, unstable int
	concurrentRead := make(chan struct{})
	var concurrentReadOnce sync.Once
	s.transport.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.Method == http.MethodGet && req.Header.Get("If-Match") != "" {
			key, inSource := strings.CutPrefix(req.URL.Path, "/"+sourceName+"/")
			var revision int
			switch {
			case inSource && key == "updates/01-recovered" && recovered == 0:
				// The later key must reach GET while the first key is still in flight.
				select {
				case <-concurrentRead:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				recovered++
				revision = recovered
			case inSource && key == "updates/02-unstable" && unstable < 3:
				unstable++
				revision = unstable
			case inSource && key == "updates/03-later":
				concurrentReadOnce.Do(func() { close(concurrentRead) })
			}
			if revision != 0 {
				body := fmt.Sprintf("revision %d", revision)
				if _, err := remote.PutObject(req.Context(), sourceName, key, strings.NewReader(body),
					int64(len(body)), minio.PutObjectOptions{}); err != nil {
					return nil, fmt.Errorf("update test fixture: %w", err)
				}
			}
		}
		return http.ProxyFromEnvironment(req)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Work(workerCtx) }()
	defer func() { stop(); <-done }()
	getClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, Timeout: 15 * time.Second}
	}
	admin, viewer := getClient(), getClient()

	request := func(client *http.Client, method, path string, body any, want int, data any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, resp.StatusCode, want, raw)
		}
		if data != nil {
			if err := json.Unmarshal(raw, data); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
		}
	}
	waitRun := func(taskID int64, mode string, after int64, expectedState ...string) Run {
		t.Helper()
		want := "success"
		if len(expectedState) != 0 {
			want = expectedState[0]
		}
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			var runs []Run
			request(admin, "GET", "/api/runs", nil, 200, &runs)
			for _, r := range runs {
				if r.TaskID != taskID || r.ID <= after || r.Mode != mode {
					continue
				}
				if r.State == "failed" || r.State == "success" {
					if r.State != want {
						t.Fatalf("run %d %s: got %s, want %s: %s", r.ID, mode, r.State, want, r.Detail)
					}
					return r
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("task %d mode %s did not finish", taskID, mode)
		return Run{}
	}
	put := func(key string, data []byte) {
		t.Helper()
		_, err := remote.PutObject(ctx, sourceName, key, bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{ContentType: "application/octet-stream", UserMetadata: map[string]string{"origin": "integration"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	request(admin, "GET", "/api/config", nil, 401, nil)
	request(admin, "POST", "/api/login", map[string]string{"user": "admin", "password": password}, 200, nil)
	request(viewer, "POST", "/api/login", map[string]string{"user": "viewer", "password": password}, 200, nil)
	var source, target Bucket
	for i, b := range []*Bucket{&source, &target} {
		var connection Connection
		name := []string{sourceName, targetName}[i]
		request(admin, "POST", "/api/connections", map[string]any{
			"name": name, "endpoint": endpoint, "region": "us-east-1", "path_style": true, "access_key": access, "secret_key": secret,
		}, 200, &connection)
		request(admin, "POST", "/api/buckets", Bucket{ConnectionID: connection.ID, Name: name}, 200, b)
	}
	save := func(task Task, status int) Task {
		t.Helper()
		var result Task
		request(admin, "POST", "/api/tasks", task, status, &result)
		return result
	}
	submit := func(task Task, mode string, plan *int64, confirm bool, status int) {
		t.Helper()
		request(admin, "POST", fmt.Sprintf("/api/tasks/%d/run", task.ID),
			map[string]any{"mode": mode, "plan_id": plan, "confirm": confirm}, status, nil)
	}
	suffix := "阶段 + 100%.bin"
	content := bytes.Repeat([]byte{0x47}, 33<<20)
	put("incoming/"+suffix, content)
	put("incoming/empty", nil)
	put("incoming2/unrelated", []byte("keep"))
	backup := save(Task{Name: "Backup", Kind: "backup", SourceID: source.ID, TargetID: &target.ID,
		Settings: Settings{SourcePrefix: "incoming", TargetPrefix: "archive"}}, 200)
	save(Task{Name: "Overlap", Kind: "backup", SourceID: source.ID, TargetID: &source.ID,
		Settings: Settings{SourcePrefix: "incoming", TargetPrefix: "incoming/nested"}}, 400)
	submit(backup, "manual", nil, false, 202)
	full := waitRun(backup.ID, "manual", 0)
	if full.Copied != 2 || full.Deleted != 0 {
		t.Fatalf("unexpected first backup: %+v", full)
	}
	info, err := remote.StatObject(ctx, targetName, "archive/"+suffix, minio.StatObjectOptions{})
	if err != nil || info.Size != int64(len(content)) || info.Metadata.Get("X-Amz-Meta-Origin") != "integration" {
		t.Fatalf("multipart object/metadata: size=%d, metadata=%v, err=%v", info.Size, info.Metadata, err)
	}
	submit(backup, "manual", nil, false, 202)
	incremental := waitRun(backup.ID, "manual", full.ID)
	if incremental.Skipped != 2 || incremental.Copied != 0 {
		t.Fatalf("unexpected incremental backup: %+v", incremental)
	}
	put("incoming/empty", []byte("changed"))
	if err := remote.RemoveObject(ctx, targetName, "archive/"+suffix, minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	submit(backup, "manual", nil, false, 202)
	repaired := waitRun(backup.ID, "manual", incremental.ID)
	if repaired.Copied != 2 {
		t.Fatalf("expected changed and missing objects to be copied: %+v", repaired)
	}

	index := save(Task{Name: "Index", Kind: "index", SourceID: source.ID, Enabled: true, Settings: Settings{Cron: "0 3 * * *", Timezone: "Asia/Shanghai"}}, 200)
	if index.NextRun == nil {
		t.Fatal("schedule was not persisted")
	}
	if _, err := s.db.Exec(ctx, "UPDATE tasks SET next_run=now()-interval '3 seconds' WHERE id=$1", index.ID); err != nil {
		t.Fatal(err)
	}
	indexed := waitRun(index.ID, "scheduled", 0)
	if indexed.Scanned != 3 {
		t.Fatalf("unexpected index count: %+v", indexed)
	}
	type objectPage struct{ Items []Object }
	var result objectPage
	request(viewer, "GET", fmt.Sprintf("/api/objects?browse=1&bucket=%d", source.ID), nil, 200, &result)
	if len(result.Items) != 2 || !result.Items[0].Directory || result.Items[0].Key != "incoming/" {
		t.Fatalf("unexpected folders: %+v", result)
	}
	request(viewer, "GET", "/api/objects?q="+url.QueryEscape("100%"), nil, 200, &result)
	if len(result.Items) != 1 || result.Items[0].Key != "incoming/"+suffix {
		t.Fatalf("unexpected search: %+v", result)
	}

	cleanup := save(Task{Name: "Cleanup", Kind: "cleanup", SourceID: source.ID,
		Settings: Settings{SourcePrefix: "incoming", CleanupMode: "all", Quiescent: true}}, 200)
	route := fmt.Sprintf("/api/tasks/%d/run", cleanup.ID)
	request(viewer, "POST", route, map[string]any{"mode": "preview"}, 403, nil)
	submit(cleanup, "manual", nil, true, 400)
	submit(cleanup, "preview", nil, false, 202)
	plan := waitRun(cleanup.ID, "preview", 0)
	if !plan.CanExecute || plan.Matched != 2 {
		t.Fatalf("unexpected preview: %+v", plan)
	}
	request(admin, "GET", fmt.Sprintf("/api/runs/%d/items", plan.ID), nil, 200, &result)
	if len(result.Items) != 2 {
		t.Fatalf("unexpected preview list: %+v", result)
	}
	// Replace one candidate and add a new object after preview. Both must survive.
	put("incoming/empty", []byte("changed after preview"))
	put("incoming/new", []byte("new"))
	submit(cleanup, "manual", &plan.ID, false, 400)
	submit(cleanup, "manual", &plan.ID, true, 202)
	removed := waitRun(cleanup.ID, "manual", plan.ID)
	if removed.Deleted != 1 || removed.Skipped != 1 {
		t.Fatalf("cleanup did not honor its preview: %+v", removed)
	}
	submit(cleanup, "manual", &plan.ID, true, 409)

	future := time.Now().Add(24 * time.Hour)
	before := save(Task{Name: "Scheduled cleanup", Kind: "cleanup", SourceID: source.ID, Enabled: true,
		Settings: Settings{SourcePrefix: "incoming2", CleanupMode: "before", Cutoff: &future, Quiescent: true, Cron: "0 6 * * *", Timezone: "UTC"}}, 200)
	if _, err := s.db.Exec(ctx, "UPDATE tasks SET next_run=now()-interval '3 seconds' WHERE id=$1", before.ID); err != nil {
		t.Fatal(err)
	}
	scheduled := waitRun(before.ID, "scheduled", 0)
	if scheduled.Deleted != 1 {
		t.Fatalf("scheduled cleanup: %+v", scheduled)
	}

	move := save(Task{Name: "Move", Kind: "backup", SourceID: source.ID, TargetID: &target.ID,
		Settings: Settings{SourcePrefix: "incoming", TargetPrefix: "moved", Move: true, Quiescent: true}}, 200)
	submit(move, "manual", nil, true, 202)
	moved := waitRun(move.ID, "manual", 0)
	if moved.Copied != 2 || moved.Deleted != 2 {
		t.Fatalf("unexpected move: %+v", moved)
	}
	if _, err := remote.StatObject(ctx, targetName, "moved/new", minio.StatObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.StatObject(ctx, sourceName, "incoming/new", minio.StatObjectOptions{}); !objectMissing(err) {
		t.Fatalf("source should be removed, got %v", err)
	}

	put("updates/01-recovered", []byte("initial"))
	put("updates/02-unstable", []byte("initial"))
	put("updates/03-later", []byte("stable"))
	updates := save(Task{Name: "Back up changing objects", Kind: "backup", SourceID: source.ID, TargetID: &target.ID,
		Settings: Settings{SourcePrefix: "updates", TargetPrefix: "updates"}}, 200)
	submit(updates, "manual", nil, false, 202)
	partial := waitRun(updates.ID, "manual", 0, "failed")
	if partial.Scanned != 3 || partial.Copied != 2 || partial.Skipped != 0 ||
		!strings.Contains(partial.Detail, "Could not copy 1 source objects") || !strings.Contains(partial.Detail, "02-unstable") {
		t.Fatalf("expected only the continuously changing object to remain pending: %+v", partial)
	}
	for key, want := range map[string]string{"updates/01-recovered": "revision 1", "updates/03-later": "stable"} {
		object, err := remote.GetObject(ctx, targetName, key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(object)
		object.Close()
		if err != nil || string(data) != want {
			t.Fatalf("%s: got %q, want %q, err=%v", key, data, want, err)
		}
	}
	submit(updates, "manual", nil, false, 202)
	resumed := waitRun(updates.ID, "manual", partial.ID)
	if resumed.Copied != 1 || resumed.Skipped != 2 {
		t.Fatalf("expected the next run to reuse both valid checkpoints: %+v", resumed)
	}
	request(viewer, "POST", "/api/logout", map[string]any{}, 200, nil)
	request(viewer, "GET", "/api/config", nil, 401, nil)
}
