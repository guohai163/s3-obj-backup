package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/guohai163/s3-obj-backup/internal/backup"
	"github.com/joho/godotenv"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		endpoint := "http://127.0.0.1:8080/healthz"
		if len(os.Args) > 2 {
			endpoint = os.Args[2]
		}
		response, err := client.Get(endpoint)
		if err != nil {
			os.Exit(1)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not load .env")
	}
	secure := false
	if raw := os.Getenv("COOKIE_SECURE"); raw != "" {
		var err error
		secure, err = strconv.ParseBool(raw)
		if err != nil {
			return errors.New("COOKIE_SECURE must be true or false")
		}
	}
	cfg := backup.Config{
		DatabaseURL: os.Getenv("DATABASE_URL"), EncryptionKey: os.Getenv("APP_ENCRYPTION_KEY"),
		AdminUser: os.Getenv("ADMIN_USER"), AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		ViewerUser: os.Getenv("VIEWER_USER"), ViewerPassword: os.Getenv("VIEWER_PASSWORD"), SecureCookie: secure,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	service, err := backup.Open(startup, cfg, slog.Default())
	cancel()
	if err != nil {
		return err
	}
	defer service.Close()

	done := make(chan struct{})
	go func() { defer close(done); service.Work(ctx) }()
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr: addr, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second,
	}
	failure := make(chan error, 1)
	go func() { failure <- server.ListenAndServe() }()
	slog.Info("web server ready", "address", addr)
	select {
	case <-ctx.Done():
	case err := <-failure:
		stop()
		<-done
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	<-done
	return err
}
