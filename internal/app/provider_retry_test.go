package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplicationRetriesTruncatedProviderResponse(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("retry_e2e_%d_%d", os.Getpid(), e2eSchemaSequence.Add(1))
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "1000")
		}
		fmt.Fprint(w, `{"date":"2026-09-22","base":"USD","quote":"EUR","rate":0.9234}`)
	}))
	defer provider.Close()
	cfg := e2eConfig(t, databaseURL, schema, provider.URL)
	_, cancel, done := startE2EApplication(t, cfg)
	defer func() { cancel(); waitApplication(t, done) }()
	created := postOperation(t, http.DefaultClient, cfg.ListenAddress, "truncated-then-healthy")
	completed := pollOperation(t, cfg.ListenAddress, created.ID, 4*time.Second)
	if completed.Status != "completed" || completed.Quote == nil || completed.Quote.Rate != "0.9234" || calls.Load() != 2 {
		t.Fatalf("transient response was not retried successfully: %+v, calls=%d", completed, calls.Load())
	}
}
