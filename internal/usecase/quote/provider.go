package quote

import (
	"context"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

type RateProvider interface {
	FetchRate(context.Context, entity.Pair) (entity.Quote, error)
}

type FailureKind string

const (
	FailureTemporary FailureKind = "temporary"
	FailureThrottled FailureKind = "throttled"
	FailurePermanent FailureKind = "permanent"
)

// ProviderError классифицирует ошибки адаптера, не связывая сценарий
// с HTTP-клиентом или реализацией провайдера.
type ProviderError struct {
	Kind       FailureKind
	Code       entity.FailureCode
	RetryAfter time.Duration
	Err        error
}

func (err ProviderError) Error() string {
	if err.Err == nil {
		return string(err.Kind)
	}
	return err.Err.Error()
}

func (err ProviderError) Unwrap() error { return err.Err }
