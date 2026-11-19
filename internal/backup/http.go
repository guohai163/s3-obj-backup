package backup

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const cookieName = "s3_backup_session"

type endpoint func(http.ResponseWriter, *http.Request) error

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	add := func(pattern string, access string, fn endpoint) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || r.Header.Get("Sec-Fetch-Site") == "same-site" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Cross-origin requests are disabled."})
				return
			}
			if access != "" {
				role := s.role(r)
				if role == "" {
					respond(w, 401, map[string]string{"error": "Sign in to continue."})
					return
				}
				if access == "admin" && role != "admin" {
					respond(w, 403, map[string]string{"error": "Administrator access required."})
					return
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			if err := fn(w, r.WithContext(ctx)); err != nil {
				s.httpError(w, err)
			}
		})
	}
	static, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.db.Ping(ctx) != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Write([]byte("ok"))
	})
	add("POST /api/login", "", s.loginHandler())
	add("POST /api/logout", "viewer", s.logout)
	add("GET /api/session", "viewer", func(w http.ResponseWriter, r *http.Request) error {
		respond(w, 200, map[string]string{"role": s.role(r)})
		return nil
	})
	add("GET /api/config", "viewer", s.configuration)
	add("POST /api/connections", "admin", s.connectionHandler)
	add("GET /api/connections/{id}/buckets", "admin", s.discover)
	add("POST /api/buckets", "admin", s.bucketHandler)
	add("POST /api/tasks", "admin", s.taskHandler)
	add("POST /api/tasks/{id}/run", "admin", s.runHandler)
	add("GET /api/runs", "viewer", func(w http.ResponseWriter, r *http.Request) error {
		result, err := s.runs(r.Context(), queryID(r, "before"))
		if err == nil {
			respond(w, 200, result)
		}
		return err
	})
	add("GET /api/runs/{id}/items", "viewer", func(w http.ResponseWriter, r *http.Request) error {
		id, err := routeID(r)
		if err != nil {
			return err
		}
		objects, err := s.previewItems(r.Context(), id, r.URL.Query().Get("after"))
		if err == nil {
			page(w, objects)
		}
		return err
	})
	add("GET /api/objects", "viewer", func(w http.ResponseWriter, r *http.Request) error {
		query := r.URL.Query()
		p, err := prefix(query.Get("prefix"))
		if err != nil {
			return err
		}
		objects, err := s.inventory(r.Context(), queryID(r, "bucket"), p, query.Get("q"), query.Get("after"),
			queryID(r, "after_bucket"), query.Get("browse") == "1")
		if err == nil {
			page(w, objects)
		}
		return err
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		return invalid("Expected a JSON request.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return invalid("Invalid JSON request.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("Invalid JSON request.")
	}
	return nil
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Service) httpError(w http.ResponseWriter, err error) {
	status, message := 500, "Request failed. Check the server logs."
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, errInvalid):
		status, message = 400, strings.TrimPrefix(err.Error(), "invalid input: ")
	case errors.Is(err, errConflict):
		status, message = 409, strings.TrimPrefix(err.Error(), "conflicting state: ")
	case errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errNotFound):
		status, message = 404, "Record not found."
	case errors.As(err, &pg) && pg.Code == "23505":
		status, message = 409, "A record with this name already exists."
	case errors.As(err, &pg) && pg.Code == "23503":
		status, message = 400, "A referenced record does not exist."
	default:
		s.log.Error("web request failed", "error", err)
	}
	respond(w, status, map[string]string{"error": message})
}

func page(w http.ResponseWriter, objects []Object) {
	more := len(objects) > 100
	if more {
		objects = objects[:100]
	}
	respond(w, 200, map[string]any{"items": objects, "more": more})
}

func routeID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errNotFound
	}
	return id, nil
}

func queryID(r *http.Request, key string) int64 {
	id, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return id
}

func (s *Service) configuration(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, "SELECT id,name,endpoint,region,path_style FROM connections ORDER BY name")
	connections, err := collect(rows, err, scanConnection)
	if err != nil {
		return err
	}
	rows, err = s.db.Query(ctx, "SELECT id,connection_id,name,indexed_at,object_count FROM buckets ORDER BY name,id")
	buckets, err := collect(rows, err, scanBucket)
	if err != nil {
		return err
	}
	rows, err = s.db.Query(ctx, "SELECT "+taskColumns+" FROM tasks ORDER BY id")
	tasks, err := collect(rows, err, scanTask)
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"connections": connections, "buckets": buckets, "tasks": tasks})
	return nil
}

func (s *Service) connectionHandler(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Connection
		Credentials
	}
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := s.saveConnection(r.Context(), input.Connection, input.Credentials)
	if err == nil {
		respond(w, 200, result)
	}
	return err
}

func (s *Service) discover(w http.ResponseWriter, r *http.Request) error {
	id, err := routeID(r)
	if err != nil {
		return err
	}
	client, err := s.client(r.Context(), id)
	if err != nil {
		return err
	}
	buckets, err := client.ListBuckets(r.Context())
	if err != nil {
		return invalid("Bucket discovery failed. Check credentials or add a bucket by name.")
	}
	names := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		names = append(names, bucket.Name)
	}
	respond(w, 200, names)
	return nil
}

func (s *Service) bucketHandler(w http.ResponseWriter, r *http.Request) error {
	var b Bucket
	if err := decode(w, r, &b); err != nil {
		return err
	}
	result, err := s.addBucket(r.Context(), b)
	if err == nil {
		respond(w, 200, result)
	}
	return err
}

func (s *Service) taskHandler(w http.ResponseWriter, r *http.Request) error {
	var t Task
	if err := decode(w, r, &t); err != nil {
		return err
	}
	result, err := s.saveTask(r.Context(), t)
	if err == nil {
		respond(w, 200, result)
	}
	return err
}

func (s *Service) runHandler(w http.ResponseWriter, r *http.Request) error {
	id, err := routeID(r)
	if err != nil {
		return err
	}
	var input struct {
		Mode    string `json:"mode"`
		PlanID  *int64 `json:"plan_id"`
		Confirm bool   `json:"confirm"`
	}
	if err := decode(w, r, &input); err != nil {
		return err
	}
	if input.Mode != "manual" && input.Mode != "preview" {
		return invalid("Invalid run mode.")
	}
	run, err := s.enqueue(r.Context(), id, input.Mode, input.PlanID, input.Confirm)
	if err == nil {
		respond(w, 202, map[string]int64{"id": run})
	}
	return err
}

func (s *Service) tokenHash(token string) []byte {
	mac := hmac.New(sha256.New, []byte(s.cfg.EncryptionKey))
	// Changing credentials revokes existing sessions.
	for _, value := range []string{s.cfg.AdminUser, s.cfg.AdminPassword, s.cfg.ViewerUser, s.cfg.ViewerPassword, token} {
		mac.Write([]byte(strconv.Itoa(len(value)) + ":"))
		mac.Write([]byte(value))
	}
	return mac.Sum(nil)
}

func (s *Service) role(r *http.Request) string {
	cookie, err := r.Cookie(cookieName)
	if err != nil || len(cookie.Value) != 43 {
		return ""
	}
	var role string
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	s.db.QueryRow(ctx, "SELECT role FROM sessions WHERE token_hash=$1 AND expires_at>now()", s.tokenHash(cookie.Value)).Scan(&role)
	return role
}

func (s *Service) cookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

type loginWindow struct {
	until    time.Time
	attempts int
}

func (s *Service) loginHandler() endpoint {
	var guard sync.Mutex
	windows := map[string]loginWindow{}
	return func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := decode(w, r, &input); err != nil {
			return err
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		now := time.Now()
		guard.Lock()
		if len(windows) >= 1024 {
			for ip, v := range windows {
				if !v.until.After(now) {
					delete(windows, ip)
				}
			}
		}
		window := windows[host]
		if !window.until.After(now) {
			window = loginWindow{until: now.Add(time.Minute)}
		}
		window.attempts++
		limited := window.attempts > 12 || (len(windows) >= 1024 && windows[host].until.IsZero())
		if !limited {
			windows[host] = window
		}
		guard.Unlock()
		if limited {
			respond(w, 429, map[string]string{"error": "Too many attempts. Try again in a minute."})
			return nil
		}

		role := ""
		if equal(input.User, s.cfg.AdminUser)&equal(input.Password, s.cfg.AdminPassword) == 1 {
			role = "admin"
		}
		if equal(input.User, s.cfg.ViewerUser)&equal(input.Password, s.cfg.ViewerPassword) == 1 && s.cfg.ViewerUser != "" {
			role = "viewer"
		}
		if role == "" {
			respond(w, 401, map[string]string{"error": "Invalid username or password."})
			return nil
		}
		guard.Lock()
		delete(windows, host)
		guard.Unlock()
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		if _, err := s.db.Exec(r.Context(), "INSERT INTO sessions VALUES($1,$2,$3)", s.tokenHash(token), role, now.Add(12*time.Hour)); err != nil {
			return err
		}
		s.cookie(w, token, 12*3600)
		respond(w, 200, map[string]string{"role": role})
		return nil
	}
}

func equal(a, b string) int {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:])
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	cookie, err := r.Cookie(cookieName)
	if err == nil {
		if _, err := s.db.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", s.tokenHash(cookie.Value)); err != nil {
			return err
		}
	}
	s.cookie(w, "", -1)
	respond(w, 200, map[string]bool{"ok": true})
	return nil
}
