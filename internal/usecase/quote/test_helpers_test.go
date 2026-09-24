package quote

import (
	"context"
	"fmt"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

type cancelingProvider struct {
	cancel context.CancelFunc
	quote  entity.Quote
}

func (p cancelingProvider) FetchRate(context.Context, entity.Pair) (entity.Quote, error) {
	p.cancel()
	return p.quote, nil
}

type stubProvider struct {
	quote entity.Quote
	err   error
}

func (provider stubProvider) FetchRate(context.Context, entity.Pair) (entity.Quote, error) {
	return provider.quote, provider.err
}

type memoryStore struct {
	createCalls     int
	updates         map[string]entity.QuoteUpdate
	latest          map[string]entity.Quote
	idempotency     map[string]string
	nextID          int
	capacityFull    bool
	lostOwnershipOn string
	recovery        RecoverExpiredCommand
}

func newMemoryStore() *memoryStore {
	return &memoryStore{updates: make(map[string]entity.QuoteUpdate), latest: make(map[string]entity.Quote), idempotency: make(map[string]string)}
}

func (store *memoryStore) addPending(pair entity.Pair) entity.QuoteUpdate {
	store.nextID++
	update := entity.QuoteUpdate{ID: fmt.Sprintf("update-%d", store.nextID), Pair: pair, Status: entity.UpdatePending, CreatedAt: time.Now().UTC()}
	store.updates[update.ID] = update
	return update
}

func (store *memoryStore) CreateOrGet(_ context.Context, command CreateOrGetCommand) (entity.QuoteUpdate, bool, error) {
	store.createCalls++
	if command.IdempotencyKey != "" {
		if updateID, ok := store.idempotency[command.IdempotencyKey]; ok {
			update := store.updates[updateID]
			if update.Pair != command.Pair {
				return entity.QuoteUpdate{}, false, ErrIdempotencyConflict
			}
			return update, false, nil
		}
	}
	if store.capacityFull {
		return entity.QuoteUpdate{}, false, ErrCapacityExceeded
	}
	update := store.addPending(command.Pair)
	if command.IdempotencyKey != "" {
		store.idempotency[command.IdempotencyKey] = update.ID
	}
	return update, true, nil
}

func (store *memoryStore) GetUpdate(_ context.Context, query GetUpdateQuery) (entity.QuoteUpdate, error) {
	update, ok := store.updates[query.ID]
	if !ok {
		return entity.QuoteUpdate{}, ErrNotFound
	}
	return update, nil
}

func (store *memoryStore) GetLatest(_ context.Context, query GetLatestQuery) (entity.Quote, error) {
	quote, ok := store.latest[query.Pair.String()]
	if !ok {
		return entity.Quote{}, ErrNotFound
	}
	return quote, nil
}

func (store *memoryStore) ClaimNext(_ context.Context, _ ClaimNextCommand) (entity.QuoteUpdate, error) {
	for id, update := range store.updates {
		if update.Status != entity.UpdatePending {
			continue
		}
		update.Status = entity.UpdateProcessing
		update.Attempt++
		store.updates[id] = update
		return update, nil
	}
	return entity.QuoteUpdate{}, ErrNoWork
}

func (store *memoryStore) Complete(_ context.Context, command CompleteCommand) (entity.QuoteUpdate, error) {
	if store.lostOwnershipOn == "complete" {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update, err := store.GetUpdate(context.Background(), GetUpdateQuery{ID: command.UpdateID})
	if err != nil {
		return entity.QuoteUpdate{}, err
	}
	if update.Status != entity.UpdateProcessing || update.Attempt != command.Attempt {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update.Status = entity.UpdateCompleted
	update.Quote = &command.Quote
	update.FailureCode = ""
	store.updates[update.ID] = update
	store.latest[update.Pair.String()] = command.Quote
	return update, nil
}

func (store *memoryStore) Retry(_ context.Context, command RetryCommand) (entity.QuoteUpdate, error) {
	if store.lostOwnershipOn == "retry" {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update, err := store.GetUpdate(context.Background(), GetUpdateQuery{ID: command.UpdateID})
	if err != nil {
		return entity.QuoteUpdate{}, err
	}
	if update.Status != entity.UpdateProcessing || update.Attempt != command.Attempt {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update.Status = entity.UpdatePending
	store.updates[update.ID] = update
	return update, nil
}

func (store *memoryStore) Fail(_ context.Context, command FailCommand) (entity.QuoteUpdate, error) {
	if store.lostOwnershipOn == "fail" {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update, err := store.GetUpdate(context.Background(), GetUpdateQuery{ID: command.UpdateID})
	if err != nil {
		return entity.QuoteUpdate{}, err
	}
	if update.Status != entity.UpdateProcessing || update.Attempt != command.Attempt {
		return entity.QuoteUpdate{}, ErrLostOwnership
	}
	update.Status = entity.UpdateFailed
	update.FailureCode = command.Code
	store.updates[update.ID] = update
	return update, nil
}

func (store *memoryStore) RecoverExpired(_ context.Context, command RecoverExpiredCommand) ([]entity.QuoteUpdate, error) {
	store.recovery = command
	return nil, nil
}
