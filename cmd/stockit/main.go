package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"stockit/internal/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := flag.String("db", filepath.Join("data", "stockit.db"), "SQLite database path")
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve working directory: %v", err)
	}
	resolvedDBPath, err := filepath.Abs(*dbPath)
	if err != nil {
		log.Fatalf("resolve db path: %v", err)
	}
	if err := configureSQLiteTempDir(resolvedDBPath); err != nil {
		log.Fatalf("configure sqlite temp dir: %v", err)
	}

	log.Printf("StockIt starting addr=%s db=%s resolved_db=%s cwd=%s", *addr, *dbPath, resolvedDBPath, cwd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := app.New(ctx, app.Config{
		Addr:   *addr,
		DBPath: *dbPath,
	})
	if err != nil {
		log.Fatalf("start app: %v", err)
	}
	defer func() {
		if err := server.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	log.Printf(
		"StockIt running addr=%s db=%s tmpdir=%s sqlite_tmpdir=%s",
		*addr,
		resolvedDBPath,
		os.Getenv("TMPDIR"),
		os.Getenv("SQLITE_TMPDIR"),
	)
	if err := server.Run(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("StockIt stopped")
}

func configureSQLiteTempDir(resolvedDBPath string) error {
	dbDir := filepath.Dir(resolvedDBPath)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return err
	}
	if err := os.Setenv("TMPDIR", dbDir); err != nil {
		return err
	}
	if err := os.Setenv("SQLITE_TMPDIR", dbDir); err != nil {
		return err
	}
	return nil
}
