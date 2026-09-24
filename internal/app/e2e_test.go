package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/app/config"
	fixture "github.com/Timofey121/fx-quotes/test/fixture/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var e2eSchemaSequence atomic.Int64

// Проверяем потерю ответа и перезапуск с настоящими HTTP-обработчиком,
// пулом фоновых обработчиков, PostgreSQL и адаптером провайдера.
func TestApplicationDurabilityAcrossResponseLossAndRestart(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("quotes_e2e_%d_%d", os.Getpid(), e2eSchemaSequence.Add(1))
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})

	provider := fixture.New(fixture.Config{Rate: "0.9234", SourceDate: "2026-09-22"})
	providerServer := httptest.NewServer(provider)
	t.Cleanup(providerServer.Close)
	config := e2eConfig(t, databaseURL, schema, providerServer.URL)

	block := make(chan struct{})
	started := make(chan struct{}, 1)
	provider.SetBlock(block, started)
	first, firstCancel, firstDone := startE2EApplication(t, config)
	_ = first
	createdAt := time.Now()
	operation := postOperation(t, http.DefaultClient, config.ListenAddress, "restart-key")
	if elapsed := time.Since(createdAt); elapsed > 250*time.Millisecond {
		t.Fatalf("POST waited for blocked provider: %s", elapsed)
	}
	if operation.Status != "pending" {
		t.Fatalf("new operation = %#v", operation)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start the blocked provider request")
	}
	firstCancel()
	waitApplication(t, firstDone)
	close(block)
	provider.SetBlock(nil, nil)

	_, secondCancel, secondDone := startE2EApplication(t, config)
	t.Cleanup(func() {
		secondCancel()
		waitApplication(t, secondDone)
	})
	completed := pollOperation(t, config.ListenAddress, operation.ID, 5*time.Second)
	if completed.Status != "completed" || completed.Quote == nil || completed.Quote.Rate != "0.9234" {
		t.Fatalf("recovered operation = %#v", completed)
	}

	key := "response-lost-key"
	request, err := http.NewRequest(http.MethodPost, "http://"+config.ListenAddress+"/v1/quote-updates", bytes.NewBufferString(`{"pair":"USD/EUR"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	_, err = (&http.Client{Transport: responseLostTransport{base: http.DefaultTransport}}).Do(request)
	if !errors.Is(err, errResponseLost) {
		t.Fatalf("lost response request = %v, want %v", err, errResponseLost)
	}
	persistedID := operationIDByKey(t, admin, schema, key)
	replay := postOperation(t, http.DefaultClient, config.ListenAddress, key)
	if replay.StatusCode != http.StatusOK || replay.ID != persistedID {
		t.Fatalf("idempotent retry = %#v, persisted id=%s", replay, persistedID)
	}

	provider.SetStatus(http.StatusBadRequest)
	failed := postOperation(t, http.DefaultClient, config.ListenAddress, "permanent-failure-key")
	failed = pollOperation(t, config.ListenAddress, failed.ID, 3*time.Second)
	if failed.Status != "failed" || failed.FailureCode != "provider_permanent" {
		t.Fatalf("permanent provider failure = %#v", failed)
	}
	latest := getLatest(t, config.ListenAddress)
	if latest.Rate != "0.9234" {
		t.Fatalf("failure erased prior latest quote: %#v", latest)
	}
}

func e2eConfig(t *testing.T, databaseURL, schema, providerURL string) config.Config {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	address := freeAddress(t)
	return config.Config{
		ListenAddress: address, DatabaseURL: parsed.String(), ProviderURL: providerURL,
		ProviderTimeout: 250 * time.Millisecond, ReadTimeout: time.Second, WriteTimeout: time.Second, RequestTimeout: time.Second,
		WorkerCount: 1, PollInterval: 10 * time.Millisecond, RecoveryInterval: 20 * time.Millisecond,
		WorkerErrorDelay: 10 * time.Millisecond, LeaseDuration: 1300 * time.Millisecond, MaxAttempts: 2,
		MaxActive: 20, LogLevel: "error", ShutdownTimeout: time.Second,
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func startE2EApplication(t *testing.T, configuration config.Config) (*Application, context.CancelFunc, <-chan error) {
	t.Helper()
	application, err := Build(context.Background(), configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + configuration.ListenAddress + "/health/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return application, cancel, done
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	waitApplication(t, done)
	t.Fatal("application did not become ready")
	return nil, nil, nil
}

func waitApplication(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("application did not stop")
	}
}

type operation struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Quote       *responseQuote `json:"quote"`
	FailureCode string         `json:"failure_code"`
	StatusCode  int
}
type responseQuote struct {
	Rate string `json:"rate"`
}

func postOperation(t *testing.T, client *http.Client, address, key string) operation {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/quote-updates", bytes.NewBufferString(`{"pair":"USD/EUR"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result operation
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	result.StatusCode = response.StatusCode
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		t.Fatalf("POST status=%d body=%#v", response.StatusCode, result)
	}
	return result
}

func pollOperation(t *testing.T, address, id string, timeout time.Duration) operation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + address + "/v1/quote-updates/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var result operation
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == "completed" || result.Status == "failed" {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s did not finish", id)
	return operation{}
}

func operationIDByKey(t *testing.T, admin *pgxpool.Pool, schema, key string) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := admin.QueryRow(context.Background(), "SELECT id::text FROM "+pgx.Identifier{schema}.Sanitize()+".quote_updates WHERE idempotency_key=$1", key).Scan(&id)
		if err == nil {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("response-lost request was not admitted")
	return ""
}

func getLatest(t *testing.T, address string) responseQuote {
	t.Helper()
	response, err := http.Get("http://" + address + "/v1/quotes/latest?pair=USD%2FEUR")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("latest status=%d", response.StatusCode)
	}
	var result responseQuote
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

var errResponseLost = errors.New("response lost after server accepted request")

type responseLostTransport struct{ base http.RoundTripper }

func (transport responseLostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	return nil, errResponseLost
}
