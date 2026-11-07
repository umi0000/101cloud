package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/install"
	"101cloud/backend/internal/platform"
	"101cloud/backend/internal/server"
	migrations "101cloud/backend/sql"
)

func main() {
	if e := run(); e != nil {
		slog.Error("backend stopped", "error", e.Error())
		os.Exit(1)
	}
}
func run() error {
	command, configPath, err := commandConfig(os.Args[1:])
	if err != nil {
		return err
	}
	if configPath != "" {
		if err := loadConfigFile(configPath); err != nil {
			return err
		}
	}
	cfg, e := platform.LoadConfig()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "install" {
		if e := install.Run(ctx, cfg, os.Getenv("CLOUD_MYSQL_ADMIN_DSN"), os.Getenv("CLOUD_MYSQL_APP_HOST")); e != nil {
			return e
		}
		slog.Info("database tables and restricted business MySQL account initialized")
		return nil
	}
	dbConfig, e := databaseConfig(command, cfg, os.Getenv("CLOUD_MYSQL_ADMIN_DSN"))
	if e != nil {
		return e
	}
	db, e := platform.OpenDB(dbConfig)
	if e != nil {
		return e
	}
	sqlDB, e := db.DB()
	if e != nil {
		return e
	}
	defer sqlDB.Close()
	a := platform.New(db, cfg)
	switch command {
	case "migrate":
		if e = migrations.Up(ctx, db); e != nil {
			return e
		}
		return billing.NormalizeDailyBuckets(ctx, db)
	case "serve":
		if e = migrations.Ready(ctx, db); e != nil {
			return e
		}
		// Complete the financial cutover before any HTTP request or worker can
		// observe legacy daily buckets with overlapping validity.
		if e = billing.NormalizeDailyBuckets(ctx, db); e != nil {
			return e
		}
		initial, err := auth.EnsureAdministrator(ctx, a)
		if err != nil {
			return err
		}
		if initial != nil {
			slog.Warn("initial administrator created; save these credentials and change the password after login; restrict access to this log", "username", initial.Username, "password", initial.Password)
			initial.Password = ""
		}
		if !cfg.HTTPS() {
			slog.Warn("CLOUD_ORIGIN uses plain http: passwords, session and CSRF cookies travel unencrypted and cookies are not Secure; use only for local development", "origin", cfg.Origin)
		}
		router := server.Router(a)
		httpServer := &http.Server{Addr: cfg.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
		workerDone := make(chan struct{})
		go func() { defer close(workerDone); a.Run(ctx) }()
		errorsCh := make(chan error, 1)
		go func() { errorsCh <- httpServer.ListenAndServe() }()
		select {
		case e = <-errorsCh:
			cancel()
		case <-ctx.Done():
		}
		shutdownCtx, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		select {
		case <-workerDone:
		case <-shutdownCtx.Done():
			return shutdownCtx.Err()
		}
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			return e
		}
		return shutdownErr
	default:
		return errors.New("unknown command")
	}
}
