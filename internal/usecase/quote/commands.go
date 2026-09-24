package quote

import (
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

type CreateOrGetCommand struct {
	Pair           entity.Pair
	IdempotencyKey string
}

type CreateUpdateCommand struct {
	Pair           entity.Pair
	IdempotencyKey string
}

type CreateUpdateResult struct {
	Update  entity.QuoteUpdate
	Created bool
}

type GetUpdateQuery struct {
	ID string
}

type GetLatestQuery struct {
	Pair entity.Pair
}

type ClaimNextCommand struct {
	LeaseDuration time.Duration
}

type CompleteCommand struct {
	UpdateID string
	Attempt  int64
	Quote    entity.Quote
}

type RetryCommand struct {
	UpdateID   string
	Attempt    int64
	RetryAfter time.Duration
}

type FailCommand struct {
	UpdateID string
	Attempt  int64
	Code     entity.FailureCode
}

type RecoverExpiredCommand struct {
	MaxAttempts int64
}

type ProcessNextUpdateCommand struct {
	LeaseDuration time.Duration
}

type ProcessOutcome string

const (
	ProcessCompleted      ProcessOutcome = "completed"
	ProcessRetryScheduled ProcessOutcome = "retry_scheduled"
	ProcessFailed         ProcessOutcome = "failed"
	ProcessLost           ProcessOutcome = "lost"
)

type ProcessResult struct {
	Update     entity.QuoteUpdate
	Outcome    ProcessOutcome
	RetryAfter time.Duration
}
