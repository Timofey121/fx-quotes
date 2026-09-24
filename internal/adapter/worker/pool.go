package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

// Processor предоставляет фоновым обработчикам прикладные сценарии.
type Processor interface {
	ProcessNextUpdate(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error)
	RecoverExpired(context.Context) ([]entity.QuoteUpdate, error)
}

var _ Processor = quote.Service{}

type Config struct {
	Concurrency      int
	LeaseDuration    time.Duration
	PollInterval     time.Duration
	RecoveryInterval time.Duration
	ErrorDelay       time.Duration
}

type Pool struct {
	processor Processor
	config    Config
	logger    *slog.Logger
	wakeup    chan struct{}
}

func New(processor Processor, config Config, logger *slog.Logger) (*Pool, error) {
	if processor == nil || logger == nil {
		return nil, fmt.Errorf("worker processor and logger are required")
	}
	if config.Concurrency <= 0 || config.LeaseDuration <= 0 || config.PollInterval <= 0 || config.RecoveryInterval <= 0 || config.ErrorDelay <= 0 {
		return nil, fmt.Errorf("worker concurrency and durations must be positive")
	}
	return &Pool{
		processor: processor,
		config:    config,
		logger:    logger,
		wakeup:    make(chan struct{}, config.Concurrency),
	}, nil
}

// Notify будит один обработчик для принятого задания. Вызов не блокируется,
// допустим до Run и при остановке; уведомление лишь сокращает ожидание.
func (pool *Pool) Notify() {
	select {
	case pool.wakeup <- struct{}{}:
	default:
	}
}

// Run управляет рабочими горутинами и восстановлением, ожидая их завершения.
// Вызывается один раз с контекстом приложения, а не отдельного HTTP-запроса.
func (pool *Pool) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	recoveryDelay := pool.recoverExpiredUpdates(ctx)
	if ctx.Err() != nil {
		return
	}
	var group sync.WaitGroup
	group.Add(pool.config.Concurrency + 1)
	go func() {
		defer group.Done()
		for pause(ctx, recoveryDelay, nil) {
			if ctx.Err() != nil {
				return
			}
			recoveryDelay = pool.recoverExpiredUpdates(ctx)
		}
	}()
	for range pool.config.Concurrency {
		go func() {
			defer group.Done()
			pool.work(ctx)
		}()
	}
	group.Wait()
}

func (pool *Pool) work(ctx context.Context) {
	for ctx.Err() == nil {
		result, err := pool.processor.ProcessNextUpdate(ctx, quote.ProcessNextUpdateCommand{LeaseDuration: pool.config.LeaseDuration})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, quote.ErrNoWork) {
			if !pause(ctx, pool.config.PollInterval, pool.wakeup) {
				return
			}
			continue
		}
		if err != nil {
			pool.logError(ctx, "quote update processing failed", err)
			// Уведомления не должны прерывать паузу после ошибки хранилища.
			if !pause(ctx, pool.config.ErrorDelay, nil) {
				return
			}
			continue
		}
		pool.logger.InfoContext(ctx, "quote update processed",
			"update_id", result.Update.ID,
			"pair", result.Update.Pair.String(),
			"attempt", result.Update.Attempt,
			"outcome", result.Outcome,
		)
	}
}

func (pool *Pool) recoverExpiredUpdates(ctx context.Context) time.Duration {
	updates, err := pool.processor.RecoverExpired(ctx)
	if ctx.Err() != nil {
		return pool.config.RecoveryInterval
	}
	if err != nil {
		pool.logError(ctx, "quote update recovery failed", err)
		return max(pool.config.RecoveryInterval, pool.config.ErrorDelay)
	}
	for _, update := range updates {
		pool.logger.InfoContext(ctx, "quote update recovered",
			"update_id", update.ID,
			"pair", update.Pair.String(),
			"attempt", update.Attempt,
			"outcome", update.Status,
		)
	}
	for range min(len(updates), pool.config.Concurrency) {
		pool.Notify()
	}
	if len(updates) > 0 {
		// Продолжаем хвост короткими пакетами, оставляя паузу для остальных запросов.
		return min(pool.config.PollInterval, pool.config.RecoveryInterval)
	}
	return pool.config.RecoveryInterval
}

// Пишем категорию ошибки: исходный текст драйвера может раскрыть реквизиты БД или данные строк.
func (pool *Pool) logError(ctx context.Context, message string, err error) {
	kind := "internal"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	}
	root := err
	for errors.Unwrap(root) != nil {
		root = errors.Unwrap(root)
	}
	fields := []any{"outcome", "error", "error_kind", kind, "error_type", fmt.Sprintf("%T", root)}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		valid := len(code) == 5
		for _, c := range code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				valid = false
			}
		}
		if valid {
			fields = append(fields, "sqlstate", code)
		}
	}
	pool.logger.ErrorContext(ctx, message, fields...)
}

func pause(ctx context.Context, delay time.Duration, wakeup <-chan struct{}) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-wakeup:
		return true
	}
}
