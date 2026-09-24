package deployment_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireSmokeTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is required for smoke contract tests")
		}
	}
}

func runSmoke(t *testing.T, url, timeout string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "scripts/smoke.sh")
	cmd.Dir = moduleRoot(t)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "FX_QUOTES_SMOKE_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "FX_QUOTES_SMOKE_URL="+url, "FX_QUOTES_SMOKE_EXPECTED_RATE=0.9234", "FX_QUOTES_SMOKE_TIMEOUT_SECONDS="+timeout)
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

func smokeFixture(stall string) http.Handler {
	var mu sync.Mutex
	seen := make(map[string]bool)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stage := "poll"
		if r.Method == http.MethodPost {
			mu.Lock()
			key := r.Header.Get("Idempotency-Key")
			stage = "create"
			if seen[key] {
				stage = "replay"
			}
			seen[key] = true
			mu.Unlock()
		} else if strings.HasSuffix(r.URL.Path, "/latest") {
			stage = "latest"
		}
		if stage == stall {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if stage == "create" {
			w.Header().Set("Location", "/v1/quote-updates/test-id")
			w.WriteHeader(http.StatusAccepted)
		}
		fmt.Fprint(w, `{"id":"test-id","status":"completed","quote":{"rate":"0.9234"},"rate":"0.9234"}`)
	})
}

func TestSmokeBoundsEveryNetworkStage(t *testing.T) {
	requireSmokeTools(t)
	for _, stage := range []string{"create", "poll", "latest", "replay"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(smokeFixture(stage))
			defer server.Close()
			started := time.Now()
			output, err := runSmoke(t, server.URL, "1")
			if err == nil {
				t.Errorf("hanging %s unexpectedly succeeded: %s", stage, output)
			}
			if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
				t.Errorf("%s exceeded 1s budget: %s; %s", stage, elapsed, output)
			}
		})
	}
}

func TestSmokeConcurrentRunsHaveDistinctKeys(t *testing.T) {
	requireSmokeTools(t)
	server := httptest.NewServer(smokeFixture(""))
	defer server.Close()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if output, err := runSmoke(t, server.URL, "5"); err != nil {
				t.Errorf("smoke failed: %v; %s", err, output)
			}
		}()
	}
	wg.Wait()
}

func TestSmokeUsesOneBudgetForTheWholeScenario(t *testing.T) {
	requireSmokeTools(t)
	fixture := smokeFixture("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(600 * time.Millisecond):
			fixture.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	started := time.Now()
	if output, err := runSmoke(t, server.URL, "2"); err == nil {
		t.Fatalf("scenario exceeded total budget without failing: %s", output)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("total budget exceeded: %s", elapsed)
	}
}

func TestSmokeRejectsInvalidTimeoutBeforeSendingRequests(t *testing.T) {
	requireSmokeTools(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid timeout caused an HTTP request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, timeout := range []string{"0", "-1", "01", "1.5", "abc", "3601", "999999999999999999"} {
		if _, err := runSmoke(t, server.URL, timeout); err == nil {
			t.Errorf("timeout %q accepted", timeout)
		}
	}
}
