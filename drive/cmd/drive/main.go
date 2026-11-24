// Command drive is the home-driver Drive server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/config"
	"github.com/chuan99nd/home-driver/drive/internal/db"
	"github.com/chuan99nd/home-driver/drive/internal/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("fatal", "err", err.Error())
		fmt.Fprintln(os.Stderr, "drive:", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "backup-db":
			if len(args) != 2 {
				return errors.New("usage: drive backup-db <destination-file>")
			}
			return backupDB(args[1])
		case "serve":
		default:
			return fmt.Errorf("unknown command %q (commands: serve, backup-db <dest>)", args[0])
		}
	}

	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	if cfg.DevLogin {
		log.Warn("!!! DRIVE_DEV_LOGIN=1: ANYONE WHO CAN REACH THIS PORT CAN SIGN IN AS ANY ALLOWED USER WITHOUT A PASSWORD. NEVER USE THIS IN PRODUCTION. !!!")
	}
	if !config.IsLoopbackAddr(cfg.Addr) {
		log.Warn("DRIVE_ADDR is not a loopback address; home-driver services are meant to listen on 127.0.0.1 only", "addr", cfg.Addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()

	srv, err := server.New(cfg, database, log)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No ReadTimeout/WriteTimeout: uploads and downloads of large files
		// legitimately take a long time. Handlers set their own read
		// deadlines for JSON bodies and chunks.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		srv.RunJobs(ctx)
	}()

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	log.Info("drive listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "data_dir", cfg.DataDir,
		"db", cfg.DBPath, "curator", cfg.CuratorURL, "trash_days", cfg.TrashDays, "admins", len(cfg.AdminEmails))

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown: closing remaining connections", "err", err)
		httpSrv.Close()
	}
	<-jobsDone
	return nil
}

// backupDB writes a consistent copy of the database with VACUUM INTO. It is
// safe while the server is running.
func backupDB(dest string) error {
	path := os.Getenv("DRIVE_DB_PATH")
	if path == "" {
		path = "/var/lib/home-driver/drive/drive.db"
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("database %s: %w", path, err)
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination %s already exists; refusing to overwrite", dest)
	}
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()
	if _, err := d.Exec(`VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("backup to %s: %w", dest, err)
	}
	fmt.Println("backup written to", dest)
	return nil
}
