package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/adapter/frankfurter"
	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

// Проверяем отмену контекста через обработчик, сценарий и настоящий HTTP-адаптер.
// Захваченное задание остаётся в хранилище для восстановления по истечении аренды.
func TestShutdownCancelsHTTPRequestAndLeavesClaimForRecovery(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	provider, err := frankfurter.New(server.Client(), frankfurter.Config{BaseURL: server.URL, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	store := &leaseStore{update: entity.QuoteUpdate{ID: "claimed-job", Pair: entity.Pair{Base: entity.USD, Quote: entity.EUR}, Status: entity.UpdatePending}}
	service := quote.NewService(store, provider, 3, quote.RetryPolicy{})
	pool, err := New(service, testConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background provider request never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not join workers")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider HTTP request not canceled")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.update.Status != entity.UpdateProcessing || store.update.Attempt != 1 || store.finished != 0 {
		t.Fatalf("claim after shutdown=%#v, completion calls=%d", store.update, store.finished)
	}
}

type leaseStore struct {
	quote.Store
	mu       sync.Mutex
	update   entity.QuoteUpdate
	finished int
}

func (s *leaseStore) ClaimNext(ctx context.Context, _ quote.ClaimNextCommand) (entity.QuoteUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return entity.QuoteUpdate{}, ctx.Err()
	}
	if s.update.Status != entity.UpdatePending {
		return entity.QuoteUpdate{}, quote.ErrNoWork
	}
	s.update.Status = entity.UpdateProcessing
	s.update.Attempt = 1
	return s.update, nil
}
func (s *leaseStore) RecoverExpired(context.Context, quote.RecoverExpiredCommand) ([]entity.QuoteUpdate, error) {
	return nil, nil
}
func (s *leaseStore) Complete(context.Context, quote.CompleteCommand) (entity.QuoteUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return s.update, nil
}
func (s *leaseStore) Retry(context.Context, quote.RetryCommand) (entity.QuoteUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return s.update, nil
}
func (s *leaseStore) Fail(context.Context, quote.FailCommand) (entity.QuoteUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return s.update, nil
}
