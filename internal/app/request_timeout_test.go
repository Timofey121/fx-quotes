package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/test/fixture/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRequestTimeoutReleasesAdmissionConnection(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("quotes_timeout_%d_%d", os.Getpid(), e2eSchemaSequence.Add(1))
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})

	providerServer := httptest.NewServer(provider.New(provider.Config{}))
	t.Cleanup(providerServer.Close)
	configuration := e2eConfig(t, databaseURL, schema, providerServer.URL)
	configuration.RequestTimeout = 250 * time.Millisecond
	configuration.WriteTimeout = time.Second
	application, cancel, done := startE2EApplication(t, configuration)
	t.Cleanup(func() {
		cancel()
		waitApplication(t, done)
	})

	lock, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	})
	if _, err = lock.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x46585141)); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, "http://"+configuration.ListenAddress+"/v1/quote-updates", strings.NewReader(`{"pair":"USD/EUR"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "request-timeout")
	startedAt := time.Now()
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("timed-out admission status=%d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("admission returned after %s, want request timeout", elapsed)
	}

	deadline := time.Now().Add(time.Second)
	acquired := application.pool.Stat().AcquiredConns()
	for acquired != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		acquired = application.pool.Stat().AcquiredConns()
	}
	// Проверяем тот же снимок: между чтениями соединение может занять worker.
	if acquired != 0 {
		t.Fatalf("timed-out admission kept %d database connections while advisory lock remains held", acquired)
	}
	if err = lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	operation := postOperation(t, http.DefaultClient, configuration.ListenAddress, "request-timeout-retry")
	if operation.StatusCode != http.StatusAccepted {
		t.Fatalf("admission after lock release = %#v", operation)
	}
}
