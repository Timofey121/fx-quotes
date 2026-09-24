package entity

import (
	"time"
)

type UpdateStatus string

const (
	UpdatePending    UpdateStatus = "pending"
	UpdateProcessing UpdateStatus = "processing"
	UpdateCompleted  UpdateStatus = "completed"
	UpdateFailed     UpdateStatus = "failed"
)

type FailureCode string

const (
	FailureProviderPermanent FailureCode = "provider_permanent"
	FailureAttemptsExhausted FailureCode = "attempts_exhausted"
)

// QuoteUpdate хранит состояние операции. Переходы выполняет хранилище атомарно.
type QuoteUpdate struct {
	ID          string
	Pair        Pair
	Status      UpdateStatus
	Attempt     int64
	Quote       *Quote
	FailureCode FailureCode
	CreatedAt   time.Time
}
