package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSchemaRejectsIllegalStates(t *testing.T) {
	s, db := testStore(t, 1)
	u := createUpdate(t, s, "")
	for _, sql := range []string{
		"UPDATE quote_updates SET status='processing' WHERE id=$1",
		"UPDATE quote_updates SET status='completed' WHERE id=$1",
		"UPDATE quote_updates SET status='failed' WHERE id=$1",
		"UPDATE quote_updates SET lease_until=clock_timestamp() WHERE id=$1",
		"UPDATE quote_updates SET rate=0 WHERE id=$1",
		"UPDATE quote_updates SET failure_code='arbitrary details' WHERE id=$1",
	} {
		if _, err := db.Exec(context.Background(), sql, u.ID); err == nil {
			t.Errorf("accepted illegal row: %s", sql)
		}
	}
}

func TestMigrationsSerializeAndRollbackFailedFile(t *testing.T) {
	_, db := testStore(t, 1)
	ctx := context.Background()
	files := fstest.MapFS{
		"migrations/002_probe.sql": {Data: []byte("CREATE TABLE migration_probe (id INTEGER)")},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runMigrations(ctx, db, files); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	files["migrations/003_fault.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE rolled_back_probe (id INTEGER); SELECT 1 / 0")}
	if err := runMigrations(ctx, db, files); err == nil {
		t.Fatal("expected migration failure")
	}
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('rolled_back_probe') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration leaked table: %v %v", exists, err)
	}
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name='migrations/003_fault.sql')").Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration recorded as applied: %v %v", exists, err)
	}
	files["migrations/003_fault.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE rolled_back_probe (id INTEGER)")}
	if err := runMigrations(ctx, db, files); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
}

func TestSchemaBoundsIdempotencyKeyBytes(t *testing.T) {
	s, db := testStore(t, 1)
	u := createUpdate(t, s, strings.Repeat("é", 64))
	for _, key := range []string{strings.Repeat("A", 129), strings.Repeat("é", 64) + "a"} {
		_, err := db.Exec(context.Background(), "UPDATE quote_updates SET idempotency_key=$1 WHERE id=$2", key, u.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("129-byte key must violate CHECK constraint: %v", err)
		}
	}
}
