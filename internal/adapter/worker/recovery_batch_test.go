package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

func TestRecoveryContinuesBacklogWithoutWaitingFullInterval(t *testing.T) {
	config := testConfig()
	config.PollInterval = 25 * time.Millisecond
	for _, recovered := range []int{0, 100} {
		pool, err := New(fakeProcessor{recover: func(context.Context) ([]entity.QuoteUpdate, error) {
			return make([]entity.QuoteUpdate, recovered), nil
		}}, config, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		want := time.Minute
		if recovered > 0 {
			want = 25 * time.Millisecond
		}
		if delay := pool.recoverExpiredUpdates(context.Background()); delay != want {
			t.Fatalf("recovered=%d next delay=%s want=%s", recovered, delay, want)
		}
	}
}

func TestRecoveryBatchWakesAtMostOneWorkerPerRecoveredUpdate(t *testing.T) {
	for _, recovered := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d recovered updates", recovered), func(t *testing.T) {
			config := testConfig()
			config.Concurrency = 3
			updates := make([]entity.QuoteUpdate, recovered)
			pool, err := New(fakeProcessor{recover: func(context.Context) ([]entity.QuoteUpdate, error) {
				return updates, nil
			}}, config, discardLogger())
			if err != nil {
				t.Fatal(err)
			}

			pool.recoverExpiredUpdates(context.Background())
			if wakeups := len(pool.wakeup); wakeups != min(recovered, config.Concurrency) {
				t.Fatalf("recovery wakeups = %d, want %d", wakeups, min(recovered, config.Concurrency))
			}
		})
	}
}
