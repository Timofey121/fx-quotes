package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

func TestRecoveryShrinksBatchAfterTimeout(t *testing.T) {
	s, db := testStore(t, 1000)
	ctx := context.Background()
	_, err := db.Exec(ctx, `INSERT INTO quote_updates(id,pair,status,attempt,next_attempt_at,lease_until)
		SELECT md5(i::text)::uuid,'USD/EUR','processing',1,NULL,clock_timestamp()-interval '1 minute'
		FROM generate_series(1,1000) i;
		CREATE FUNCTION slow_recovery() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.02); RETURN NEW; END $$;
		CREATE TRIGGER slow_recovery BEFORE UPDATE ON quote_updates FOR EACH ROW EXECUTE FUNCTION slow_recovery()`)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 7; attempt++ {
		batch, err := s.RecoverExpired(ctx, quote.RecoverExpiredCommand{MaxAttempts: 3})
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			continue
		}
		if len(batch) == 0 {
			t.Fatal("no recovered rows")
		}
		var pending int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM quote_updates WHERE status='pending'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != len(batch) {
			t.Fatal("successful batch not committed")
		}
		return
	}
	t.Fatal("recovery repeatedly times out without reducing its batch")
}

func TestRecoveryMakesProgressWithLargeSlowBacklog(t *testing.T) {
	s, db := testStore(t, 10000)
	ctx := context.Background()
	_, err := db.Exec(ctx, `INSERT INTO quote_updates(id,pair,status,attempt,next_attempt_at,lease_until)
		SELECT md5(i::text)::uuid,'USD/EUR','processing',1,NULL,clock_timestamp()-interval '1 minute'
		FROM generate_series(1,10000) i`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `CREATE FUNCTION slow_recovery() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.001); RETURN NEW; END $$;
		CREATE TRIGGER slow_recovery BEFORE UPDATE ON quote_updates FOR EACH ROW EXECUTE FUNCTION slow_recovery()`)
	if err != nil {
		t.Fatal(err)
	}
	for batch := 1; batch <= 3; batch++ {
		recovered, err := s.RecoverExpired(ctx, quote.RecoverExpiredCommand{MaxAttempts: 3})
		if err != nil {
			t.Fatalf("batch %d made no progress: %v", batch, err)
		}
		if len(recovered) == 0 || len(recovered) > 100 {
			t.Fatalf("unbounded or empty batch: %d", len(recovered))
		}
		var pending int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM quote_updates WHERE status='pending'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != batch*len(recovered) {
			t.Fatalf("recovery not committed: pending=%d", pending)
		}
	}
	if _, err := db.Exec(ctx, `DROP TRIGGER slow_recovery ON quote_updates`); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		batch, err := s.RecoverExpired(deadline, quote.RecoverExpiredCommand{MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
	}
	var pending int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM quote_updates WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 10000 {
		t.Fatalf("backlog not drained: %d", pending)
	}
}

func TestCapacityTenThousand(t *testing.T) {
	s, db := testStore(t, 10000)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO quote_updates(id,pair) SELECT md5(i::text)::uuid,'USD/EUR' FROM generate_series(1,10000) i`); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR")})
	if !errors.Is(err, quote.ErrCapacityExceeded) {
		t.Fatalf("10000 active should reject admission: %v", err)
	}
	u := claimUpdate(t, s)
	if _, err = s.Fail(ctx, quote.FailCommand{UpdateID: u.ID, Attempt: u.Attempt, Code: entity.FailureProviderPermanent}); err != nil {
		t.Fatal(err)
	}
	_, created, err := s.CreateOrGet(ctx, quote.CreateOrGetCommand{Pair: mustPair("USD/EUR")})
	if err != nil || !created {
		t.Fatalf("released slot not admitted: created=%v err=%v", created, err)
	}
	var active int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM quote_updates WHERE status IN ('pending','processing')`).Scan(&active); err != nil || active != 10000 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}
