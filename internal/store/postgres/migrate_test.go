package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/migrations"
)

func TestLoadMigrations(t *testing.T) {
	loaded, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no migrations found")
	}

	for i, m := range loaded {
		if m.Up == "" {
			t.Errorf("migration %d has no up statement", m.Version)
		}
		if m.Down == "" {
			t.Errorf("migration %d has no down statement; a schema change that cannot be rolled back is a deployment nobody can undo", m.Version)
		}
		if i > 0 && loaded[i-1].Version >= m.Version {
			t.Errorf("migrations are not in ascending version order: %d then %d", loaded[i-1].Version, m.Version)
		}
	}
}

// Migrate must be safe to run on every start-up: a process restarting is not a
// reason to reapply anything.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES_MIGRATE")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES_MIGRATE not set")
	}

	ctx := context.Background()

	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Start from an empty schema. The first assertion below is that a fresh
	// database receives every migration, which is only true once — without this
	// the test passes on a new database and fails on every later run, which is
	// the same as not being runnable.
	//
	// Dropping is safe here and only here: NIT_TEST_POSTGRES_MIGRATE names a
	// database dedicated to this test, separate from NIT_TEST_POSTGRES, exactly
	// so that it can be emptied.
	if _, err := s.Pool().Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	loaded, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}

	first, err := postgres.Migrate(ctx, s.Pool(), loaded)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if first != len(loaded) {
		t.Errorf("applied %d migrations, want %d", first, len(loaded))
	}

	second, err := postgres.Migrate(ctx, s.Pool(), loaded)
	if err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
	if second != 0 {
		t.Errorf("reapplied %d migrations on a second run, want 0", second)
	}
}
