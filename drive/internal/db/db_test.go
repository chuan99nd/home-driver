package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "drive.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	var fk, busy int
	d.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	d.QueryRow(`PRAGMA busy_timeout`).Scan(&busy)
	if mode != "wal" || fk != 1 || busy < 1000 {
		t.Errorf("pragmas: journal_mode=%s foreign_keys=%d busy_timeout=%d", mode, fk, busy)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if v != len(migs) {
		t.Errorf("schema version %d, want %d", v, len(migs))
	}
	// Foreign keys are really enforced.
	if _, err := d.Exec(`INSERT INTO nodes(id, owner_id, name, is_dir, created_at, updated_at) VALUES ('x', 999, 'x', 1, 0, 0)`); err == nil {
		t.Error("foreign key violation accepted")
	}
	d.Close()

	// Re-opening applies nothing twice.
	d, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if n != len(migs) {
		t.Errorf("migrations recorded %d times", n)
	}
	// A database from a newer binary is refused.
	if _, err := d.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (9999, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Error("newer schema accepted")
	}
}
