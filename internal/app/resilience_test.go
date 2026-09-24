package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fixture "github.com/Timofey121/fx-quotes/test/fixture/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSixtyFourWorkersCapacityAndAllPairs(t *testing.T) {
	dburl := os.Getenv("TEST_DATABASE_URL")
	if dburl == "" {
		t.Skip("database required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dburl)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("audit64_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	provider := fixture.New(fixture.Config{})
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	defer release()
	started := make(chan struct{}, 128)
	provider.SetBlock(block, started)
	ps := httptest.NewServer(provider)
	defer ps.Close()
	cfg := e2eConfig(t, dburl, schema, ps.URL)
	cfg.WorkerCount = 64
	cfg.MaxActive = 64
	cfg.ProviderTimeout = 10 * time.Second
	cfg.LeaseDuration = 12 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.RequestTimeout = 5 * time.Second
	a, cancel, done := startE2EApplication(t, cfg)
	defer func() { cancel(); waitApplication(t, done) }()
	// Параллельные запросы к провайдеру не требуют SQL-соединения на каждый обработчик.
	if a.pool.Config().MaxConns > 16 {
		t.Fatalf("pool=%d", a.pool.Config().MaxConns)
	}
	base := "http://" + cfg.ListenAddress
	client := &http.Client{Timeout: 5 * time.Second}
	pairs := []string{"USD/EUR", "USD/MXN", "EUR/USD", "EUR/MXN", "MXN/USD", "MXN/EUR"}
	ids := make([]string, 64)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", base+"/v1/quote-updates", strings.NewReader(fmt.Sprintf(`{"pair":%q}`, pairs[i%6])))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", fmt.Sprintf("audit-%d", i))
			r, e := client.Do(req)
			if e != nil {
				t.Error(e)
				return
			}
			defer r.Body.Close()
			var body operation
			if e = json.NewDecoder(r.Body).Decode(&body); e != nil || r.StatusCode != 202 {
				t.Errorf("POST status=%d err=%v", r.StatusCode, e)
				return
			}
			ids[i] = body.ID
		}(i)
	}
	wg.Wait()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for n := 0; n < 64; n++ {
		select {
		case <-started:
		case <-timer.C:
			release()
			t.Fatalf("only %d provider requests active", n)
		}
	}
	t.Logf("64 provider requests simultaneously active; pool limit=%d", a.pool.Config().MaxConns)
	req, _ := http.NewRequest("POST", base+"/v1/quote-updates", strings.NewReader(`{"pair":"USD/EUR"}`))
	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req)
	if err != nil {
		release()
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 503 || r.Header.Get("Retry-After") != "1" {
		t.Errorf("capacity status=%d retry=%s", r.StatusCode, r.Header.Get("Retry-After"))
	}
	replay := postOperation(t, client, cfg.ListenAddress, "audit-0")
	if replay.ID != ids[0] || replay.StatusCode != 200 {
		t.Errorf("replay=%+v", replay)
	}
	release()
	for _, id := range ids {
		u := pollOperation(t, cfg.ListenAddress, id, 5*time.Second)
		if u.Status != "completed" {
			t.Errorf("status=%s", u.Status)
		}
	}
	for _, pair := range pairs {
		r, e := client.Get(base + "/v1/quotes/latest?pair=" + strings.ReplaceAll(pair, "/", "%2F"))
		if e != nil {
			t.Fatal(e)
		}
		var q map[string]any
		e = json.NewDecoder(r.Body).Decode(&q)
		r.Body.Close()
		if e != nil || r.StatusCode != 200 || q["rate"] != "0.9234" || q["pair"] != pair {
			t.Errorf("latest=%v status=%d err=%v", q, r.StatusCode, e)
		}
	}
	var count int
	if err = admin.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".quote_updates").Scan(&count); err != nil || count != 64 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	t.Log("64 durable completions, all 6 latest pairs, replay at capacity, overload 503 verified")
}

func TestShutdownBoundsBlockedAdmission(t *testing.T) {
	dburl := os.Getenv("TEST_DATABASE_URL")
	if dburl == "" {
		t.Skip("database required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dburl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("auditshutdown_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	ps := httptest.NewServer(fixture.New(fixture.Config{}))
	t.Cleanup(ps.Close)
	cfg := e2eConfig(t, dburl, schema, ps.URL)
	cfg.ShutdownTimeout = 100 * time.Millisecond
	cfg.WriteTimeout = 100 * time.Millisecond
	parsedURL, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	applicationName := "fxquotes-shutdown-" + schema
	query := parsedURL.Query()
	query.Set("application_name", applicationName)
	parsedURL.RawQuery = query.Encode()
	cfg.DatabaseURL = parsedURL.String()
	a, cancel, done := startE2EApplication(t, cfg)
	applicationDone := make(chan struct{})
	go func() {
		<-done
		close(applicationDone)
	}()
	t.Cleanup(func() {
		cancel()
		_ = a.server.Close()
		select {
		case <-applicationDone:
		case <-time.After(2 * time.Second):
			t.Error("cleanup did not stop application")
		}
	})
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	})
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x46585141)); err != nil {
		t.Fatal(err)
	}
	requestctx, stopRequest := context.WithCancel(ctx)
	t.Cleanup(stopRequest)
	req, _ := http.NewRequestWithContext(requestctx, "POST", "http://"+cfg.ListenAddress+"/v1/quote-updates", strings.NewReader(`{"pair":"USD/EUR"}`))
	req.Header.Set("Content-Type", "application/json")
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, e := http.DefaultClient.Do(req)
		if e == nil {
			r.Body.Close()
		}
	}()
	t.Cleanup(func() {
		stopRequest()
		select {
		case <-requestDone:
		case <-time.After(2 * time.Second):
			t.Error("cleanup did not stop blocked request")
		}
	})
	deadline := time.Now().Add(time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var count int
		err = admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory'", applicationName).Scan(&count)
		if err == nil && count > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	exited := false
	select {
	case <-applicationDone:
		exited = true
	case <-time.After(500 * time.Millisecond):
	}
	// Освобождаем тестовую блокировку даже при обнаруженном зависании.
	_ = tx.Rollback(ctx)
	_ = a.server.Close()
	if !exited {
		select {
		case <-applicationDone:
		case <-time.After(2 * time.Second):
			t.Fatal("cleanup did not stop application")
		}
	}
	stopRequest()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not stop blocked request")
	}
	if !blocked {
		t.Fatal("request did not reach admission lock")
	}
	if !exited {
		t.Fatal("application exceeded shutdown timeout 5x while HTTP request held a database connection")
	}
}
