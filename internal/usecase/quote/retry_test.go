package quote

import (
	"context"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

func TestRetryPolicyBackoffJitterAndCap(t *testing.T) {
	policy := RetryPolicy{Initial: time.Second, Maximum: time.Minute}
	for _, tc := range []struct {
		attempt  int64
		min, max time.Duration
	}{
		{1, 500 * time.Millisecond, time.Second},
		{2, time.Second, 2 * time.Second},
		{3, 2 * time.Second, 4 * time.Second},
		{63, 30 * time.Second, time.Minute},
		{1 << 62, 30 * time.Second, time.Minute},
	} {
		delay := policy.Delay("update-a", tc.attempt, 0)
		if delay < tc.min || delay > tc.max {
			t.Fatalf("attempt %d delay %s outside [%s,%s]", tc.attempt, delay, tc.min, tc.max)
		}
		if again := policy.Delay("update-a", tc.attempt, 0); again != delay {
			t.Fatalf("jitter changed: %s / %s", delay, again)
		}
	}
	if policy.Delay("update-a", 1, 0) == policy.Delay("update-b", 1, 0) {
		t.Fatal("different jobs should spread their retries")
	}
	if policy.Delay("update-a", 6, 0) == policy.Delay("update-a", 7, 0) {
		t.Fatal("attempt must contribute to jitter")
	}
}

func TestRetryPolicyProviderHintIsBoundedLowerBound(t *testing.T) {
	policy := RetryPolicy{Initial: time.Second, Maximum: time.Minute}
	for _, tc := range []struct{ hint, want time.Duration }{
		{5 * time.Second, 5 * time.Second},
		{24 * time.Hour, time.Minute},
	} {
		if got := policy.Delay("update-a", 1, tc.hint); got != tc.want {
			t.Fatalf("hint %s delay %s, want %s", tc.hint, got, tc.want)
		}
	}
	if got := policy.Delay("update-a", 5, time.Millisecond); got < 8*time.Second || got > 16*time.Second {
		t.Fatalf("small hint suppressed backoff: %s", got)
	}
	if got := (RetryPolicy{}).Delay("update-a", 1, -time.Second); got <= 0 || got > time.Second {
		t.Fatalf("default delay = %s", got)
	}
}

func TestProcessAppliesRetryPolicyToStoredSchedule(t *testing.T) {
	store := &retryRecordingStore{memoryStore: newMemoryStore()}
	pair, _ := entity.ParsePair("EUR/USD")
	store.addPending(pair)
	service := NewService(store, stubProvider{err: ProviderError{Kind: FailureTemporary}}, 3, RetryPolicy{Initial: 10 * time.Second, Maximum: time.Minute})
	result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if result.RetryAfter < 5*time.Second || result.RetryAfter > 10*time.Second || store.retry.RetryAfter != result.RetryAfter {
		t.Fatalf("retry result=%#v, stored=%#v", result, store.retry)
	}
}

type retryRecordingStore struct {
	*memoryStore
	retry RetryCommand
}

func (s *retryRecordingStore) Retry(ctx context.Context, cmd RetryCommand) (entity.QuoteUpdate, error) {
	s.retry = cmd
	return s.memoryStore.Retry(ctx, cmd)
}
