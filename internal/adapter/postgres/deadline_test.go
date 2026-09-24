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

func TestCompletionBoundsLockWaitAndRollsBack(t *testing.T) {
	s, db := testStore(t, 3)
	createUpdate(t, s, "blocked-completion")
	u := claimUpdate(t, s)
	lock, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(context.Background(), "LOCK TABLE latest_quotes IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	_, err = s.Complete(ctx, quote.CompleteCommand{UpdateID: u.ID, Attempt: u.Attempt, Quote: fixtureQuote("0.9234", "2026-09-22")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("storage did not bound lock wait: %s", elapsed)
	}
	if ctx.Err() != nil {
		t.Error("storage waited for the caller's deadline")
	}
	if err = lock.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetUpdate(context.Background(), quote.GetUpdateQuery{ID: u.ID})
	if err != nil || stored.Status != entity.UpdateProcessing {
		t.Fatalf("partial completion survived rollback: %+v, %v", stored, err)
	}
	completeUpdate(t, s, u, "0.9234", "2026-09-22")
}

func TestRecoverySkipsLockedExpiredJobs(t *testing.T) {
	s, db := testStore(t, 3)
	a := createUpdate(t, s, "locked")
	b := createUpdate(t, s, "available")
	claimUpdate(t, s)
	claimUpdate(t, s)
	if _, err := db.Exec(context.Background(), "UPDATE quote_updates SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	lock, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(context.Background(), "SELECT id FROM quote_updates WHERE id=$1 FOR UPDATE", a.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	updates, err := s.RecoverExpired(ctx, quote.RecoverExpiredCommand{MaxAttempts: 3})
	if err != nil || len(updates) != 1 || updates[0].ID != b.ID {
		t.Fatalf("recovery blocked healthy work: %+v, %v", updates, err)
	}
	if err = lock.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	updates, err = s.RecoverExpired(context.Background(), quote.RecoverExpiredCommand{MaxAttempts: 3})
	if err != nil || len(updates) != 1 || updates[0].ID != a.ID {
		t.Fatalf("unlocked job was lost: %+v, %v", updates, err)
	}
}

func TestBackgroundOperationsBoundTableLockWaits(t *testing.T) {
	s, db := testStore(t, 2)
	u := createUpdate(t, s, "deadline")
	operations := map[string]func(context.Context) error{
		"claim": func(ctx context.Context) error {
			_, err := s.ClaimNext(ctx, quote.ClaimNextCommand{LeaseDuration: time.Minute})
			return err
		},
		"retry": func(ctx context.Context) error {
			_, err := s.Retry(ctx, quote.RetryCommand{UpdateID: u.ID, Attempt: 1})
			return err
		},
		"fail": func(ctx context.Context) error {
			_, err := s.Fail(ctx, quote.FailCommand{UpdateID: u.ID, Attempt: 1, Code: entity.FailureProviderPermanent})
			return err
		},
		"recover": func(ctx context.Context) error {
			_, err := s.RecoverExpired(ctx, quote.RecoverExpiredCommand{MaxAttempts: 3})
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			lock, err := db.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.Background())
			if _, err = lock.Exec(context.Background(), "LOCK TABLE quote_updates IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			started := time.Now()
			if err = operation(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("operation returned %v", err)
			}
			if time.Since(started) > 2*time.Second || ctx.Err() != nil {
				t.Fatal("background operation used only the caller deadline")
			}
		})
	}
}

func TestBlockedAdmissionsDoNotExhaustDatabaseConnections(t *testing.T) {
	s, db := testStore(t, 100)
	u := createUpdate(t, s, "readable")
	lock, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lock.Exec(context.Background(), "SELECT pg_advisory_xact_lock($1)", int64(0x46585141)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	defer func() { cancel(); _ = lock.Rollback(context.Background()); group.Wait() }()
	for n := 0; n < 24; n++ {
		group.Add(1)
		go func(n int) {
			defer group.Done()
			_, _, _ = s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: fmt.Sprintf("waiting-%d", n)})
		}(n)
	}
	// Даём ожидающим POST дойти до ограничителя приёма.
	deadline := time.Now().Add(300 * time.Millisecond)
	for db.Stat().AcquiredConns() < db.Config().MaxConns && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	readContext, readCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer readCancel()
	got, err := s.GetUpdate(readContext, quote.GetUpdateQuery{ID: u.ID})
	if err != nil || got.ID != u.ID {
		t.Fatalf("admission waiters starved an unrelated read: %+v, %v", got, err)
	}
	claimContext, claimCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer claimCancel()
	got, err = s.ClaimNext(claimContext, quote.ClaimNextCommand{LeaseDuration: time.Minute})
	if err != nil || got.ID != u.ID {
		t.Fatalf("admission waiters starved a worker: %+v, %v", got, err)
	}
}
