package quote

import (
	"context"
	"errors"
	"fmt"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

type Service struct {
	store       Store
	provider    RateProvider
	maxAttempts int64
	retryPolicy RetryPolicy
}

func NewService(store Store, provider RateProvider, maxAttempts int, retryPolicy RetryPolicy) Service {
	return Service{store: store, provider: provider, maxAttempts: int64(maxAttempts), retryPolicy: retryPolicy}
}

func (service Service) CreateUpdate(ctx context.Context, command CreateUpdateCommand) (CreateUpdateResult, error) {
	if len(command.IdempotencyKey) > MaxIdempotencyKeyBytes {
		return CreateUpdateResult{}, ErrInvalidIdempotencyKey
	}
	update, created, err := service.store.CreateOrGet(ctx, CreateOrGetCommand{Pair: command.Pair, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return CreateUpdateResult{}, err
	}
	return CreateUpdateResult{Update: update, Created: created}, nil
}

func (service Service) GetUpdate(ctx context.Context, query GetUpdateQuery) (entity.QuoteUpdate, error) {
	return service.store.GetUpdate(ctx, query)
}

func (service Service) GetLatest(ctx context.Context, query GetLatestQuery) (entity.Quote, error) {
	return service.store.GetLatest(ctx, query)
}

func (service Service) ProcessNextUpdate(ctx context.Context, command ProcessNextUpdateCommand) (ProcessResult, error) {
	if service.provider == nil {
		return ProcessResult{}, fmt.Errorf("rate provider is required")
	}
	update, err := service.store.ClaimNext(ctx, ClaimNextCommand{LeaseDuration: command.LeaseDuration})
	if err != nil {
		return ProcessResult{}, err
	}
	obtainedQuote, err := service.provider.FetchRate(ctx, update.Pair)
	if ctx.Err() != nil {
		return ProcessResult{Update: update, Outcome: ProcessLost}, nil
	}
	if err != nil {
		return service.handleProviderFailure(ctx, update, err)
	}
	return service.complete(ctx, update, obtainedQuote)
}

func (service Service) complete(ctx context.Context, update entity.QuoteUpdate, obtainedQuote entity.Quote) (ProcessResult, error) {
	completed, err := service.store.Complete(ctx, CompleteCommand{
		UpdateID: update.ID,
		Attempt:  update.Attempt,
		Quote:    obtainedQuote,
	})
	if errors.Is(err, ErrLostOwnership) {
		return ProcessResult{Update: update, Outcome: ProcessLost}, nil
	}
	if err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{Update: completed, Outcome: ProcessCompleted}, nil
}

func (service Service) handleProviderFailure(ctx context.Context, update entity.QuoteUpdate, err error) (ProcessResult, error) {
	providerError, classified := providerFailure(err)
	retryable := classified && (providerError.Kind == FailureTemporary || providerError.Kind == FailureThrottled)
	if retryable && update.Attempt < service.maxAttempts {
		delay := service.retryPolicy.Delay(update.ID, update.Attempt, providerError.RetryAfter)
		retried, retryErr := service.store.Retry(ctx, RetryCommand{
			UpdateID:   update.ID,
			Attempt:    update.Attempt,
			RetryAfter: delay,
		})
		if errors.Is(retryErr, ErrLostOwnership) {
			return ProcessResult{Update: update, Outcome: ProcessLost}, nil
		}
		if retryErr != nil {
			return ProcessResult{}, retryErr
		}
		return ProcessResult{Update: retried, Outcome: ProcessRetryScheduled, RetryAfter: delay}, nil
	}
	failureCode := entity.FailureProviderPermanent
	if retryable {
		failureCode = entity.FailureAttemptsExhausted
	}
	if classified && providerError.Code != "" {
		failureCode = providerError.Code
	}
	failed, failErr := service.store.Fail(ctx, FailCommand{
		UpdateID: update.ID,
		Attempt:  update.Attempt,
		Code:     failureCode,
	})
	if errors.Is(failErr, ErrLostOwnership) {
		return ProcessResult{Update: update, Outcome: ProcessLost}, nil
	}
	if failErr != nil {
		return ProcessResult{}, failErr
	}
	return ProcessResult{Update: failed, Outcome: ProcessFailed}, nil
}

func (service Service) RecoverExpired(ctx context.Context) ([]entity.QuoteUpdate, error) {
	return service.store.RecoverExpired(ctx, RecoverExpiredCommand{MaxAttempts: service.maxAttempts})
}

func providerFailure(err error) (ProviderError, bool) {
	var pointer *ProviderError
	if errors.As(err, &pointer) {
		return *pointer, true
	}
	var value ProviderError
	if errors.As(err, &value) {
		return value, true
	}
	return ProviderError{}, false
}
