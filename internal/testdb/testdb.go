// Package testdb gives tests a freshly migrated PostgreSQL database.
//
// Tests that need it are skipped unless PAAS_TEST_DATABASE_URL is set,
// so `go test ./...` still works on a machine without Postgres.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

func Open(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("PAAS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PAAS_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()

	raw, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	raw.Close()

	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
