package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql web/*
var assets embed.FS

type Config struct {
	DatabaseURL    string
	EncryptionKey  string
	AdminUser      string
	AdminPassword  string
	ViewerUser     string
	ViewerPassword string
	SecureCookie   bool
}

type Service struct {
	db        *pgxpool.Pool
	cipher    cipher.AEAD
	cfg       Config
	log       *slog.Logger
	transport *http.Transport
}

func Open(ctx context.Context, cfg Config, log *slog.Logger) (*Service, error) {
	if cfg.AdminUser == "" || len(cfg.AdminPassword) < 12 {
		return nil, errors.New("ADMIN_USER and ADMIN_PASSWORD (at least 12 bytes) are required")
	}
	if (cfg.ViewerUser != "" || cfg.ViewerPassword != "") &&
		(cfg.ViewerUser == "" || cfg.ViewerUser == cfg.AdminUser || len(cfg.ViewerPassword) < 12) {
		return nil, errors.New("VIEWER_USER must differ from ADMIN_USER and VIEWER_PASSWORD must be at least 12 bytes")
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	key, err := base64.StdEncoding.DecodeString(cfg.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("APP_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	dbConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, errors.New("DATABASE_URL is invalid")
	}
	// Describe parameters so pgx can encode typed JSONB and nullable values.
	dbConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	if dbConfig.MaxConns < 4 {
		return nil, errors.New("DATABASE_URL requires pool_max_conns of at least 4")
	}
	db, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 60 * time.Second
	s := &Service{db: db, cipher: aead, cfg: cfg, log: log, transport: transport}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	return s, nil
}

func (s *Service) Close() {
	s.transport.CloseIdleConnections()
	s.db.Close()
}

func (s *Service) migrate(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(71428721, hashtext(current_schema()))"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS s3backup_migrations (version integer PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM s3backup_migrations").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("database schema is newer than this application")
	}
	if version == 0 {
		script, err := assets.ReadFile("schema.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script), pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO s3backup_migrations VALUES (1)"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) seal(credentials Credentials, endpoint string) ([]byte, error) {
	data, err := json.Marshal(credentials)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.cipher.Seal(nonce, nonce, data, []byte(endpoint)), nil
}

func (s *Service) unseal(encrypted []byte, endpoint string) (Credentials, error) {
	var credentials Credentials
	n := s.cipher.NonceSize()
	if len(encrypted) < n {
		return credentials, errors.New("stored credentials are invalid")
	}
	data, err := s.cipher.Open(nil, encrypted[:n], encrypted[n:], []byte(endpoint))
	if err != nil {
		return credentials, errors.New("could not decrypt stored credentials")
	}
	return credentials, json.Unmarshal(data, &credentials)
}
