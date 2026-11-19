package backup

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type Connection struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	PathStyle bool   `json:"path_style"`
}

type Credentials struct {
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	SessionToken string `json:"session_token,omitempty"`
}

type Bucket struct {
	ID           int64      `json:"id"`
	ConnectionID int64      `json:"connection_id"`
	Name         string     `json:"name"`
	IndexedAt    *time.Time `json:"indexed_at"`
	ObjectCount  int64      `json:"object_count"`
}

type Task struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	SourceID int64      `json:"source_id"`
	TargetID *int64     `json:"target_id"`
	Enabled  bool       `json:"enabled"`
	NextRun  *time.Time `json:"next_run"`
	Revision int64      `json:"revision"`
	Settings Settings   `json:"settings"`
}

type Settings struct {
	SourcePrefix string     `json:"source_prefix"`
	TargetPrefix string     `json:"target_prefix"`
	Move         bool       `json:"move"`
	Quiescent    bool       `json:"quiescent"`
	CleanupMode  string     `json:"cleanup_mode"`
	Cutoff       *time.Time `json:"cutoff"`
	RetainDays   int        `json:"retain_days"`
	Cron         string     `json:"cron"`
	Timezone     string     `json:"timezone"`
}

type Counts struct {
	Scanned int64 `json:"scanned"`
	Copied  int64 `json:"copied"`
	Skipped int64 `json:"skipped"`
	Deleted int64 `json:"deleted"`
	Matched int64 `json:"matched"`
}

type Run struct {
	ID         int64      `json:"id"`
	TaskID     int64      `json:"task_id"`
	TaskName   string     `json:"task_name"`
	Kind       string     `json:"kind"`
	Mode       string     `json:"mode"`
	PlanID     *int64     `json:"plan_id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Counts
	Detail     string `json:"detail"`
	CanExecute bool   `json:"can_execute"`
}

type Object struct {
	BucketID  int64     `json:"bucket_id,omitempty"`
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Directory bool      `json:"directory,omitempty"`
	ETag      string    `json:"-"`
}

var (
	errInvalid     = errors.New("invalid input")
	errConflict    = errors.New("conflicting state")
	errNotFound    = errors.New("not found")
	errChanged     = errors.New("object changed since it was listed")
	scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
)

func invalid(message string) error  { return fmt.Errorf("%w: %s", errInvalid, message) }
func conflict(message string) error { return fmt.Errorf("%w: %s", errConflict, message) }

func normalizeEndpoint(raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return "", invalid("Enter an HTTP(S) S3 endpoint with no path, credentials, query or fragment.")
	}
	return endpoint.Scheme + "://" + strings.ToLower(endpoint.Host), nil
}

func prefix(input string) (string, error) {
	if len(input) > 1024 || strings.ContainsRune(input, 0) {
		return "", invalid("An object prefix must be at most 1024 bytes and contain no NUL.")
	}
	if input != "" && !strings.HasSuffix(input, "/") {
		input += "/"
	}
	if len(input) > 1024 {
		return "", invalid("The folder prefix is too long.")
	}
	return input, nil
}

func overlaps(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func nextRun(s Settings, after time.Time) (time.Time, error) {
	// Keep the timezone separate so there is one explicit timezone per task.
	if len(strings.Fields(s.Cron)) != 5 || strings.ContainsAny(s.Timezone, " \r\t\n=") ||
		s.Timezone == "" || s.Timezone == "Local" {
		return time.Time{}, invalid("Use a five-field Cron expression and an IANA timezone.")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return time.Time{}, invalid("Unknown IANA timezone.")
	}
	schedule, err := scheduleParser.Parse("CRON_TZ=" + s.Timezone + " " + s.Cron)
	if err != nil {
		return time.Time{}, invalid("Invalid five-field Cron expression.")
	}
	next := schedule.Next(after.UTC())
	if next.IsZero() {
		return time.Time{}, invalid("Cron has no upcoming occurrence.")
	}
	return next, nil
}

func (t *Task) validate(now time.Time) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" || len(t.Name) > 160 || t.SourceID < 1 {
		return invalid("Enter a task name and select a source bucket.")
	}
	s := &t.Settings
	var err error
	if s.SourcePrefix, err = prefix(s.SourcePrefix); err != nil {
		return err
	}
	if s.TargetPrefix, err = prefix(s.TargetPrefix); err != nil {
		return err
	}
	switch t.Kind {
	case "backup":
		if t.TargetID == nil || *t.TargetID < 1 {
			return invalid("Select a destination bucket.")
		}
		s.CleanupMode, s.Cutoff, s.RetainDays = "", nil, 0
	case "cleanup":
		if s.SourcePrefix == "" {
			return invalid("Cleanup requires a non-empty folder prefix.")
		}
		switch s.CleanupMode {
		case "all":
			s.Cutoff, s.RetainDays = nil, 0
		case "before":
			if s.Cutoff == nil || s.Cutoff.IsZero() {
				return invalid("Select a cutoff date and time.")
			}
			s.RetainDays = 0
		case "retain":
			if s.RetainDays < 1 || s.RetainDays > 36500 {
				return invalid("Retention must be between 1 and 36500 days.")
			}
			s.Cutoff = nil
		default:
			return invalid("Select a cleanup rule.")
		}
		t.TargetID, s.TargetPrefix, s.Move = nil, "", false
	case "index":
		t.TargetID = nil
		*s = Settings{Cron: s.Cron, Timezone: s.Timezone}
	default:
		return invalid("Unknown task type.")
	}
	if (s.Move || (t.Kind == "cleanup" && t.Enabled)) && !s.Quiescent {
		return invalid("Confirm that writers are paused during destructive runs.")
	}
	s.Cron = strings.TrimSpace(s.Cron)
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}
	t.NextRun = nil
	if t.Enabled || s.Cron != "" {
		next, err := nextRun(*s, now)
		if err != nil {
			return err
		}
		if t.Enabled {
			t.NextRun = &next
		}
	}
	return nil
}

func (s Settings) cutoff(now time.Time) *time.Time {
	switch s.CleanupMode {
	case "before":
		return s.Cutoff
	case "retain":
		value := now.UTC().AddDate(0, 0, -s.RetainDays)
		return &value
	default:
		return nil
	}
}
