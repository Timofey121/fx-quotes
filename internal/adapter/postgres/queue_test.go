package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

func TestConcurrentClaimsAndDueOrder(t *testing.T) {
	s, db := testStore(t, 20)
	for i := 0; i < 20; i++ {
		createUpdate(t, s, "")
	}
	// При одинаковом времени запуска задания выбираются по порядку приёма.
	if _, err := db.Exec(context.Background(), "UPDATE quote_updates SET next_attempt_at='2000-01-01'::timestamptz"); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := db.QueryRow(context.Background(), "SELECT id::text FROM quote_updates ORDER BY request_seq LIMIT 1").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if got := claimUpdate(t, s); got.ID != first {
		t.Fatalf("first=%s want=%s", got.ID, first)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := s.ClaimNext(context.Background(), quote.ClaimNextCommand{LeaseDuration: time.Minute})
			if errors.Is(err, quote.ErrNoWork) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			if u.Attempt != 1 || u.Status != entity.UpdateProcessing {
				t.Errorf("claim shape=%#v", u)
			}
			ids <- u.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{first: true}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate claim %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 20 {
		t.Fatalf("claimed %d jobs", len(seen))
	}
}

func TestRetryDelayAndFencing(t *testing.T) {
	s, db := testStore(t, 1)
	createUpdate(t, s, "")
	old := claimUpdate(t, s)
	ctx := context.Background()
	u, err := s.Retry(ctx, quote.RetryCommand{UpdateID: old.ID, Attempt: old.Attempt, RetryAfter: time.Hour})
	if err != nil || u.Status != entity.UpdatePending {
		t.Fatalf("retry=%#v %v", u, err)
	}
	_, err = s.ClaimNext(ctx, quote.ClaimNextCommand{LeaseDuration: time.Minute})
	if !errors.Is(err, quote.ErrNoWork) {
		t.Fatalf("early claim=%v", err)
	}
	_, _, err = s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR")})
	if !errors.Is(err, quote.ErrCapacityExceeded) {
		t.Fatalf("retry released capacity: %v", err)
	}
	if _, err = db.Exec(ctx, "UPDATE quote_updates SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1", old.ID); err != nil {
		t.Fatal(err)
	}
	current := claimUpdate(t, s)
	if current.Attempt != 2 {
		t.Fatalf("attempt=%d", current.Attempt)
	}
	_, err = s.Complete(ctx, quote.CompleteCommand{UpdateID: old.ID, Attempt: old.Attempt, Quote: fixtureQuote("0.9", "2026-09-22")})
	if !errors.Is(err, quote.ErrLostOwnership) {
		t.Fatalf("stale complete=%v", err)
	}
	_, err = s.Retry(ctx, quote.RetryCommand{UpdateID: old.ID, Attempt: old.Attempt})
	if !errors.Is(err, quote.ErrLostOwnership) {
		t.Fatalf("stale retry=%v", err)
	}
	_, err = s.Fail(ctx, quote.FailCommand{UpdateID: old.ID, Attempt: old.Attempt, Code: entity.FailureProviderPermanent})
	if !errors.Is(err, quote.ErrLostOwnership) {
		t.Fatalf("stale fail=%v", err)
	}
	_, err = s.GetLatest(ctx, quote.GetLatestQuery{Pair: old.Pair})
	if !errors.Is(err, quote.ErrNotFound) {
		t.Fatalf("stale completion wrote projection: %v", err)
	}
	completeUpdate(t, s, current, "0.9", "2026-09-22")
	_, err = s.Fail(ctx, quote.FailCommand{UpdateID: current.ID, Attempt: current.Attempt, Code: entity.FailureProviderPermanent})
	if !errors.Is(err, quote.ErrLostOwnership) {
		t.Fatalf("terminal mutation=%v", err)
	}
}

func TestCompletionOrderingAndTerminalReplay(t *testing.T) {
	for _, dates := range [][2]string{{"2026-09-22", "2026-09-22"}, {"2026-09-21", "2026-09-22"}, {"2026-09-23", "2026-09-22"}} {
		t.Run(fmt.Sprint(dates), func(t *testing.T) {
			store, _ := testStore(t, 2)
			createUpdate(t, store, "older")
			olderUpdate := claimUpdate(t, store)
			createUpdate(t, store, "newer")
			newerUpdate := claimUpdate(t, store)
			completeUpdate(t, store, newerUpdate, "0.123456789012345", dates[1])
			completedOlderUpdate := completeUpdate(t, store, olderUpdate, "999999999999999.999999999999999", dates[0])
			ctx := context.Background()
			latest, err := store.GetLatest(ctx, quote.GetLatestQuery{Pair: olderUpdate.Pair})
			if err != nil {
				t.Fatal(err)
			}
			want := "0.123456789012345"
			if dates[0] > dates[1] {
				want = "999999999999999.999999999999999"
			}
			if latest.Rate.String() != want || latest.UpdatedAt.Year() == 1999 || latest.UpdatedAt.Location() != time.UTC {
				t.Fatalf("latest=%#v rate=%s", latest, latest.Rate.String())
			}
			storedOlderUpdate, err := store.GetUpdate(ctx, quote.GetUpdateQuery{ID: olderUpdate.ID})
			if err != nil || storedOlderUpdate.Quote == nil || storedOlderUpdate.Quote.Rate.String() != "999999999999999.999999999999999" || !storedOlderUpdate.Quote.UpdatedAt.Equal(completedOlderUpdate.Quote.UpdatedAt) {
				t.Fatalf("storedOlderUpdate persisted result=%#v %v", storedOlderUpdate, err)
			}
			for _, state := range []string{"completed", "failed"} {
				key := "older"
				expected := olderUpdate.ID
				if state == "failed" {
					createUpdate(t, store, "failure")
					failedUpdate := claimUpdate(t, store)
					_, err = store.Fail(ctx, quote.FailCommand{UpdateID: failedUpdate.ID, Attempt: failedUpdate.Attempt, Code: entity.FailureProviderPermanent})
					if err != nil {
						t.Fatal(err)
					}
					key = "failure"
					expected = failedUpdate.ID
				}
				replayedUpdate, created, err := store.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: olderUpdate.Pair, IdempotencyKey: key})
				if err != nil || created || replayedUpdate.ID != expected || string(replayedUpdate.Status) != state {
					t.Fatalf("terminal replay=%#v %v %v", replayedUpdate, created, err)
				}
			}
			after, err := store.GetLatest(ctx, quote.GetLatestQuery{Pair: olderUpdate.Pair})
			if err != nil || after.Rate.String() != want {
				t.Fatalf("failure erased latest: %#v %v", after, err)
			}
		})
	}
}

func TestRecoveryUsesDatabaseLeaseAndAttemptLimit(t *testing.T) {
	s, db := testStore(t, 3)
	ctx := context.Background()
	createUpdate(t, s, "")
	a := claimUpdate(t, s)
	createUpdate(t, s, "")
	b := claimUpdate(t, s)
	createUpdate(t, s, "")
	live := claimUpdate(t, s)
	if _, err := db.Exec(ctx, "UPDATE quote_updates SET lease_until=clock_timestamp()-interval '1 second', attempt=CASE WHEN id=$1 THEN 3 ELSE attempt END WHERE id IN ($1,$2)", b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RecoverExpired(ctx, quote.RecoverExpiredCommand{MaxAttempts: 3})
	if err != nil || len(recovered) != 2 {
		t.Fatalf("recover=%#v %v", recovered, err)
	}
	for _, u := range recovered {
		if u.ID == a.ID && (u.Status != entity.UpdatePending || u.Attempt != 1) {
			t.Errorf("retry recovery=%#v", u)
		}
		if u.ID == b.ID && (u.Status != entity.UpdateFailed || u.FailureCode != entity.FailureAttemptsExhausted) {
			t.Errorf("final recovery=%#v", u)
		}
	}
	u, err := s.GetUpdate(ctx, quote.GetUpdateQuery{ID: live.ID})
	if err != nil || u.Status != entity.UpdateProcessing {
		t.Fatalf("unexpired=%#v %v", u, err)
	}
	if got := claimUpdate(t, s); got.ID != a.ID || got.Attempt != 2 {
		t.Fatalf("reclaim=%#v", got)
	}
}

func TestCompletionRollbackPreservesUpdateAndProjection(t *testing.T) {
	s, db := testStore(t, 2)
	ctx := context.Background()
	createUpdate(t, s, "")
	completeUpdate(t, s, claimUpdate(t, s), "0.8", "2026-09-21")
	createUpdate(t, s, "")
	u := claimUpdate(t, s)
	if _, err := db.Exec(ctx, "ALTER TABLE latest_quotes ADD CONSTRAINT test_projection_failure CHECK (rate < 1)"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Complete(ctx, quote.CompleteCommand{UpdateID: u.ID, Attempt: u.Attempt, Quote: fixtureQuote("2", "2026-09-22")})
	if err == nil {
		t.Fatal("expected projection constraint failure")
	}
	got, err := s.GetUpdate(ctx, quote.GetUpdateQuery{ID: u.ID})
	if err != nil || got.Status != entity.UpdateProcessing || got.Quote != nil {
		t.Fatalf("partial completion=%#v %v", got, err)
	}
	latest, err := s.GetLatest(ctx, quote.GetLatestQuery{Pair: u.Pair})
	if err != nil || latest.Rate.String() != "0.8" {
		t.Fatalf("projection changed=%#v %v", latest, err)
	}
}
