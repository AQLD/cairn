package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database written by an older engine is upgraded in place, keeping its data.
func TestMigratesOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// Build a version-1 database by hand, as the first engine release wrote it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0] + `PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO saves (id, workspace_id, kind, state, size, created_at) VALUES ('sv_old', 'ws_old', 'quick', 'ready', 42, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _ := st.Version(); v != SchemaVersion {
		t.Fatalf("schema version %d, want %d", v, SchemaVersion)
	}
	sv, err := st.GetSave(context.Background(), "sv_old")
	if err != nil {
		t.Fatal(err)
	}
	if sv.State != SaveReady || sv.Size != 42 || !sv.DeletedAt.IsZero() {
		t.Fatalf("save after migration: %+v", sv)
	}
	// the new column works
	if err := st.SoftDeleteSave(context.Background(), "sv_old", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Opening twice is a no-op the second time.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}

// A database from a newer engine is refused, never misread or downgraded.
func TestRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this engine") {
		t.Fatalf("want refusal, got %v", err)
	}
}
