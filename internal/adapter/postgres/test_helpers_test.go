package postgres

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaSequence atomic.Int64

// У каждого теста своя схема; подключение TEST_DATABASE_URL должно разрешать CREATE SCHEMA.
func testStore(t *testing.T, capacity int) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("quotes_test_%d_%d", os.Getpid(), schemaSequence.Add(1))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	// Проверяем UTC, даже если у сеанса другой часовой пояс.
	config.ConnConfig.RuntimeParams["timezone"] = "Pacific/Honolulu"
	config.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	return NewStore(pool, capacity), pool
}

func mustPair(value string) entity.Pair {
	p, err := entity.ParsePair(value)
	if err != nil {
		panic(err)
	}
	return p
}

func createUpdate(t *testing.T, store *Store, key string) entity.QuoteUpdate {
	t.Helper()
	u, created, err := store.CreateOrGet(context.Background(), quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: key})
	if err != nil || !created {
		t.Fatalf("create = %#v, %v, %v", u, created, err)
	}
	return u
}

func claimUpdate(t *testing.T, store *Store) entity.QuoteUpdate {
	t.Helper()
	u, err := store.ClaimNext(context.Background(), quote.ClaimNextCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func fixtureQuote(rate, date string) entity.Quote {
	r, err := entity.ParseRate(rate)
	if err != nil {
		panic(err)
	}
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return entity.Quote{Pair: mustPair("USD/EUR"), Rate: r, SourceDate: d, Provider: "fixture", UpdatedAt: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func completeUpdate(t *testing.T, store *Store, u entity.QuoteUpdate, rate, date string) entity.QuoteUpdate {
	t.Helper()
	u, err := store.Complete(context.Background(), quote.CompleteCommand{UpdateID: u.ID, Attempt: u.Attempt, Quote: fixtureQuote(rate, date)})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
