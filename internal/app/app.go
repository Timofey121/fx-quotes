// Пакет app связывает адаптеры с прикладными сценариями.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Timofey121/fx-quotes/internal/adapter/frankfurter"
	"github.com/Timofey121/fx-quotes/internal/adapter/httpapi"
	"github.com/Timofey121/fx-quotes/internal/adapter/postgres"
	"github.com/Timofey121/fx-quotes/internal/adapter/worker"
	appconfig "github.com/Timofey121/fx-quotes/internal/app/config"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Application struct {
	config    appconfig.Config
	pool      *pgxpool.Pool
	server    *http.Server
	workers   *worker.Pool
	readiness *readinessGate
}

func Build(ctx context.Context, config appconfig.Config, logger *slog.Logger) (*Application, error) {
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	// Запрос к провайдеру не удерживает SQL-соединение. Отдельно ограничиваем пул БД,
	// чтобы множество обработчиков не создавало очередь соединений и блокировок.
	poolConfig.MaxConns = int32(min(16, max(8, config.WorkerCount+6)))
	poolConfig.MinConns = 2
	poolConfig.MaxConnIdleTime = time.Minute
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	closePool := true
	defer func() {
		if closePool {
			pool.Close()
		}
	}()
	if err = postgres.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = int(poolConfig.MaxConns)
	transport.MaxIdleConnsPerHost = int(poolConfig.MaxConns)
	provider, err := frankfurter.New(&http.Client{Transport: transport}, frankfurter.Config{
		BaseURL: config.ProviderURL,
		Timeout: config.ProviderTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}
	store := postgres.NewStore(pool, config.MaxActive)
	service := quote.NewService(store, provider, config.MaxAttempts, quote.RetryPolicy{
		Initial: time.Second,
		Maximum: time.Minute,
	})
	workers, err := worker.New(service, worker.Config{
		Concurrency:      config.WorkerCount,
		LeaseDuration:    config.LeaseDuration,
		PollInterval:     config.PollInterval,
		RecoveryInterval: config.RecoveryInterval,
		ErrorDelay:       config.WorkerErrorDelay,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("create workers: %w", err)
	}
	readiness := newReadinessGate(pool)
	handler := httpapi.New(service, workers, readiness, logger)
	handler = httpapi.WithRequestTimeout(handler, config.RequestTimeout)
	application := &Application{
		config:    config,
		pool:      pool,
		workers:   workers,
		readiness: readiness,
		server: &http.Server{
			Addr:              config.ListenAddress,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       time.Minute,
			MaxHeaderBytes:    16 << 10,
		},
	}
	logger.Info("application configured",
		"listen_addr", config.ListenAddress,
		"provider_url", config.ProviderURL,
		"request_timeout", config.RequestTimeout,
		"shutdown_timeout", config.ShutdownTimeout,
		"worker_error_delay", config.WorkerErrorDelay,
		"provider_timeout", config.ProviderTimeout,
		"http_read_timeout", config.ReadTimeout,
		"http_write_timeout", config.WriteTimeout,
		"worker_count", config.WorkerCount,
		"poll_interval", config.PollInterval,
		"recovery_interval", config.RecoveryInterval,
		"lease_duration", config.LeaseDuration,
		"max_attempts", config.MaxAttempts,
		"max_active", config.MaxActive,
		"db_pool_max_conns", poolConfig.MaxConns,
	)
	closePool = false
	return application, nil
}

func (application *Application) Run(ctx context.Context) error {
	if application == nil {
		return fmt.Errorf("application is required")
	}
	application.readiness.SetReady(true)
	defer application.readiness.SetReady(false)
	defer application.pool.Close()
	return serve(ctx, application.config.ShutdownTimeout, application.server, application.workers, func() { application.readiness.SetReady(false) })
}
