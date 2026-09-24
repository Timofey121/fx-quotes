package httpapi

import (
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

type quoteDTO struct {
	Pair       string `json:"pair"`
	Rate       string `json:"rate"`
	Provider   string `json:"provider"`
	SourceDate string `json:"source_date"`
	UpdatedAt  string `json:"updated_at"`
}

type updateDTO struct {
	ID          string              `json:"id"`
	Pair        string              `json:"pair"`
	Status      entity.UpdateStatus `json:"status"`
	CreatedAt   string              `json:"created_at"`
	Quote       *quoteDTO           `json:"quote,omitempty"`
	FailureCode entity.FailureCode  `json:"failure_code,omitempty"`
}

func mapQuote(value entity.Quote) quoteDTO {
	return quoteDTO{
		Pair:       value.Pair.String(),
		Rate:       value.Rate.String(),
		Provider:   value.Provider,
		SourceDate: value.SourceDate.Format(time.DateOnly),
		UpdatedAt:  value.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func mapUpdate(update entity.QuoteUpdate) (updateDTO, bool) {
	result := updateDTO{
		ID:        update.ID,
		Pair:      update.Pair.String(),
		Status:    update.Status,
		CreatedAt: update.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	switch update.Status {
	case entity.UpdatePending, entity.UpdateProcessing:
	case entity.UpdateCompleted:
		if update.Quote == nil {
			return updateDTO{}, false
		}
		mappedQuote := mapQuote(*update.Quote)
		result.Quote = &mappedQuote
	case entity.UpdateFailed:
		if update.FailureCode != entity.FailureProviderPermanent && update.FailureCode != entity.FailureAttemptsExhausted {
			return updateDTO{}, false
		}
		result.FailureCode = update.FailureCode
	default:
		return updateDTO{}, false
	}
	return result, true
}
