package deployment_test

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Timofey121/fx-quotes/internal/app/config"
)

func TestComposeRemoteHostDoesNotInheritLocalTLSException(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	var document composeDocument
	if err := json.Unmarshal(composeConfig(t, map[string]string{"FX_QUOTES_DATABASE_HOST": "db.example"}), &document); err != nil {
		t.Fatal(err)
	}
	env := document.Services["app"].Environment
	cfg, err := config.LoadFromLookup(func(key string) (string, bool) { v, ok := env[key]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil || u.Query().Get("sslmode") != "verify-full" {
		t.Fatal("remote Compose database inherited disabled TLS")
	}
}

type composeDocument struct {
	Name    string `json:"name"`
	Volumes map[string]struct {
		Name string `json:"name"`
	} `json:"volumes"`
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	Environment     composeEnvironment `json:"environment"`
	Healthcheck     composeHealthcheck `json:"healthcheck"`
	Ports           []composePort      `json:"ports"`
	StopGracePeriod string             `json:"stop_grace_period"`
}

// Значение null в Compose означает отсутствие переменной, а не пустую строку.
type composeEnvironment map[string]string

func (environment *composeEnvironment) UnmarshalJSON(data []byte) error {
	var values map[string]*string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	*environment = make(composeEnvironment, len(values))
	for key, value := range values {
		if value != nil {
			(*environment)[key] = *value
		}
	}
	return nil
}

type composeHealthcheck struct {
	Test []string `json:"test"`
}

type composePort struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	HostIP    string `json:"host_ip"`
}

func TestComposeForwardsApplicationConfiguration(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker executable is required to validate the Compose contract")
	}
	output := composeConfig(t, composeValues)
	var document composeDocument
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode docker compose config: %v", err)
	}
	environment := document.Services["app"].Environment
	for key, expected := range composeValues {
		if actual := environment[key]; actual != expected {
			t.Errorf("app environment %s = %q, want %q", key, actual, expected)
		}
	}
}

func TestComposeWiresCustomListenPortHealthcheckAndShutdownGracePeriod(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker executable is required to validate the Compose contract")
	}
	output := composeConfig(t, map[string]string{
		"FX_QUOTES_LISTEN_ADDR":       ":9080",
		"FX_QUOTES_CONTAINER_PORT":    "9080",
		"FX_QUOTES_HOST_PORT":         "19080",
		"FX_QUOTES_HEALTHCHECK_URL":   "http://127.0.0.1:9080",
		"FX_QUOTES_STOP_GRACE_PERIOD": "45s",
	})
	var document composeDocument
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode docker compose config: %v", err)
	}
	app := document.Services["app"]
	if got := app.Environment["FX_QUOTES_LISTEN_ADDR"]; got != ":9080" {
		t.Errorf("app listen address = %q, want :9080", got)
	}
	if got := app.Environment["FX_QUOTES_HEALTHCHECK_URL"]; got != "http://127.0.0.1:9080" {
		t.Errorf("healthcheck URL environment = %q", got)
	}
	if len(app.Healthcheck.Test) != 2 || app.Healthcheck.Test[1] != "wget -q -O - $${FX_QUOTES_HEALTHCHECK_URL:-http://127.0.0.1:8080}/health/ready >/dev/null" {
		t.Errorf("unexpected healthcheck: %#v", app.Healthcheck.Test)
	}
	if len(app.Ports) != 1 || app.Ports[0].HostIP != "127.0.0.1" || app.Ports[0].Target != 9080 || app.Ports[0].Published != "19080" {
		t.Errorf("unexpected published port: %#v", app.Ports)
	}
	if app.StopGracePeriod != "45s" {
		t.Errorf("stop grace period = %q, want 45s", app.StopGracePeriod)
	}
}

func TestComposeDefaultsStopGracePeriodToShutdownAndCleanupBudget(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker executable is required to validate the Compose contract")
	}
	output := composeConfig(t, map[string]string{"FX_QUOTES_SHUTDOWN_TIMEOUT": "45s"})
	var document composeDocument
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode docker compose config: %v", err)
	}
	if got := document.Services["app"].StopGracePeriod; got != "5m10s" {
		t.Errorf("stop grace period = %q, want 5m10s for shutdown and cleanup", got)
	}
}

var composeValues = map[string]string{
	"FX_QUOTES_LISTEN_ADDR":        ":8080",
	"FX_QUOTES_DATABASE_URL":       "postgres://override:secret@database.example:5432/quotes?sslmode=require",
	"FX_QUOTES_PROVIDER_URL":       "http://provider.example",
	"FX_QUOTES_PROVIDER_TIMEOUT":   "2s",
	"FX_QUOTES_HTTP_READ_TIMEOUT":  "3s",
	"FX_QUOTES_HTTP_WRITE_TIMEOUT": "4s",
	"FX_QUOTES_REQUEST_TIMEOUT":    "1500ms",
	"FX_QUOTES_WORKER_COUNT":       "64",
	"FX_QUOTES_POLL_INTERVAL":      "10ms",
	"FX_QUOTES_RECOVERY_INTERVAL":  "100ms",
	"FX_QUOTES_WORKER_ERROR_DELAY": "10ms",
	"FX_QUOTES_LEASE_DURATION":     "5s",
	"FX_QUOTES_MAX_ATTEMPTS":       "10",
	"FX_QUOTES_MAX_ACTIVE":         "10000",
	"FX_QUOTES_LOG_LEVEL":          "debug",
	"FX_QUOTES_SHUTDOWN_TIMEOUT":   "30s",
}

func composeConfig(t *testing.T, values map[string]string) []byte {
	return renderCompose(t, "compose.yaml", "", values)
}

func renderCompose(t *testing.T, file, project string, values map[string]string) []byte {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "compose.env")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values[key])
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"compose", "--env-file", envFile, "-f", file}
	if project != "" {
		args = append(args, "-p", project)
	}
	args = append(args, "config", "--format", "json")
	command := exec.Command("docker", args...)
	command.Dir = moduleRoot(t)
	command.Env = dockerRuntimeEnvironment(t)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	return output
}

func TestComposeDatabasePasswordSurvivesConfiguration(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	var document composeDocument
	if err := json.Unmarshal(composeConfig(t, map[string]string{"POSTGRES_PASSWORD": "local/test?pass#%"}), &document); err != nil {
		t.Fatal(err)
	}
	env := document.Services["app"].Environment
	cfg, err := config.LoadFromLookup(func(key string) (string, bool) { v, ok := env[key]; return v, ok })
	if err != nil {
		t.Fatalf("rendered Compose configuration cannot start: %v", err)
	}
	if cfg.DatabaseURL != "postgres://fxquotes:local%2Ftest%3Fpass%23%25@postgres:5432/fxquotes?sslmode=disable" {
		t.Fatal("password did not survive URL encoding")
	}
}

func TestLoadComposeIsolatesDatabaseProviderAndPort(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	var base, load composeDocument
	if err := json.Unmarshal(composeConfig(t, nil), &base); err != nil {
		t.Fatal(err)
	}
	// Обычные настройки приложения не должны направлять нагрузку на реальные данные.
	values := map[string]string{"FX_QUOTES_DATABASE_URL": "postgres://real/data", "FX_QUOTES_PROVIDER_URL": "https://api.frankfurter.dev", "COMPOSE_PROJECT_NAME": base.Name}
	if err := json.Unmarshal(renderCompose(t, "compose.load.yaml", "fx-quotes-load", values), &load); err != nil {
		t.Fatal(err)
	}
	if _, ok := load.Services["postgres"]; !ok {
		t.Fatal("load deployment has no isolated database")
	}
	for _, a := range base.Volumes {
		for _, b := range load.Volumes {
			if a.Name == b.Name {
				t.Fatal("load shares the application database volume")
			}
		}
	}
	app := load.Services["app"]
	if app.Environment["FX_QUOTES_PROVIDER_URL"] != "http://fixture-provider:8090" {
		t.Fatal("load can target public provider")
	}
	if app.Environment["FX_QUOTES_DATABASE_URL"] != "" || app.Environment["FX_QUOTES_DATABASE_HOST"] != "postgres" {
		t.Fatal("load can target ordinary database")
	}
	if len(app.Ports) != 1 || app.Ports[0].Published != "18080" || app.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("load port is not isolated: %+v", app.Ports)
	}
}

func dockerRuntimeEnvironment(t *testing.T) []string {
	t.Helper()
	values := make([]string, 0, 2)
	for _, key := range []string{"PATH", "HOME"} {
		value, ok := os.LookupEnv(key)
		if !ok || value == "" {
			t.Fatalf("%s must be set to run docker compose", key)
		}
		values = append(values, key+"="+value)
	}
	return values
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return path
}
