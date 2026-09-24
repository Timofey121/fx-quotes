package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	config, err := LoadFromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != "127.0.0.1:8080" || config.WorkerCount != 2 || config.MaxAttempts != 3 || config.MaxActive != 100 {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if config.ProviderURL != "https://api.frankfurter.dev" || config.ProviderTimeout != 5*time.Second {
		t.Fatalf("unexpected provider defaults: %#v", config)
	}
	if config.ReadTimeout != 10*time.Second || config.WriteTimeout != 10*time.Second || config.RequestTimeout != 5*time.Second {
		t.Fatalf("unexpected HTTP timeout defaults: %#v", config)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"FX_QUOTES_LISTEN_ADDR":        "127.0.0.1:9000",
		"FX_QUOTES_DATABASE_URL":       "postgres://example",
		"FX_QUOTES_PROVIDER_URL":       "http://provider.test",
		"FX_QUOTES_PROVIDER_TIMEOUT":   "2s",
		"FX_QUOTES_HTTP_READ_TIMEOUT":  "4s",
		"FX_QUOTES_HTTP_WRITE_TIMEOUT": "6s",
		"FX_QUOTES_REQUEST_TIMEOUT":    "1500ms",
		"FX_QUOTES_WORKER_COUNT":       "4",
		"FX_QUOTES_POLL_INTERVAL":      "250ms",
		"FX_QUOTES_RECOVERY_INTERVAL":  "3s",
		"FX_QUOTES_WORKER_ERROR_DELAY": "300ms",
		"FX_QUOTES_LEASE_DURATION":     "3s",
		"FX_QUOTES_MAX_ATTEMPTS":       "5",
		"FX_QUOTES_MAX_ACTIVE":         "20",
		"FX_QUOTES_LOG_LEVEL":          "debug",
		"FX_QUOTES_SHUTDOWN_TIMEOUT":   "12s",
	}
	config, err := LoadFromLookup(func(key string) (string, bool) { value, present := values[key]; return value, present })
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != "127.0.0.1:9000" || config.DatabaseURL != "postgres://example" || config.WorkerCount != 4 || config.WorkerErrorDelay != 300*time.Millisecond || config.ReadTimeout != 4*time.Second || config.WriteTimeout != 6*time.Second || config.RequestTimeout != 1500*time.Millisecond || config.LogLevel != "debug" {
		t.Fatalf("overrides not applied: %#v", config)
	}
}

func TestLoadAcceptsDocumentedDurationBounds(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"FX_QUOTES_PROVIDER_TIMEOUT":   "5m",
		"FX_QUOTES_HTTP_READ_TIMEOUT":  "5m",
		"FX_QUOTES_HTTP_WRITE_TIMEOUT": "5m",
		"FX_QUOTES_REQUEST_TIMEOUT":    "5m",
		"FX_QUOTES_POLL_INTERVAL":      "10ms",
		"FX_QUOTES_RECOVERY_INTERVAL":  "100ms",
		"FX_QUOTES_WORKER_ERROR_DELAY": "10ms",
		"FX_QUOTES_LEASE_DURATION":     "10m",
		"FX_QUOTES_SHUTDOWN_TIMEOUT":   "5m",
		"FX_QUOTES_WORKER_COUNT":       "64",
		"FX_QUOTES_MAX_ATTEMPTS":       "10",
		"FX_QUOTES_MAX_ACTIVE":         "10000",
	}
	config, err := LoadFromLookup(func(key string) (string, bool) { value, present := values[key]; return value, present })
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkerCount != 64 || config.MaxAttempts != 10 || config.MaxActive != 10_000 {
		t.Fatalf("documented integer bounds were not retained: %#v", config)
	}
}

func TestLoadAcceptsMinimumDurationBounds(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"FX_QUOTES_PROVIDER_TIMEOUT":   "10ms",
		"FX_QUOTES_HTTP_READ_TIMEOUT":  "10ms",
		"FX_QUOTES_HTTP_WRITE_TIMEOUT": "10ms",
		"FX_QUOTES_REQUEST_TIMEOUT":    "10ms",
		"FX_QUOTES_POLL_INTERVAL":      "10ms",
		"FX_QUOTES_RECOVERY_INTERVAL":  "100ms",
		"FX_QUOTES_WORKER_ERROR_DELAY": "10ms",
		"FX_QUOTES_LEASE_DURATION":     "1010ms",
		"FX_QUOTES_SHUTDOWN_TIMEOUT":   "10ms",
	}
	if _, err := LoadFromLookup(func(key string) (string, bool) { value, present := values[key]; return value, present }); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsDurationValuesOutsideDocumentedBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"provider timeout below minimum", "FX_QUOTES_PROVIDER_TIMEOUT", "9ms"},
		{"provider timeout above maximum", "FX_QUOTES_PROVIDER_TIMEOUT", "5m1ms"},
		{"HTTP read timeout below minimum", "FX_QUOTES_HTTP_READ_TIMEOUT", "9ms"},
		{"HTTP read timeout above maximum", "FX_QUOTES_HTTP_READ_TIMEOUT", "5m1ms"},
		{"HTTP write timeout below minimum", "FX_QUOTES_HTTP_WRITE_TIMEOUT", "9ms"},
		{"HTTP write timeout above maximum", "FX_QUOTES_HTTP_WRITE_TIMEOUT", "5m1ms"},
		{"request timeout below minimum", "FX_QUOTES_REQUEST_TIMEOUT", "9ms"},
		{"request timeout above maximum", "FX_QUOTES_REQUEST_TIMEOUT", "5m1ms"},
		{"poll interval below minimum", "FX_QUOTES_POLL_INTERVAL", "9ms"},
		{"poll interval above maximum", "FX_QUOTES_POLL_INTERVAL", "1m1ms"},
		{"recovery interval below minimum", "FX_QUOTES_RECOVERY_INTERVAL", "99ms"},
		{"recovery interval above maximum", "FX_QUOTES_RECOVERY_INTERVAL", "1h1ms"},
		{"worker error delay below minimum", "FX_QUOTES_WORKER_ERROR_DELAY", "9ms"},
		{"worker error delay above maximum", "FX_QUOTES_WORKER_ERROR_DELAY", "30s1ms"},
		{"lease duration below minimum", "FX_QUOTES_LEASE_DURATION", "999ms"},
		{"lease duration above maximum", "FX_QUOTES_LEASE_DURATION", "10m1ms"},
		{"shutdown timeout below minimum", "FX_QUOTES_SHUTDOWN_TIMEOUT", "9ms"},
		{"shutdown timeout above maximum", "FX_QUOTES_SHUTDOWN_TIMEOUT", "5m1ms"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFromLookup(func(key string) (string, bool) {
				if key == test.key {
					return test.value, true
				}
				return "", false
			})
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("expected %s validation error, got %v", test.key, err)
			}
		})
	}
}

func TestLoadRejectsLeaseWhenProviderTimeoutCannotFitCommitMargin(t *testing.T) {
	t.Parallel()
	_, err := LoadFromLookup(func(key string) (string, bool) {
		switch key {
		case "FX_QUOTES_PROVIDER_TIMEOUT":
			return "5m", true
		case "FX_QUOTES_LEASE_DURATION":
			return "5m", true
		default:
			return "", false
		}
	})
	if err == nil || !strings.Contains(err.Error(), "FX_QUOTES_LEASE_DURATION") {
		t.Fatalf("expected lease validation error, got %v", err)
	}
}

func TestLoadRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"zero workers", "FX_QUOTES_WORKER_COUNT", "0"},
		{"invalid duration", "FX_QUOTES_POLL_INTERVAL", "later"},
		{"zero HTTP read timeout", "FX_QUOTES_HTTP_READ_TIMEOUT", "0s"},
		{"lease shorter than request and margin", "FX_QUOTES_LEASE_DURATION", "5s"},
		{"unknown log level", "FX_QUOTES_LOG_LEVEL", "loud"},
		{"blank database URL", "FX_QUOTES_DATABASE_URL", " "},
		{"blank provider URL", "FX_QUOTES_PROVIDER_URL", " "},
		{"worker count above bound", "FX_QUOTES_WORKER_COUNT", "65"},
		{"attempts above bound", "FX_QUOTES_MAX_ATTEMPTS", "11"},
		{"active updates above bound", "FX_QUOTES_MAX_ACTIVE", "10001"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFromLookup(func(key string) (string, bool) {
				if key == test.key {
					return test.value, true
				}
				if test.key == "FX_QUOTES_LEASE_DURATION" && key == "FX_QUOTES_PROVIDER_TIMEOUT" {
					return "5s", true
				}
				return "", false
			})
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("expected %s validation error, got %v", test.key, err)
			}
		})
	}
}

func TestLoadFromLookupRejectsExplicitEmptyURLs(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"FX_QUOTES_DATABASE_URL", "FX_QUOTES_PROVIDER_URL"} {
		t.Run(key, func(t *testing.T) {
			_, err := LoadFromLookup(func(candidate string) (string, bool) {
				return "", candidate == key
			})
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected explicit empty %s error, got %v", key, err)
			}
		})
	}
}

func TestLoadDatabaseURLSchemes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		valid bool
	}{
		{"postgres", "postgres://example/database", true},
		{"postgresql", "postgresql://example/database", true},
		{"HTTP", "http://example/database", false},
		{"MySQL", "mysql://example/database", false},
		{"uppercase", "POSTGRES://example/database", false},
		{"relative", "/database", false},
		{"blank", " ", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFromLookup(func(key string) (string, bool) {
				return test.value, key == "FX_QUOTES_DATABASE_URL"
			})
			if test.valid && err != nil {
				t.Fatalf("LoadFromLookup() error = %v", err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), "FX_QUOTES_DATABASE_URL")) {
				t.Fatalf("expected database URL validation error, got %v", err)
			}
		})
	}
}
