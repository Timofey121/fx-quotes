//go:build unix

package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	fixture "github.com/Timofey121/fx-quotes/test/fixture/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplicationRecoversAfterSIGKILL(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	binary := filepath.Join(t.TempDir(), "fx-quotes")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/fx-quotes")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build application: %v\n%s", err, output)
	}
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("quotes_process_%d_%d", os.Getpid(), e2eSchemaSequence.Add(1))
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})

	provider := fixture.New(fixture.Config{Rate: "0.9234"})
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	configuration := e2eConfig(t, databaseURL, schema, server.URL)
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	provider.SetBlock(block, started)
	client := &http.Client{Timeout: time.Second}
	environment := processEnvironment(configuration.DatabaseURL, server.URL, configuration.ListenAddress)
	first := startApplicationProcess(t, ctx, binary, environment, configuration.ListenAddress)
	created := postOperation(t, client, configuration.ListenAddress, "crash-key")
	if created.StatusCode != http.StatusAccepted {
		t.Fatalf("new operation status = %d", created.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach the provider")
	}
	if err := first.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.wait(t, syscall.SIGKILL)

	var status string
	var attempt, latestCount int
	if err := admin.QueryRow(ctx, "SELECT status,attempt FROM "+quotedSchema+".quote_updates WHERE id=$1", created.ID).Scan(&status, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "processing" || attempt != 1 {
		t.Fatalf("state after SIGKILL = %s, attempt %d", status, attempt)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".latest_quotes").Scan(&latestCount); err != nil || latestCount != 0 {
		t.Fatalf("latest before recovery = %d, error %v", latestCount, err)
	}

	provider.SetBlock(nil, nil)
	close(block)
	second := startApplicationProcess(t, ctx, binary, environment, configuration.ListenAddress)
	replay := postOperation(t, client, configuration.ListenAddress, "crash-key")
	if replay.StatusCode != http.StatusOK || replay.ID != created.ID {
		t.Fatalf("replay after crash = %#v, original id=%s", replay, created.ID)
	}
	completed := pollOperation(t, configuration.ListenAddress, created.ID, 10*time.Second)
	if completed.Status != "completed" || completed.Quote == nil || completed.Quote.Rate != "0.9234" {
		t.Fatalf("recovered operation = %#v", completed)
	}
	if latest := getLatest(t, configuration.ListenAddress); latest.Rate != "0.9234" {
		t.Fatalf("latest after recovery = %#v", latest)
	}
	var operations int
	if err := admin.QueryRow(ctx, "SELECT count(*),max(attempt) FROM "+quotedSchema+".quote_updates WHERE idempotency_key='crash-key'").Scan(&operations, &attempt); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || attempt != 2 {
		t.Fatalf("after recovery: operations=%d, attempt=%d", operations, attempt)
	}
	if err := second.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	second.wait(t, 0)
}

type applicationProcess struct {
	command *exec.Cmd
	output  bytes.Buffer
	waited  bool
}

func startApplicationProcess(t *testing.T, ctx context.Context, binary string, environment []string, address string) *applicationProcess {
	t.Helper()
	process := &applicationProcess{command: exec.CommandContext(ctx, binary)}
	process.command.Env = environment
	process.command.Stdout, process.command.Stderr = &process.output, &process.output
	process.command.WaitDelay = time.Second
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !process.waited {
			_ = process.command.Process.Kill()
			_ = process.command.Wait()
		}
		if t.Failed() {
			t.Logf("application output:\n%s", process.output.String())
		}
	})
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/health/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("application process did not become ready")
	return nil
}

func (process *applicationProcess) wait(t *testing.T, signal syscall.Signal) {
	t.Helper()
	timeout := time.AfterFunc(5*time.Second, func() { _ = process.command.Process.Kill() })
	defer timeout.Stop()
	err := process.command.Wait()
	process.waited = true
	if signal == 0 {
		if err != nil {
			t.Fatalf("application exit: %v", err)
		}
		return
	}
	status, ok := process.command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != signal {
		t.Fatalf("application exit = %v, want signal %v", err, signal)
	}
}

func processEnvironment(databaseURL, providerURL, address string) []string {
	var environment []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FX_QUOTES_") {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		"FX_QUOTES_DATABASE_URL="+databaseURL,
		"FX_QUOTES_PROVIDER_URL="+providerURL,
		"FX_QUOTES_LISTEN_ADDR="+address,
		"FX_QUOTES_WORKER_COUNT=1",
		"FX_QUOTES_PROVIDER_TIMEOUT=5s",
		"FX_QUOTES_LEASE_DURATION=6s",
		"FX_QUOTES_POLL_INTERVAL=10ms",
		"FX_QUOTES_RECOVERY_INTERVAL=100ms",
		"FX_QUOTES_MAX_ATTEMPTS=2",
		"FX_QUOTES_SHUTDOWN_TIMEOUT=1s",
		"FX_QUOTES_LOG_LEVEL=error",
	)
}
