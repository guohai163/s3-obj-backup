package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

func (s *Service) Work(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.workSession(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("worker session ended", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) workSession(ctx context.Context) error {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return err
	}
	var locked bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(71428722, hashtext(current_schema()))").Scan(&locked)
	if err != nil || !locked {
		conn.Release()
		return err
	}
	// Use a dedicated PostgreSQL session. Closing it also releases the lock after any error.
	raw := conn.Hijack()
	lockedCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-lockedCtx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(lockedCtx, 3*time.Second)
				err := raw.Ping(check)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-done
		timeout, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		raw.Close(timeout)
	}()
	_, err = s.db.Exec(lockedCtx, `UPDATE runs SET state='failed',finished_at=now(),
  detail='Run interrupted. Check recorded progress and start a new run.' WHERE state='running'`)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for lockedCtx.Err() == nil {
		if err := s.schedule(lockedCtx); err != nil {
			return err
		}
		var id int64
		err := s.db.QueryRow(lockedCtx, `UPDATE runs SET state='running',started_at=now()
   WHERE id=(SELECT id FROM runs WHERE state='queued' ORDER BY id LIMIT 1)
   RETURNING id`).Scan(&id)
		switch {
		case err == nil:
			if err := s.perform(lockedCtx, id); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		default:
			select {
			case <-lockedCtx.Done():
				return lockedCtx.Err()
			case <-ticker.C:
			}
		}
	}
	return lockedCtx.Err()
}

func (s *Service) schedule(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, "SELECT "+taskColumns+` FROM tasks WHERE enabled AND next_run<=$1
  ORDER BY next_run,id LIMIT 100 FOR UPDATE SKIP LOCKED`, now)
	tasks, err := collect(rows, err, scanTask)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		next, err := nextRun(t.Settings, now)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE tasks SET next_run=$2 WHERE id=$1", t.ID, next); err != nil {
			return err
		}
		state, detail := "queued", ""
		if (t.Kind == "cleanup" || t.Settings.Move) && now.Sub(*t.NextRun) >= time.Minute {
			state, detail = "failed", "Destructive schedule missed its start minute."
		}
		if _, err = tx.Exec(ctx, `INSERT INTO runs(task_id,revision,mode,created_at,state,finished_at,detail)
    VALUES($1,$2,'scheduled',$3,$4,CASE WHEN $4='failed' THEN now() ELSE NULL END,$5)
    ON CONFLICT DO NOTHING`, t.ID, t.Revision, *t.NextRun, state, detail); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM candidates c USING runs p WHERE c.run_id=p.id AND
  p.state IN ('success','failed') AND (p.mode<>'preview' OR p.finished_at<now()-interval '1 hour')
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.plan_id=p.id AND r.state IN ('queued','running'))`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at<now()")
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) perform(ctx context.Context, id int64) error {
	var r Run
	var revision int64
	err := s.db.QueryRow(ctx, `SELECT task_id,revision,mode,plan_id,created_at FROM runs WHERE id=$1`, id).
		Scan(&r.TaskID, &revision, &r.Mode, &r.PlanID, &r.CreatedAt)
	if err != nil {
		return err
	}
	t, err := scanTask(s.db.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=$1", r.TaskID))
	if err != nil {
		return err
	}
	if t.Revision != revision {
		err = errors.New("Task configuration changed.")
	} else {
		runCtx, cancel := context.WithCancel(ctx)
		err = s.execute(runCtx, id, r, t, &r.Counts)
		cancel()
	}
	state, detail := "success", ""
	if err != nil {
		state, detail = "failed", strings.ToValidUTF8(err.Error(), "�")
		if len(detail) > 2500 {
			detail = strings.ToValidUTF8(detail[:2500], "�")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = s.db.Exec(ctx, `UPDATE runs SET state=$2,detail=$3,finished_at=now(),scanned=$4,copied=$5,
  skipped=$6,deleted=$7,matched=$8 WHERE id=$1`, id, state, detail, r.Scanned, r.Copied, r.Skipped, r.Deleted, r.Matched)
	return err
}

func (s *Service) execute(ctx context.Context, id int64, r Run, t Task, c *Counts) error {
	destructive := t.Settings.Move || (t.Kind == "cleanup" && r.Mode != "preview")
	if destructive && !t.Settings.Quiescent {
		return errors.New("Writers must remain paused throughout the run.")
	}
	if destructive && r.Mode == "scheduled" && time.Since(r.CreatedAt) >= time.Minute {
		return errors.New("Destructive schedule missed its start minute.")
	}
	if destructive && r.Mode == "manual" && time.Since(r.CreatedAt) >= 5*time.Minute {
		return errors.New("Manual destructive run waited more than five minutes. Verify writers are paused and retry.")
	}
	src, err := s.bucket(ctx, t.SourceID)
	if err != nil {
		return err
	}
	switch t.Kind {
	case "index":
		return s.index(ctx, id, t.SourceID, src, c)
	case "cleanup":
		plan := id
		if r.PlanID != nil {
			plan = *r.PlanID
		} else {
			if err := s.stage(ctx, id, src, t.Settings.SourcePrefix, t.Settings.cutoff(time.Now()), c); err != nil {
				return err
			}
		}
		if r.Mode == "preview" {
			return nil
		}
		return s.visitCandidates(ctx, plan, func(o Object) error {
			if r.PlanID != nil {
				c.Scanned++
				c.Matched++
			}
			err := src.checkedDelete(ctx, o)
			if errors.Is(err, errChanged) || objectMissing(err) {
				c.Skipped++
			} else if err != nil {
				return err
			} else {
				c.Deleted++
			}
			return s.markProgress(ctx, id, *c)
		})
	case "backup":
		dst, err := s.bucket(ctx, *t.TargetID)
		if err != nil {
			return err
		}
		copy := func(ctx context.Context, o Object, counts *Counts) error {
			key := t.Settings.TargetPrefix + strings.TrimPrefix(o.Key, t.Settings.SourcePrefix)
			if key == "" {
				// An S3 folder marker at the source prefix has no key at a bucket root.
				counts.Skipped++
				return nil
			}
			return s.copyObject(ctx, t, src, dst, o, key, counts)
		}
		if t.Settings.Move {
			if err := s.stage(ctx, id, src, t.Settings.SourcePrefix, nil, c); err != nil {
				return err
			}
			return s.visitCandidates(ctx, id, func(o Object) error {
				if err := copy(ctx, o, c); err != nil {
					return err
				}
				return s.markProgress(ctx, id, *c)
			})
		}
		return s.backupObjects(ctx, id, src, t.Settings.SourcePrefix, c, copy)
	}
	return fmt.Errorf("unknown task type %q", t.Kind)
}

func (s *Service) backupObjects(ctx context.Context, id int64, src remoteBucket, prefix string, counts *Counts,
	copy func(context.Context, Object, *Counts) error,
) error {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	group, work := errgroup.WithContext(work)
	jobs := make(chan Object)
	type result struct {
		counts   Counts
		unstable string
	}
	results := make(chan result, s.cfg.BackupWorkers)
	group.Go(func() error {
		defer close(jobs)
		return src.list(work, prefix, func(object Object) error {
			select {
			case jobs <- object:
				return nil
			case <-work.Done():
				return work.Err()
			}
		})
	})
	for range s.cfg.BackupWorkers {
		group.Go(func() error {
			for object := range jobs {
				if err := work.Err(); err != nil {
					return err
				}
				var item result
				err := copy(work, object, &item.counts)
				if errors.Is(err, errSourceUnstable) {
					item.unstable = object.Key
					err = nil
				}
				results <- item
				if err != nil {
					return objectError(object.Key, err)
				}
			}
			return nil
		})
	}
	outcome := make(chan error, 1)
	go func() {
		outcome <- group.Wait()
		close(results)
	}()
	var unstable int64
	var firstUnstable string
	var progressErr error
	// Only this goroutine updates the run's shared totals.
	for item := range results {
		counts.Scanned++
		counts.Copied += item.counts.Copied
		counts.Skipped += item.counts.Skipped
		if item.unstable != "" {
			if unstable == 0 {
				firstUnstable = item.unstable
			}
			unstable++
		}
		if progressErr == nil {
			progressErr = s.markProgress(ctx, id, *counts)
			if progressErr != nil {
				cancel()
			}
		}
	}
	err := <-outcome
	if progressErr != nil {
		return progressErr
	}
	if unstable == 0 {
		return err
	}
	return errors.Join(err, fmt.Errorf(
		"Could not copy %d source objects that changed or disappeared during backup. First affected key is %q.",
		unstable, firstUnstable))
}

func (s *Service) stage(ctx context.Context, id int64, src remoteBucket, prefix string, cutoff *time.Time, c *Counts) error {
	batch := make([][]any, 0, 500)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := s.db.CopyFrom(ctx, pgx.Identifier{"candidates"}, []string{"run_id", "object_key", "etag", "size", "modified"}, pgx.CopyFromRows(batch))
		batch = batch[:0]
		if err != nil {
			return err
		}
		return s.markProgress(ctx, id, *c)
	}
	err := src.list(ctx, prefix, func(o Object) error {
		c.Scanned++
		if cutoff != nil && !o.Modified.Before(*cutoff) {
			return nil
		}
		if o.Modified.IsZero() || o.ETag == "" {
			return errors.New("missing modification date or ETag")
		}
		batch = append(batch, []any{id, o.Key, o.ETag, o.Size, o.Modified})
		c.Matched++
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (s *Service) visitCandidates(ctx context.Context, plan int64, visit func(Object) error) error {
	var after string
	for {
		rows, err := s.db.Query(ctx, `SELECT object_key,etag,size,modified FROM candidates
   WHERE run_id=$1 AND object_key>$2 ORDER BY object_key LIMIT 500`, plan, after)
		list, err := collect(rows, err, func(row scanner) (o Object, err error) {
			err = row.Scan(&o.Key, &o.ETag, &o.Size, &o.Modified)
			return
		})
		if err != nil {
			return err
		}
		for _, o := range list {
			if err := visit(o); err != nil {
				return objectError(o.Key, err)
			}
		}
		if len(list) < 500 {
			return nil
		}
		after = list[len(list)-1].Key
	}
}

func (s *Service) index(ctx context.Context, id, bucketID int64, src remoteBucket, c *Counts) error {
	// Keep the previous generation available until the full scan is committed.
	_, err := s.db.Exec(ctx, `DELETE FROM inventory i USING buckets b
  WHERE i.bucket_id=b.id AND b.id=$1 AND i.generation<>b.generation`, bucketID)
	if err != nil {
		return err
	}
	batch := make([][]any, 0, 500)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := s.db.CopyFrom(ctx, pgx.Identifier{"inventory"}, []string{"bucket_id", "generation", "object_key", "size", "modified"}, pgx.CopyFromRows(batch))
		batch = batch[:0]
		if err != nil {
			return err
		}
		return s.markProgress(ctx, id, *c)
	}
	err = src.list(ctx, "", func(o Object) error {
		c.Scanned++
		batch = append(batch, []any{bucketID, id, o.Key, o.Size, o.Modified})
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE buckets SET generation=$2,indexed_at=now(),object_count=$3 WHERE id=$1`,
		bucketID, id, c.Scanned); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM inventory WHERE bucket_id=$1 AND generation<>$2`, bucketID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
