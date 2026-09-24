package postgres

import (
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/jackc/pgx/v5"
)

func scanUpdate(row pgx.Row, extra ...any) (entity.QuoteUpdate, error) {
	var update entity.QuoteUpdate
	var pair string
	var rate, provider, failure *string
	var sourceDate, finishedAt *time.Time
	fields := []any{
		&update.ID,
		&pair,
		&update.Status,
		&update.Attempt,
		&rate,
		&provider,
		&sourceDate,
		&finishedAt,
		&failure,
		&update.CreatedAt,
	}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return update, err
	}
	var err error
	update.Pair, err = entity.ParsePair(pair)
	if err != nil {
		return entity.QuoteUpdate{}, err
	}
	update.CreatedAt = update.CreatedAt.UTC()
	if failure != nil {
		update.FailureCode = entity.FailureCode(*failure)
	}
	if rate != nil {
		parsedRate, err := entity.ParseRate(*rate)
		if err != nil {
			return entity.QuoteUpdate{}, err
		}
		update.Quote = &entity.Quote{
			Pair:       update.Pair,
			Rate:       parsedRate,
			Provider:   *provider,
			SourceDate: sourceDate.UTC(),
			UpdatedAt:  finishedAt.UTC(),
		}
	}
	return update, nil
}
