package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

func TestAdmissionsAndReads(t *testing.T) {
	s, db := testStore(t, 3)
	a, b := createUpdate(t, s, ""), createUpdate(t, s, "")
	if a.ID == b.ID || len(a.ID) != 36 {
		t.Fatalf("IDs not distinct UUIDs: %q %q", a.ID, b.ID)
	}
	if a.Status != entity.UpdatePending || a.Attempt != 0 || a.CreatedAt.Location() != time.UTC {
		t.Fatalf("pending shape: %#v", a)
	}
	c := createUpdate(t, s, "Key")
	ctx := context.Background()
	for _, p := range []string{"USD/EUR", " usd / eur "} {
		u, created, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair(p), IdempotencyKey: "Key"})
		if err != nil || created || u.ID != c.ID {
			t.Fatalf("replay at capacity = %#v %v %v", u, created, err)
		}
	}
	_, _, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("EUR/USD"), IdempotencyKey: "Key"})
	if !errors.Is(err, quote.ErrIdempotencyConflict) {
		t.Fatalf("conflict = %v", err)
	}
	_, _, err = s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: "key"})
	if !errors.Is(err, quote.ErrCapacityExceeded) {
		t.Fatalf("case-sensitive new key = %v", err)
	}
	got, err := s.GetUpdate(ctx, quote.GetUpdateQuery{ID: a.ID})
	if err != nil || got.ID != a.ID {
		t.Fatalf("get = %#v %v", got, err)
	}
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		_, err = s.GetUpdate(ctx, quote.GetUpdateQuery{ID: id})
		if !errors.Is(err, quote.ErrNotFound) {
			t.Fatalf("missing %q = %v", id, err)
		}
	}
	_, err = s.GetLatest(ctx, quote.GetLatestQuery{Pair: mustPair("USD/EUR")})
	if !errors.Is(err, quote.ErrNotFound) {
		t.Fatalf("missing latest = %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM quote_updates").Scan(&count); err != nil || count != 3 {
		t.Fatalf("persisted count = %d %v", count, err)
	}
}

func TestConcurrentAdmissions(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			s, db := testStore(t, 5)
			// Независимые локальные ограничители должны соблюдать общий лимит в БД.
			stores := []*Store{s, NewStore(db, 5)}
			var wg sync.WaitGroup
			var created atomic.Int64
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					key := fmt.Sprint(i)
					if sameKey {
						key = "same"
					}
					_, fresh, err := stores[i%len(stores)].CreateOrGet(context.Background(), quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: key})
					if err != nil && (sameKey || !errors.Is(err, quote.ErrCapacityExceeded)) {
						t.Error(err)
					}
					if fresh {
						created.Add(1)
					}
				}(i)
			}
			wg.Wait()
			want := int64(5)
			if sameKey {
				want = 1
			}
			var count int64
			if err := db.QueryRow(context.Background(), "SELECT count(*) FROM quote_updates").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if created.Load() != want || count != want {
				t.Fatalf("created=%d rows=%d want=%d", created.Load(), count, want)
			}
		})
	}
}

func TestProcessingCapacityAndTerminalReplayAtCapacity(t *testing.T) {
	s, _ := testStore(t, 1)
	first := createUpdate(t, s, "done")
	u := claimUpdate(t, s)
	ctx := context.Background()
	_, _, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: u.Pair})
	if !errors.Is(err, quote.ErrCapacityExceeded) {
		t.Fatalf("processing must count as active: %v", err)
	}
	completeUpdate(t, s, u, "0.91", "2026-09-22")
	createUpdate(t, s, "occupies-capacity")
	got, fresh, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: u.Pair, IdempotencyKey: "done"})
	if err != nil || fresh || got.ID != first.ID || got.Status != entity.UpdateCompleted {
		t.Fatalf("terminal replay at capacity = %#v %v %v", got, fresh, err)
	}
}

func TestOperationalErrorsPreserveCause(t *testing.T) {
	s, _ := testStore(t, 1)
	u := createUpdate(t, s, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: u.Pair})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("admission cause=%v", err)
	}
	_, err = s.GetUpdate(ctx, quote.GetUpdateQuery{ID: u.ID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read cause=%v", err)
	}
	_, err = s.ClaimNext(ctx, quote.ClaimNextCommand{LeaseDuration: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("claim cause=%v", err)
	}
}

func TestIdempotencyKeyByteBoundaries(t *testing.T) {
	var large strings.Builder
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&large, "%x", sha256.Sum256([]byte(fmt.Sprint(i))))
	}
	for _, tc := range []struct {
		name, key string
		invalid   bool
	}{
		{"128 ASCII bytes", strings.Repeat("A", 128), false},
		{"129 ASCII bytes", strings.Repeat("A", 129), true},
		{"128 UTF-8 bytes", strings.Repeat("é", 64), false},
		{"129 UTF-8 bytes", strings.Repeat("é", 64) + "a", true},
		{"large incompressible", large.String(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := testStore(t, 2)
			ctx := context.Background()
			u, created, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: tc.key})
			var count int
			if countErr := db.QueryRow(ctx, "SELECT count(*) FROM quote_updates").Scan(&count); countErr != nil {
				t.Fatal(countErr)
			}
			if tc.invalid {
				if !errors.Is(err, quote.ErrInvalidIdempotencyKey) {
					t.Errorf("error=%v, want ErrInvalidIdempotencyKey", err)
				}
				if created || count != 0 {
					t.Fatalf("invalid input persisted: created=%v rows=%d", created, count)
				}
				// Отменённый контекст не получит соединение из пула. Ошибка валидации
				// должна вернуться раньше, поскольку проверка предшествует приёму.
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				_, _, err = s.CreateOrGet(canceled, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR"), IdempotencyKey: tc.key})
				if !errors.Is(err, quote.ErrInvalidIdempotencyKey) {
					t.Fatalf("validation did not precede database access: %v", err)
				}
				return
			}
			if err != nil || !created || count != 1 {
				t.Fatalf("accepted boundary=%#v %v %v rows=%d", u, created, err, count)
			}
			var storedKey string
			if err := db.QueryRow(ctx, "SELECT idempotency_key FROM quote_updates WHERE id=$1", u.ID).Scan(&storedKey); err != nil || storedKey != tc.key {
				t.Fatalf("stored key differs: %v", err)
			}
			replayed, fresh, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: u.Pair, IdempotencyKey: tc.key})
			if err != nil || fresh || replayed.ID != u.ID {
				t.Fatalf("boundary replay=%#v %v %v", replayed, fresh, err)
			}
		})
	}
}
