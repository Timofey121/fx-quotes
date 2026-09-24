package app

import (
	"context"
	"errors"
	"testing"
)

func TestReadinessGateRequiresStartupAndStorage(t *testing.T) {
	t.Parallel()
	storage := pingFunc(func(context.Context) error { return nil })
	gate := newReadinessGate(storage)
	if err := gate.Ready(context.Background()); !errors.Is(err, errNotReady) {
		t.Fatalf("Ready before startup = %v", err)
	}
	gate.SetReady(true)
	if err := gate.Ready(context.Background()); err != nil {
		t.Fatalf("Ready after startup = %v", err)
	}
	gate.SetReady(false)
	if err := gate.Ready(context.Background()); !errors.Is(err, errNotReady) {
		t.Fatalf("Ready after shutdown = %v", err)
	}
}

func TestReadinessGateReturnsStorageFailure(t *testing.T) {
	t.Parallel()
	storageFailure := errors.New("postgres unavailable")
	gate := newReadinessGate(pingFunc(func(context.Context) error { return storageFailure }))
	gate.SetReady(true)
	if err := gate.Ready(context.Background()); !errors.Is(err, storageFailure) {
		t.Fatalf("Ready() error = %v", err)
	}
}

type pingFunc func(context.Context) error

func (fn pingFunc) Ping(ctx context.Context) error { return fn(ctx) }
