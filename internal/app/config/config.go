// Пакет config читает и проверяет настройки приложения из переменных окружения.
package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	commitMargin = time.Second

	minRequestTimeout = 10 * time.Millisecond
	maxRequestTimeout = 5 * time.Minute
	minPollInterval   = 10 * time.Millisecond
	maxPollInterval   = time.Minute
	minRecoveryPeriod = 100 * time.Millisecond
	maxRecoveryPeriod = time.Hour
	minErrorDelay     = 10 * time.Millisecond
	maxErrorDelay     = 30 * time.Second
	minLeaseDuration  = time.Second
	maxLeaseDuration  = 10 * time.Minute
)

const (
	maxWorkerCount = 64
	maxAttempts    = 10
	maxActive      = 10_000
)

type Config struct {
	ListenAddress    string
	DatabaseURL      string
	ProviderURL      string
	ProviderTimeout  time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	RequestTimeout   time.Duration
	WorkerCount      int
	PollInterval     time.Duration
	RecoveryInterval time.Duration
	WorkerErrorDelay time.Duration
	LeaseDuration    time.Duration
	MaxAttempts      int
	MaxActive        int
	LogLevel         string
	ShutdownTimeout  time.Duration
}

func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("environment lookup is required")
	}
	config := Config{
		ListenAddress: value(lookup, "FX_QUOTES_LISTEN_ADDR", "127.0.0.1:8080"),
		ProviderURL:   value(lookup, "FX_QUOTES_PROVIDER_URL", "https://api.frankfurter.dev"),
		LogLevel:      value(lookup, "FX_QUOTES_LOG_LEVEL", "info"),
	}
	var err error
	if config.DatabaseURL, err = databaseURL(lookup); err != nil {
		return Config{}, err
	}
	if config.ProviderTimeout, err = boundedDuration(lookup, "FX_QUOTES_PROVIDER_TIMEOUT", 5*time.Second, minRequestTimeout, maxRequestTimeout); err != nil {
		return Config{}, err
	}
	if config.ReadTimeout, err = boundedDuration(lookup, "FX_QUOTES_HTTP_READ_TIMEOUT", 10*time.Second, minRequestTimeout, maxRequestTimeout); err != nil {
		return Config{}, err
	}
	if config.WriteTimeout, err = boundedDuration(lookup, "FX_QUOTES_HTTP_WRITE_TIMEOUT", 10*time.Second, minRequestTimeout, maxRequestTimeout); err != nil {
		return Config{}, err
	}
	if config.RequestTimeout, err = boundedDuration(lookup, "FX_QUOTES_REQUEST_TIMEOUT", 5*time.Second, minRequestTimeout, maxRequestTimeout); err != nil {
		return Config{}, err
	}
	if config.PollInterval, err = boundedDuration(lookup, "FX_QUOTES_POLL_INTERVAL", 500*time.Millisecond, minPollInterval, maxPollInterval); err != nil {
		return Config{}, err
	}
	if config.RecoveryInterval, err = boundedDuration(lookup, "FX_QUOTES_RECOVERY_INTERVAL", time.Minute, minRecoveryPeriod, maxRecoveryPeriod); err != nil {
		return Config{}, err
	}
	if config.WorkerErrorDelay, err = boundedDuration(lookup, "FX_QUOTES_WORKER_ERROR_DELAY", time.Second, minErrorDelay, maxErrorDelay); err != nil {
		return Config{}, err
	}
	if config.LeaseDuration, err = boundedDuration(lookup, "FX_QUOTES_LEASE_DURATION", 15*time.Second, minLeaseDuration, maxLeaseDuration); err != nil {
		return Config{}, err
	}
	if config.ShutdownTimeout, err = boundedDuration(lookup, "FX_QUOTES_SHUTDOWN_TIMEOUT", 15*time.Second, minRequestTimeout, maxRequestTimeout); err != nil {
		return Config{}, err
	}
	if config.WorkerCount, err = boundedInt(lookup, "FX_QUOTES_WORKER_COUNT", 2, maxWorkerCount); err != nil {
		return Config{}, err
	}
	if config.MaxAttempts, err = boundedInt(lookup, "FX_QUOTES_MAX_ATTEMPTS", 3, maxAttempts); err != nil {
		return Config{}, err
	}
	if config.MaxActive, err = boundedInt(lookup, "FX_QUOTES_MAX_ACTIVE", 100, maxActive); err != nil {
		return Config{}, err
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) validate() error {
	if strings.TrimSpace(config.ListenAddress) == "" {
		return fmt.Errorf("FX_QUOTES_LISTEN_ADDR must not be blank")
	}
	if strings.TrimSpace(config.DatabaseURL) == "" {
		return fmt.Errorf("FX_QUOTES_DATABASE_URL must not be blank")
	}
	databaseScheme, _, _ := strings.Cut(config.DatabaseURL, ":")
	if database, parseErr := url.Parse(config.DatabaseURL); parseErr != nil || (databaseScheme != "postgres" && databaseScheme != "postgresql") || database.Host == "" {
		return fmt.Errorf("FX_QUOTES_DATABASE_URL must be an absolute URL")
	}
	if provider, parseErr := url.Parse(config.ProviderURL); parseErr != nil || (provider.Scheme != "http" && provider.Scheme != "https") || provider.Host == "" || provider.User != nil || provider.RawQuery != "" || provider.Fragment != "" {
		return fmt.Errorf("FX_QUOTES_PROVIDER_URL must be an absolute HTTP URL without credentials, query, or fragment")
	}
	if config.LeaseDuration < commitMargin || config.ProviderTimeout > config.LeaseDuration-commitMargin {
		return fmt.Errorf("FX_QUOTES_LEASE_DURATION must cover FX_QUOTES_PROVIDER_TIMEOUT plus %s commit margin", commitMargin)
	}
	switch config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("FX_QUOTES_LOG_LEVEL must be debug, info, warn, or error")
	}
	return nil
}

func databaseURL(lookup func(string) (string, bool)) (string, error) {
	if explicit, present := lookup("FX_QUOTES_DATABASE_URL"); present {
		return explicit, nil
	}
	host := value(lookup, "FX_QUOTES_DATABASE_HOST", "127.0.0.1")
	port, err := boundedInt(lookup, "FX_QUOTES_DATABASE_PORT", 5432, 65535)
	if err != nil {
		return "", err
	}
	user := value(lookup, "FX_QUOTES_DATABASE_USER", "fxquotes")
	password := value(lookup, "FX_QUOTES_DATABASE_PASSWORD", "fxquotes")
	name := value(lookup, "FX_QUOTES_DATABASE_NAME", "fxquotes")
	if strings.TrimSpace(host) == "" || strings.ContainsAny(host, "/?#@%[]") || user == "" || name == "" {
		return "", fmt.Errorf("database host, user and name must be valid non-empty components")
	}
	sslMode := "verify-full"
	if address := net.ParseIP(host); address != nil && address.IsLoopback() {
		sslMode = "disable"
	}
	// Исключение привязано к хосту: смена адреса БД не переносит отключение TLS.
	if trustedHost := value(lookup, "FX_QUOTES_DATABASE_INSECURE_HOST", ""); trustedHost != "" && host == trustedHost {
		sslMode = "disable"
	}
	database := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + name,
		RawPath:  "/" + url.PathEscape(name),
		RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
	}
	return database.String(), nil
}

func value(lookup func(string) (string, bool), key, fallback string) string {
	if raw, present := lookup(key); present {
		return raw
	}
	return fallback
}

func boundedDuration(lookup func(string) (string, bool), key string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := value(lookup, key, fallback.String())
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be a duration from %s through %s", key, minimum, maximum)
	}
	return parsed, nil
}

func boundedInt(lookup func(string) (string, bool), key string, fallback, maximum int) (int, error) {
	raw := value(lookup, key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, fmt.Errorf("%s must be a positive integer up to %d", key, maximum)
	}
	return parsed, nil
}
