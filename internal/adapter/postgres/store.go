package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store хранит задания и последние успешные курсы. Вызывающий код управляет
// пулом соединений и запускает Migrate до первого обращения к хранилищу.
type Store struct {
	pool              *pgxpool.Pool
	maxActive         int
	admissionGate     chan struct{}
	recoveryBatchSize atomic.Int64
}

func NewStore(pool *pgxpool.Pool, maxActive int) *Store {
	store := &Store{pool: pool, maxActive: maxActive, admissionGate: make(chan struct{}, 1)}
	store.recoveryBatchSize.Store(recoveryBatchSize)
	return store
}

var _ quote.Store = (*Store)(nil)

const updateColumns = `id::text, pair, status, attempt, rate::text, provider, source_date, finished_at, failure_code, created_at`

func (store *Store) CreateOrGet(ctx context.Context, command quote.CreateOrGetCommand) (entity.QuoteUpdate, bool, error) {
	if len(command.IdempotencyKey) > quote.MaxIdempotencyKeyBytes {
		return entity.QuoteUpdate{}, false, quote.ErrInvalidIdempotencyKey
	}
	pair, err := entity.ParsePair(command.Pair.String())
	if err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("admit pair: %w", err)
	}
	// Ожидаем приёма до получения SQL-соединения, иначе очередь на общей
	// advisory lock может занять весь пул и задержать чтение и обработчики.
	select {
	case store.admissionGate <- struct{}{}:
		defer func() { <-store.admissionGate }()
	case <-ctx.Done():
		return entity.QuoteUpdate{}, false, fmt.Errorf("wait for admission: %w", ctx.Err())
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("begin admission: %w", err)
	}
	defer tx.Rollback(ctx)
	// Все реплики используют эту блокировку и одинаковый лимит очереди. Подсчёт
	// идёт после захвата блокировки, на новом снимке данных READ COMMITTED.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x46585141)); err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("lock admission: %w", err)
	}
	id, err := newUpdateID()
	if err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("generate update UUID: %w", err)
	}
	// Поиск повтора, проверка ёмкости и вставка выполняются одним запросом
	// под общей блокировкой. Запрос начинается после её захвата: перенос
	// блокировки в этот CTE может дать устаревший снимок READ COMMITTED.
	var created bool
	update, err := scanUpdate(tx.QueryRow(ctx, `WITH existing AS MATERIALIZED (
		SELECT * FROM quote_updates WHERE idempotency_key=NULLIF($3,'')
	), inserted AS (
		INSERT INTO quote_updates (id,pair,idempotency_key)
		SELECT $1,$2,NULLIF($3,'')
		WHERE NOT EXISTS (SELECT 1 FROM existing)
		  AND (SELECT count(*) FROM quote_updates WHERE status IN ('pending','processing')) < $4
		RETURNING *
	)
	SELECT `+updateColumns+`, true FROM inserted
	UNION ALL SELECT `+updateColumns+`, false FROM existing`, id, pair.String(), command.IdempotencyKey, store.maxActive), &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, false, quote.ErrCapacityExceeded
	}
	if err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("admit update: %w", err)
	}
	if update.Pair != pair {
		return entity.QuoteUpdate{}, false, quote.ErrIdempotencyConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return entity.QuoteUpdate{}, false, fmt.Errorf("commit admission: %w", err)
	}
	return update, created, nil
}

func (store *Store) GetUpdate(ctx context.Context, query quote.GetUpdateQuery) (entity.QuoteUpdate, error) {
	var id pgtype.UUID
	if err := id.Scan(query.ID); err != nil {
		return entity.QuoteUpdate{}, quote.ErrNotFound
	}
	update, err := scanUpdate(store.pool.QueryRow(ctx, "SELECT "+updateColumns+" FROM quote_updates WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, quote.ErrNotFound
	}
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("get update: %w", err)
	}
	return update, nil
}

func (store *Store) GetLatest(ctx context.Context, query quote.GetLatestQuery) (entity.Quote, error) {
	var latest entity.Quote
	var rate string
	row := store.pool.QueryRow(ctx,
		"SELECT rate::text,provider,source_date,updated_at FROM latest_quotes WHERE pair=$1",
		query.Pair.String(),
	)
	err := row.Scan(&rate, &latest.Provider, &latest.SourceDate, &latest.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return latest, quote.ErrNotFound
	}
	if err != nil {
		return latest, fmt.Errorf("get latest quote: %w", err)
	}
	latest.Pair = query.Pair
	latest.Rate, err = entity.ParseRate(rate)
	if err != nil {
		return entity.Quote{}, fmt.Errorf("decode latest rate: %w", err)
	}
	latest.SourceDate = latest.SourceDate.UTC()
	latest.UpdatedAt = latest.UpdatedAt.UTC()
	return latest, nil
}

func newUpdateID() (string, error) {
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	// UUID v4: версия в старших битах байта 6, вариант в байте 8.
	randomBytes[6] = (randomBytes[6] & 0x0f) | 0x40
	randomBytes[8] = (randomBytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		randomBytes[0:4], randomBytes[4:6], randomBytes[6:8], randomBytes[8:10], randomBytes[10:16],
	), nil
}
