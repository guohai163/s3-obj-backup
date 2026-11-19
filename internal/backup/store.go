package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type scanner interface{ Scan(...any) error }

func collect[T any](rows pgx.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []T{}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	return results, rows.Err()
}

func scanConnection(row scanner) (c Connection, err error) {
	err = row.Scan(&c.ID, &c.Name, &c.Endpoint, &c.Region, &c.PathStyle)
	return
}
func scanBucket(row scanner) (b Bucket, err error) {
	err = row.Scan(&b.ID, &b.ConnectionID, &b.Name, &b.IndexedAt, &b.ObjectCount)
	return
}

const taskColumns = "id,name,kind,source_id,target_id,enabled,next_run,revision,settings"

func scanTask(row scanner) (t Task, err error) {
	err = row.Scan(&t.ID, &t.Name, &t.Kind, &t.SourceID, &t.TargetID, &t.Enabled, &t.NextRun, &t.Revision, &t.Settings)
	return
}

func (s *Service) saveConnection(ctx context.Context, c Connection, creds Credentials) (Connection, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 160 {
		return c, invalid("Enter a connection name.")
	}
	if c.ID != 0 {
		existing, err := scanConnection(s.db.QueryRow(ctx, "SELECT id,name,endpoint,region,path_style FROM connections WHERE id=$1", c.ID))
		if err != nil {
			return c, err
		}
		// A different endpoint may refer to different data. Keep existing bucket references stable.
		c.Endpoint = existing.Endpoint
	} else {
		var err error
		c.Endpoint, err = normalizeEndpoint(c.Endpoint)
		if err != nil {
			return c, err
		}
	}
	c.Region = strings.TrimSpace(c.Region)
	if c.Region == "" {
		c.Region = "auto"
	}
	var encrypted []byte
	if creds.AccessKey != "" || creds.SecretKey != "" || creds.SessionToken != "" || c.ID == 0 {
		if creds.AccessKey == "" || creds.SecretKey == "" {
			return c, invalid("Enter both the access key and the secret key.")
		}
		var err error
		encrypted, err = s.seal(creds, c.Endpoint)
		if err != nil {
			return c, err
		}
	}
	if c.ID != 0 {
		_, err := s.db.Exec(ctx, `UPDATE connections SET name=$2,region=$3,path_style=$4,credentials=COALESCE($5,credentials) WHERE id=$1`,
			c.ID, c.Name, c.Region, c.PathStyle, encrypted)
		return c, err
	}
	err := s.db.QueryRow(ctx, `INSERT INTO connections(name,endpoint,region,path_style,credentials)
 VALUES($1,$2,$3,$4,$5) RETURNING id`, c.Name, c.Endpoint, c.Region, c.PathStyle, encrypted).Scan(&c.ID)
	return c, err
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func (s *Service) addBucket(ctx context.Context, b Bucket) (Bucket, error) {
	b.Name = strings.TrimSpace(b.Name)
	if b.ConnectionID < 1 || !bucketPattern.MatchString(b.Name) {
		return b, invalid("Select a connection and enter an S3 bucket name.")
	}
	err := s.db.QueryRow(ctx, `INSERT INTO buckets(connection_id,name) VALUES($1,$2) RETURNING id`, b.ConnectionID, b.Name).Scan(&b.ID)
	return b, err
}

func (s *Service) saveTask(ctx context.Context, t Task) (Task, error) {
	if err := t.validate(time.Now()); err != nil {
		return t, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return t, err
	}
	defer tx.Rollback(ctx)
	if t.ID != 0 {
		old, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=$1 FOR UPDATE", t.ID))
		if err != nil {
			return t, err
		}
		if t.Revision != old.Revision {
			return t, conflict("The task changed. Reload it and try again.")
		}
		if t.Kind != old.Kind {
			return t, invalid("Create a new task to change the task type.")
		}
		var active bool
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE task_id=$1 AND state IN ('queued','running'))", t.ID).Scan(&active)
		if err != nil {
			return t, err
		}
		if active {
			return t, conflict("Wait for the active run before editing this task.")
		}
		if t.Kind == "backup" && (t.SourceID != old.SourceID || *t.TargetID != *old.TargetID ||
			t.Settings.SourcePrefix != old.Settings.SourcePrefix || t.Settings.TargetPrefix != old.Settings.TargetPrefix) {
			if _, err = tx.Exec(ctx, "DELETE FROM copies WHERE task_id=$1", t.ID); err != nil {
				return t, err
			}
		}
	}
	if t.TargetID != nil {
		var same bool
		err = tx.QueryRow(ctx, `SELECT a.name=b.name AND ca.endpoint=cb.endpoint
    FROM buckets a JOIN connections ca ON ca.id=a.connection_id
    CROSS JOIN buckets b JOIN connections cb ON cb.id=b.connection_id WHERE a.id=$1 AND b.id=$2`,
			t.SourceID, *t.TargetID).Scan(&same)
		if err != nil {
			return t, err
		}
		if same && overlaps(t.Settings.SourcePrefix, t.Settings.TargetPrefix) {
			return t, invalid("Source and destination folders overlap in the same bucket.")
		}
	}
	if t.ID == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO tasks(name,kind,source_id,target_id,enabled,next_run,settings)
      VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,revision`,
			t.Name, t.Kind, t.SourceID, t.TargetID, t.Enabled, t.NextRun, t.Settings).Scan(&t.ID, &t.Revision)
	} else {
		err = tx.QueryRow(ctx, `UPDATE tasks SET name=$2,source_id=$3,target_id=$4,enabled=$5,
    next_run=$6,settings=$7,revision=revision+1 WHERE id=$1 RETURNING revision`,
			t.ID, t.Name, t.SourceID, t.TargetID, t.Enabled, t.NextRun, t.Settings).Scan(&t.Revision)
	}
	if err != nil {
		return t, err
	}
	return t, tx.Commit(ctx)
}

const planPredicate = `p.mode='preview' AND p.state='success' AND p.revision=t.revision
  AND p.finished_at > now()-interval '1 hour' AND NOT EXISTS (SELECT 1 FROM runs used WHERE used.plan_id=p.id)`

func (s *Service) enqueue(ctx context.Context, id int64, mode string, plan *int64, confirm bool) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	t, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return 0, err
	}
	if mode == "manual" && (t.Kind == "cleanup" || t.Settings.Move) && !confirm {
		return 0, invalid("Confirm that writers are paused and authorize deletion.")
	}
	switch {
	case t.Kind == "cleanup" && mode == "manual":
		if !t.Settings.Quiescent {
			return 0, invalid("Confirm that writers are paused before configuring cleanup execution.")
		}
		if plan == nil {
			return 0, invalid("A successful preview is required before manual cleanup.")
		}
		var usable bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs p JOIN tasks t ON t.id=p.task_id
    WHERE p.id=$1 AND t.id=$2 AND `+planPredicate+`)`, *plan, id).Scan(&usable)
		if err != nil {
			return 0, err
		}
		if !usable {
			return 0, conflict("The preview expired, was used, or belongs to an earlier task configuration.")
		}
	case t.Kind != "cleanup" && mode == "preview":
		return 0, invalid("Only cleanup tasks have previews.")
	case plan != nil:
		return 0, invalid("Unexpected preview ID.")
	}
	var runID int64
	err = tx.QueryRow(ctx, `INSERT INTO runs(task_id,revision,mode,plan_id)
  VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING RETURNING id`, id, t.Revision, mode, plan).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, conflict("This task already has an active run.")
	}
	if err != nil {
		return 0, err
	}
	return runID, tx.Commit(ctx)
}

func scanRun(row scanner) (r Run, err error) {
	err = row.Scan(&r.ID, &r.TaskID, &r.TaskName, &r.Kind, &r.Mode, &r.PlanID, &r.State,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.Scanned, &r.Copied, &r.Skipped,
		&r.Deleted, &r.Matched, &r.Detail, &r.CanExecute)
	return
}

func (s *Service) runs(ctx context.Context, before int64) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT p.id,p.task_id,t.name,t.kind,p.mode,p.plan_id,p.state,
  p.created_at,p.started_at,p.finished_at,p.scanned,p.copied,p.skipped,p.deleted,p.matched,p.detail,
  COALESCE((`+planPredicate+`) AND p.matched>0,false)
  FROM runs p JOIN tasks t ON t.id=p.task_id WHERE ($1::bigint=0 OR p.id<$1)
  ORDER BY p.id DESC LIMIT 40`, before)
	return collect(rows, err, scanRun)
}

func scanObject(row scanner) (o Object, err error) {
	err = row.Scan(&o.BucketID, &o.Key, &o.Size, &o.Modified, &o.Directory)
	return
}

func likePrefix(p string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(p) + "%"
}

func (s *Service) inventory(ctx context.Context, bucket int64, p, q, after string, afterBucket int64, browse bool) ([]Object, error) {
	if browse && bucket == 0 {
		return nil, invalid("Select a bucket to browse folders.")
	}
	if browse {
		rows, err := s.db.Query(ctx, `WITH entries AS (
   SELECT i.bucket_id,
     $2::text || split_part(substr(i.object_key, length($2)+1), '/', 1) ||
     CASE WHEN strpos(substr(i.object_key, length($2)+1),'/')>0 THEN '/' ELSE '' END AS entry,
     i.size,i.modified, strpos(substr(i.object_key, length($2)+1),'/')>0 AS dir
   FROM inventory i JOIN buckets b ON b.id=i.bucket_id AND b.generation=i.generation
   WHERE i.bucket_id=$1 AND i.object_key LIKE $3 AND i.object_key<>$2 AND i.object_key>$4
  )
  SELECT bucket_id,entry,CASE WHEN dir THEN 0 ELSE max(size) END,max(modified),dir
  FROM entries WHERE entry COLLATE "C">$4 GROUP BY bucket_id,entry,dir ORDER BY entry COLLATE "C" LIMIT 101`,
			bucket, p, likePrefix(p), after)
		return collect(rows, err, scanObject)
	}
	rows, err := s.db.Query(ctx, `SELECT i.bucket_id,i.object_key,i.size,i.modified,false
  FROM inventory i JOIN buckets b ON b.id=i.bucket_id AND b.generation=i.generation
  WHERE ($1::bigint=0 OR i.bucket_id=$1) AND i.object_key LIKE $2
  AND (i.bucket_id,i.object_key)>($4,$5) AND lower(i.object_key) LIKE $3
  ORDER BY i.bucket_id,i.object_key LIMIT 101`, bucket, likePrefix(p), "%"+strings.ToLower(likePrefix(q)), afterBucket, after)
	return collect(rows, err, scanObject)
}

func (s *Service) previewItems(ctx context.Context, id int64, after string) ([]Object, error) {
	rows, err := s.db.Query(ctx, `SELECT t.source_id,c.object_key,c.size,c.modified,false FROM candidates c
  JOIN runs r ON r.id=c.run_id JOIN tasks t ON t.id=r.task_id
  WHERE r.id=$1 AND r.mode='preview' AND r.state='success' AND c.object_key>$2
  ORDER BY c.object_key LIMIT 101`, id, after)
	return collect(rows, err, scanObject)
}

func (s *Service) markProgress(ctx context.Context, id int64, c Counts) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET scanned=$2,copied=$3,skipped=$4,deleted=$5,matched=$6
  WHERE id=$1 AND state='running'`, id, c.Scanned, c.Copied, c.Skipped, c.Deleted, c.Matched)
	return err
}

func objectError(key string, err error) error { return fmt.Errorf("object %q: %w", key, err) }
