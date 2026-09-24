package quote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

func TestCreateWithoutIdempotencyKeyCreatesNewUpdates(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, 3, RetryPolicy{})
	pair, _ := entity.ParsePair("USD/EUR")

	first, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair})
	if err != nil {
		t.Fatalf("first Create returned an error: %v", err)
	}
	second, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair})
	if err != nil {
		t.Fatalf("second Create returned an error: %v", err)
	}
	if !first.Created || !second.Created || first.Update.ID == second.Update.ID {
		t.Fatalf("Create without a key = %#v then %#v, want two new updates", first, second)
	}
}

func TestCreateWithIdempotencyKeyReplaysTerminalUpdateAndRejectsDifferentPair(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, 3, RetryPolicy{})
	pair, _ := entity.ParsePair("USD/EUR")
	first, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair, IdempotencyKey: "request-1"})
	if err != nil {
		t.Fatalf("first Create returned an error: %v", err)
	}
	terminal := first.Update
	rate, _ := entity.ParseRate("1.2")
	terminal.Status = entity.UpdateCompleted
	terminal.Attempt = 1
	terminal.Quote = &entity.Quote{Pair: pair, Rate: rate}
	store.updates[terminal.ID] = terminal

	replay, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair, IdempotencyKey: "request-1"})
	if err != nil {
		t.Fatalf("replay Create returned an error: %v", err)
	}
	if replay.Created || replay.Update.ID != first.Update.ID || replay.Update.Status != entity.UpdateCompleted {
		t.Fatalf("replay = %#v, want the completed original update", replay)
	}
	differentPair, _ := entity.ParsePair("USD/MXN")
	_, err = service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: differentPair, IdempotencyKey: "request-1"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("Create with a reused key and different pair error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreatePreservesCapacityError(t *testing.T) {
	store := newMemoryStore()
	store.capacityFull = true
	service := NewService(store, nil, 3, RetryPolicy{})
	pair, _ := entity.ParsePair("USD/EUR")
	_, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair})
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Create error = %v, want ErrCapacityExceeded", err)
	}
}

func TestCreateBoundsIdempotencyKeysByBytesBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		invalid   bool
	}{
		{"empty", "", false},
		{"128 ASCII bytes", strings.Repeat("A", 128), false},
		{"129 ASCII bytes", strings.Repeat("A", 129), true},
		{"128 UTF-8 bytes", strings.Repeat("é", 64), false},
		{"129 UTF-8 bytes", strings.Repeat("é", 64) + "a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			service := NewService(store, nil, 3, RetryPolicy{})
			pair, _ := entity.ParsePair("USD/EUR")
			result, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair, IdempotencyKey: tc.key})
			if tc.invalid {
				if !errors.Is(err, ErrInvalidIdempotencyKey) {
					t.Fatalf("error=%v, want ErrInvalidIdempotencyKey", err)
				}
				if store.createCalls != 0 || len(store.updates) != 0 {
					t.Fatalf("invalid input reached storage: calls=%d rows=%d", store.createCalls, len(store.updates))
				}
				return
			}
			if err != nil || !result.Created {
				t.Fatalf("accepted boundary=%#v %v", result, err)
			}
			if tc.key != "" && store.idempotency[tc.key] != result.Update.ID {
				t.Fatal("key was not preserved exactly")
			}
		})
	}
}

func TestGetUpdateAndLatestReturnStoredDomainValues(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, 3, RetryPolicy{})
	pair, _ := entity.ParsePair("EUR/MXN")
	created, err := service.CreateUpdate(context.Background(), CreateUpdateCommand{Pair: pair})
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	rate, _ := entity.ParseRate("18.500")
	storedQuote := entity.Quote{Pair: pair, Rate: rate, Provider: "bank", SourceDate: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	store.latest[pair.String()] = storedQuote

	update, err := service.GetUpdate(context.Background(), GetUpdateQuery{ID: created.Update.ID})
	if err != nil || update.ID != created.Update.ID {
		t.Fatalf("GetUpdate() = %#v, %v; want created update", update, err)
	}
	latest, err := service.GetLatest(context.Background(), GetLatestQuery{Pair: pair})
	if err != nil || latest.Rate.String() != "18.5" || !latest.UpdatedAt.Equal(storedQuote.UpdatedAt) || latest.UpdatedAt.Equal(latest.SourceDate) {
		t.Fatalf("GetLatest() = %#v, %v; want stored quote and persistence timestamp", latest, err)
	}
	if _, err := service.GetUpdate(context.Background(), GetUpdateQuery{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUpdate missing error = %v, want ErrNotFound", err)
	}
}

func TestProcessClaimsNextDueUpdateAndCompletesIt(t *testing.T) {
	store := newMemoryStore()
	pair, _ := entity.ParsePair("USD/MXN")
	store.addPending(pair)
	rate, _ := entity.ParseRate("19.25")
	provider := stubProvider{quote: entity.Quote{Pair: pair, Rate: rate, Provider: "provider-a", SourceDate: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)}}
	service := NewService(store, provider, 3, RetryPolicy{})
	result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Process returned an error: %v", err)
	}
	if result.Outcome != ProcessCompleted || result.Update.Status != entity.UpdateCompleted || result.Update.Quote == nil || result.Update.Quote.Rate.String() != "19.25" {
		t.Fatalf("Process result = %#v, want completed claimed update with quote", result)
	}
}

func TestProcessReturnsNoWorkWhenNoEligibleUpdateExists(t *testing.T) {
	service := NewService(newMemoryStore(), stubProvider{}, 3, RetryPolicy{})
	_, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if !errors.Is(err, ErrNoWork) {
		t.Fatalf("Process error = %v, want ErrNoWork", err)
	}
}

func TestProcessRetriesWrappedTemporaryProviderErrorsInPointerAndValueForms(t *testing.T) {
	for name, providerError := range map[string]error{
		"pointer": fmt.Errorf("provider response: %w", &ProviderError{Kind: FailureTemporary, RetryAfter: 5 * time.Second, Err: errors.New("timeout")}),
		"value":   fmt.Errorf("provider response: %w", ProviderError{Kind: FailureThrottled, RetryAfter: 5 * time.Second, Err: errors.New("throttled")}),
	} {
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			pair, _ := entity.ParsePair("USD/EUR")
			store.addPending(pair)
			service := NewService(store, stubProvider{err: providerError}, 2, RetryPolicy{})
			result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
			if err != nil {
				t.Fatalf("Process returned an error: %v", err)
			}
			if result.Outcome != ProcessRetryScheduled || result.RetryAfter != 5*time.Second || result.Update.Status != entity.UpdatePending {
				t.Fatalf("Process result = %#v, want retry scheduled for pending update", result)
			}
		})
	}
}

func TestProcessRetriesProviderLocalDeadlineWhileContextIsLive(t *testing.T) {
	store := newMemoryStore()
	pair, _ := entity.ParsePair("USD/EUR")
	store.addPending(pair)
	service := NewService(store, stubProvider{err: &ProviderError{Kind: FailureTemporary, Err: context.DeadlineExceeded}}, 3, RetryPolicy{})
	result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Process returned an error: %v", err)
	}
	if result.Outcome != ProcessRetryScheduled || result.Update.Status != entity.UpdatePending {
		t.Fatalf("Process result = %#v, want a retry for provider-local timeout", result)
	}
}

func TestProcessFailsPermanentAndExhaustedTemporaryFailuresWithStableCodes(t *testing.T) {
	for name, scenario := range map[string]struct {
		provider    stubProvider
		initialTry  int64
		wantFailure entity.FailureCode
	}{
		"permanent":           {provider: stubProvider{err: &ProviderError{Kind: FailurePermanent, Code: entity.FailureProviderPermanent, Err: errors.New("bad credentials")}}, wantFailure: entity.FailureProviderPermanent},
		"exhausted temporary": {provider: stubProvider{err: &ProviderError{Kind: FailureTemporary, Err: errors.New("timeout")}}, initialTry: 1, wantFailure: entity.FailureAttemptsExhausted},
	} {
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			pair, _ := entity.ParsePair("EUR/USD")
			update := store.addPending(pair)
			update.Attempt = scenario.initialTry
			store.updates[update.ID] = update
			service := NewService(store, scenario.provider, 2, RetryPolicy{})
			result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
			if err != nil {
				t.Fatalf("Process returned an error: %v", err)
			}
			if result.Outcome != ProcessFailed || result.Update.Status != entity.UpdateFailed || result.Update.FailureCode != scenario.wantFailure {
				t.Fatalf("Process result = %#v, want failed update with code %q", result, scenario.wantFailure)
			}
		})
	}
}

func TestProcessLeavesClaimedWorkIncompleteOnlyWhenProcessingContextIsCanceled(t *testing.T) {
	store := newMemoryStore()
	pair, _ := entity.ParsePair("MXN/EUR")
	store.addPending(pair)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := NewService(store, stubProvider{err: context.Canceled}, 3, RetryPolicy{})
	result, err := service.ProcessNextUpdate(ctx, ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Process returned an error: %v", err)
	}
	if result.Outcome != ProcessLost || result.Update.Status != entity.UpdateProcessing {
		t.Fatalf("Process result = %#v, want lost processing work", result)
	}
}

func TestProcessReportsLostOwnershipAfterFencedCompletion(t *testing.T) {
	store := newMemoryStore()
	store.lostOwnershipOn = "complete"
	pair, _ := entity.ParsePair("USD/MXN")
	store.addPending(pair)
	rate, _ := entity.ParseRate("19.25")
	service := NewService(store, stubProvider{quote: entity.Quote{Pair: pair, Rate: rate}}, 3, RetryPolicy{})
	result, err := service.ProcessNextUpdate(context.Background(), ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Process returned an error: %v", err)
	}
	if result.Outcome != ProcessLost || result.Update.Status != entity.UpdateProcessing {
		t.Fatalf("Process result = %#v, want lost ownership", result)
	}
}

func TestProcessLeavesClaimedWorkForRecoveryWhenProviderReturnsAfterCancellation(t *testing.T) {
	store := newMemoryStore()
	pair, _ := entity.ParsePair("USD/EUR")
	created := store.addPending(pair)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rate, _ := entity.ParseRate("1.25")
	service := NewService(store, cancelingProvider{cancel: cancel, quote: entity.Quote{Pair: pair, Rate: rate}}, 3, RetryPolicy{})
	result, err := service.ProcessNextUpdate(ctx, ProcessNextUpdateCommand{LeaseDuration: time.Minute})
	if err != nil || result.Outcome != ProcessLost || store.updates[created.ID].Status != entity.UpdateProcessing || len(store.latest) != 0 {
		t.Fatalf("canceled process = %#v, %v, stored=%#v", result, err, store.updates[created.ID])
	}
}

func TestRecoverExpiredPassesConfiguredAttemptLimit(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, 4, RetryPolicy{})
	if _, err := service.RecoverExpired(context.Background()); err != nil {
		t.Fatalf("RecoverExpired returned an error: %v", err)
	}
	if store.recovery.MaxAttempts != 4 {
		t.Fatalf("RecoverExpired command = %#v, want max attempts 4", store.recovery)
	}
}
