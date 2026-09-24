package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate применяет встроенные миграции по порядку имён, без отката назад.
// Миграция и запись о ней фиксируются вместе; блокировка согласует запуск реплик.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return runMigrations(ctx, pool, migrations)
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := applyMigration(ctx, pool, files, name); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, files fs.FS, name string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x4658514d)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())"); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	var applied bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)", name).Scan(&applied); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if !applied {
		sql, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}
