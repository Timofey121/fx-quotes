package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

func testConfig() Config {
	return Config{Concurrency: 3, LeaseDuration: time.Minute, PollInterval: 20 * time.Second, RecoveryInterval: time.Minute, ErrorDelay: 100 * time.Millisecond}
}
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func runPool(t *testing.T, p *Pool) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(cancel)
	return cancel, done
}
func stopped(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	synctest.Wait()
	select {
	case <-done:
	default:
		t.Fatal("pool did not join all goroutines after cancellation")
	}
}

func TestPoolDrainsRestartBacklogAndLogsStableOutcomeFields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var recovered atomic.Bool
		var calls atomic.Int64
		var log bytes.Buffer
		processor := fakeProcessor{
			recover: func(context.Context) ([]entity.QuoteUpdate, error) { recovered.Store(true); return nil, nil },
			process: func(ctx context.Context, cmd quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
				if !recovered.Load() {
					t.Error("claim before initial recovery")
				}
				if cmd.LeaseDuration != time.Minute {
					t.Errorf("lease = %s", cmd.LeaseDuration)
				}
				n := calls.Add(1)
				if n > 5 {
					return quote.ProcessResult{}, quote.ErrNoWork
				}
				return quote.ProcessResult{Update: entity.QuoteUpdate{ID: fmt.Sprintf("job-%d", n), Pair: entity.Pair{Base: entity.USD, Quote: entity.EUR}, Attempt: 2}, Outcome: quote.ProcessCompleted}, nil
			},
		}
		pool, err := New(processor, testConfig(), slog.New(slog.NewTextHandler(&log, nil)))
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("calls = %d, want backlog 5 plus 3 empty claims", calls.Load())
		}
		stopped(t, cancel, done)
		for _, field := range []string{"update_id=job-1", "pair=USD/EUR", "attempt=2", "outcome=completed"} {
			if !strings.Contains(log.String(), field) {
				t.Fatalf("missing %s in %s", field, log.String())
			}
		}
		if strings.Contains(log.String(), "idempotency") {
			t.Fatal("idempotency field in log")
		}
	})
}

func TestNotificationWakesOneWorkerPerJobAndNeverBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		var active, maxActive, calls atomic.Int64
		processor := fakeProcessor{process: func(ctx context.Context, _ quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
			calls.Add(1)
			if ctx.Err() != nil {
				t.Error("new claim with canceled lifecycle")
			}
			if !ready.Load() {
				return quote.ProcessResult{}, quote.ErrNoWork
			}
			current := active.Add(1)
			for old := maxActive.Load(); current > old && !maxActive.CompareAndSwap(old, current); old = maxActive.Load() {
			}
			<-ctx.Done()
			active.Add(-1)
			return quote.ProcessResult{Outcome: quote.ProcessLost}, nil
		}}
		pool, err := New(processor, testConfig(), discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatalf("initial workers = %d", calls.Load())
		}
		ready.Store(true)
		pool.Notify()
		synctest.Wait()
		if active.Load() != 1 {
			t.Fatalf("one job woke %d workers, want 1", active.Load())
		}
		for n := 0; n < 1000; n++ {
			pool.Notify()
		}
		synctest.Wait()
		if maxActive.Load() != 3 || calls.Load() != 6 {
			t.Fatalf("concurrency=%d claims=%d", maxActive.Load(), calls.Load())
		}
		stopped(t, cancel, done)
		if active.Load() != 0 || calls.Load() != 6 {
			t.Fatalf("shutdown active=%d claims=%d", active.Load(), calls.Load())
		}
	})
}

func TestPollingFindsWorkAfterMissedNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready, processed atomic.Bool
		processor := fakeProcessor{process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
			if ready.Swap(false) {
				processed.Store(true)
				return quote.ProcessResult{Outcome: quote.ProcessCompleted}, nil
			}
			return quote.ProcessResult{}, quote.ErrNoWork
		}}
		pool, err := New(processor, testConfig(), discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		pool.Notify() // Этот ранний сигнал расходуется до появления работы.
		cancel, done := runPool(t, pool)
		synctest.Wait()
		ready.Store(true)
		time.Sleep(19 * time.Second)
		synctest.Wait()
		if processed.Load() {
			t.Fatal("poll happened before interval")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !processed.Load() {
			t.Fatal("missed notification stranded pending work")
		}
		stopped(t, cancel, done)
	})
}

func TestPeriodicRecoveryWakesWorkersBeforeNextPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var recoveries, completed atomic.Int64
		var ready atomic.Bool
		processor := fakeProcessor{
			recover: func(context.Context) ([]entity.QuoteUpdate, error) {
				if recoveries.Add(1) == 2 {
					ready.Store(true)
					return []entity.QuoteUpdate{{ID: "expired", Status: entity.UpdatePending}}, nil
				}
				return nil, nil
			},
			process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
				if ready.Swap(false) {
					completed.Add(1)
					return quote.ProcessResult{Outcome: quote.ProcessCompleted}, nil
				}
				return quote.ProcessResult{}, quote.ErrNoWork
			},
		}
		config := testConfig()
		config.RecoveryInterval = 5 * time.Second
		pool, err := New(processor, config, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		synctest.Wait()
		if recoveries.Load() != 1 {
			t.Fatalf("startup recoveries=%d", recoveries.Load())
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if recoveries.Load() != 2 || completed.Load() != 1 {
			t.Fatalf("recoveries=%d completions=%d", recoveries.Load(), completed.Load())
		}
		stopped(t, cancel, done)
	})
}

func TestStoreErrorsWaitDespiteNotificationsAndRecoveryErrorsDoNotSpin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, recoveries atomic.Int64
		processor := fakeProcessor{
			process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
				calls.Add(1)
				return quote.ProcessResult{}, errors.New("store unavailable")
			},
			recover: func(context.Context) ([]entity.QuoteUpdate, error) {
				recoveries.Add(1)
				return nil, errors.New("store unavailable")
			},
		}
		config := testConfig()
		config.Concurrency = 1
		config.RecoveryInterval = time.Millisecond
		pool, err := New(processor, config, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		synctest.Wait()
		for n := 0; n < 100; n++ {
			pool.Notify()
		}
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 || recoveries.Load() != 1 {
			t.Fatalf("hot loop: process=%d recovery=%d", calls.Load(), recoveries.Load())
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 || recoveries.Load() != 2 {
			t.Fatalf("no delayed retry: process=%d recovery=%d", calls.Load(), recoveries.Load())
		}
		stopped(t, cancel, done)
	})
}

func TestShutdownCancelsRecoveryAndDoesNotStartClaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		processor := fakeProcessor{
			recover: func(ctx context.Context) ([]entity.QuoteUpdate, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
			process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
				t.Error("claim during startup recovery cancellation")
				return quote.ProcessResult{}, quote.ErrNoWork
			},
		}
		pool, err := New(processor, testConfig(), discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		<-started
		stopped(t, cancel, done)
	})
}

func TestCanceledLifecycleDoesNotRecoverOrClaim(t *testing.T) {
	processor := fakeProcessor{
		recover: func(context.Context) ([]entity.QuoteUpdate, error) {
			t.Error("recovery after cancellation")
			return nil, nil
		},
		process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
			t.Error("claim after cancellation")
			return quote.ProcessResult{}, quote.ErrNoWork
		},
	}
	pool, err := New(processor, testConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pool.Run(ctx)
}

func TestNewRejectsInvalidWorkerConfiguration(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Concurrency = 0 }, func(c *Config) { c.LeaseDuration = 0 }, func(c *Config) { c.PollInterval = 0 }, func(c *Config) { c.RecoveryInterval = 0 }, func(c *Config) { c.ErrorDelay = 0 },
	} {
		config := testConfig()
		mutate(&config)
		if _, err := New(fakeProcessor{}, config, discardLogger()); err == nil {
			t.Fatalf("accepted config %#v", config)
		}
	}
	if _, err := New(nil, testConfig(), discardLogger()); err == nil {
		t.Fatal("accepted nil processor")
	}
	if _, err := New(fakeProcessor{}, testConfig(), nil); err == nil {
		t.Fatal("accepted nil logger")
	}
}

type fakeProcessor struct {
	process func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error)
	recover func(context.Context) ([]entity.QuoteUpdate, error)
}

func (f fakeProcessor) ProcessNextUpdate(ctx context.Context, cmd quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
	return f.process(ctx, cmd)
}
func (f fakeProcessor) RecoverExpired(ctx context.Context) ([]entity.QuoteUpdate, error) {
	if f.recover != nil {
		return f.recover(ctx)
	}
	return nil, nil
}
