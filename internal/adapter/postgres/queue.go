package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
	"github.com/jackc/pgx/v5"
)

// Тайм-аут включает ожидание SQL-соединения. Более ранний дедлайн сохраняется.
const operationTimeout = time.Second

// Каждый пакет фиксируется отдельно, чтобы большой хвост не откатывался целиком.
const recoveryBatchSize = 100

func (store *Store) ClaimNext(ctx context.Context, command quote.ClaimNextCommand) (entity.QuoteUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	// Захват фиксируется этим запросом до возврата, поэтому вызов провайдера
	// не удерживает транзакцию или блокировку строки в БД.
	update, err := scanUpdate(store.pool.QueryRow(ctx, `UPDATE quote_updates
		SET status='processing', attempt=attempt+1, next_attempt_at=NULL,
		    lease_until=clock_timestamp()+$1::bigint*interval '1 microsecond'
		WHERE id=(SELECT id FROM quote_updates
		    WHERE status='pending' AND next_attempt_at <= statement_timestamp()
		    ORDER BY next_attempt_at, request_seq FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+updateColumns, command.LeaseDuration.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, quote.ErrNoWork
	}
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("claim update: %w", err)
	}
	return update, nil
}

func (store *Store) Complete(ctx context.Context, command quote.CompleteCommand) (entity.QuoteUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("begin completion: %w", err)
	}
	defer tx.Rollback(ctx)
	update, err := scanUpdate(tx.QueryRow(ctx, `UPDATE quote_updates
		SET status='completed', lease_until=NULL, finished_at=clock_timestamp(),
		    rate=$3::numeric, provider=$4, source_date=$5::date
		WHERE id=$1 AND status='processing' AND attempt=$2 AND pair=$6
		RETURNING `+updateColumns, command.UpdateID, command.Attempt, command.Quote.Rate.String(),
		command.Quote.Provider, command.Quote.SourceDate.Format("2006-01-02"), command.Quote.Pair.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, quote.ErrLostOwnership
	}
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("complete update: %w", err)
	}
	// request_seq задаёт порядок приёма, а не фиксации результата. Он сравнивается
	// только при одинаковой дате курса у провайдера.
	_, err = tx.Exec(ctx, `INSERT INTO latest_quotes (pair,update_id,request_seq,rate,provider,source_date,updated_at)
		SELECT pair,id,request_seq,rate,provider,source_date,finished_at FROM quote_updates WHERE id=$1
		ON CONFLICT (pair) DO UPDATE SET update_id=EXCLUDED.update_id,
		    request_seq=EXCLUDED.request_seq, rate=EXCLUDED.rate, provider=EXCLUDED.provider,
		    source_date=EXCLUDED.source_date, updated_at=EXCLUDED.updated_at
		WHERE (EXCLUDED.source_date,EXCLUDED.request_seq) > (latest_quotes.source_date,latest_quotes.request_seq)`, command.UpdateID)
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("advance latest quote: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("commit completion: %w", err)
	}
	return update, nil
}

func (store *Store) Retry(ctx context.Context, command quote.RetryCommand) (entity.QuoteUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	update, err := scanUpdate(store.pool.QueryRow(ctx, `UPDATE quote_updates
		SET status='pending', lease_until=NULL,
		    next_attempt_at=clock_timestamp()+$3::bigint*interval '1 microsecond'
		WHERE id=$1 AND status='processing' AND attempt=$2 RETURNING `+updateColumns,
		command.UpdateID, command.Attempt, command.RetryAfter.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, quote.ErrLostOwnership
	}
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("retry update: %w", err)
	}
	return update, nil
}

func (store *Store) Fail(ctx context.Context, command quote.FailCommand) (entity.QuoteUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	update, err := scanUpdate(store.pool.QueryRow(ctx, `UPDATE quote_updates
		SET status='failed', lease_until=NULL, finished_at=clock_timestamp(), failure_code=$3
		WHERE id=$1 AND status='processing' AND attempt=$2 RETURNING `+updateColumns,
		command.UpdateID, command.Attempt, string(command.Code)))
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.QuoteUpdate{}, quote.ErrLostOwnership
	}
	if err != nil {
		return entity.QuoteUpdate{}, fmt.Errorf("fail update: %w", err)
	}
	return update, nil
}

func (store *Store) RecoverExpired(ctx context.Context, command quote.RecoverExpiredCommand) ([]entity.QuoteUpdate, error) {
	batchSize := store.recoveryBatchSize.Load()
	updates, err := store.recoverExpiredBatch(ctx, command, batchSize)
	// Уменьшаем пакет только при собственном тайм-ауте, а не при остановке сервиса.
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && batchSize > 1 {
		store.recoveryBatchSize.CompareAndSwap(batchSize, max(1, batchSize/2))
	}
	return updates, err
}

func (store *Store) recoverExpiredBatch(ctx context.Context, command quote.RecoverExpiredCommand, batchSize int64) ([]entity.QuoteUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := store.pool.Query(ctx, `UPDATE quote_updates
		SET status=CASE WHEN attempt < $1 THEN 'pending' ELSE 'failed' END,
		    lease_until=NULL,
		    next_attempt_at=CASE WHEN attempt < $1 THEN clock_timestamp() END,
		    finished_at=CASE WHEN attempt >= $1 THEN clock_timestamp() END,
		    failure_code=CASE WHEN attempt >= $1 THEN 'attempts_exhausted' END
		WHERE id IN (SELECT id FROM quote_updates
		    WHERE status='processing' AND lease_until <= statement_timestamp()
		    ORDER BY lease_until, request_seq FOR UPDATE SKIP LOCKED LIMIT $2)
		RETURNING `+updateColumns, command.MaxAttempts, batchSize)
	if err != nil {
		return nil, fmt.Errorf("recover expired updates: %w", err)
	}
	defer rows.Close()
	var updates []entity.QuoteUpdate
	for rows.Next() {
		update, err := scanUpdate(rows)
		if err != nil {
			return nil, fmt.Errorf("decode recovered update: %w", err)
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recover expired updates: %w", err)
	}
	return updates, nil
}
