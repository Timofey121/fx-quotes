package app

import (
	"context"
	"errors"
	"sync/atomic"
)

var errNotReady = errors.New("application is not ready")

type pinger interface{ Ping(context.Context) error }

type readinessGate struct {
	storage pinger
	ready   atomic.Bool
}

func newReadinessGate(storage pinger) *readinessGate { return &readinessGate{storage: storage} }

func (gate *readinessGate) SetReady(ready bool) { gate.ready.Store(ready) }

func (gate *readinessGate) Ready(ctx context.Context) error {
	if !gate.ready.Load() {
		return errNotReady
	}
	return gate.storage.Ping(ctx)
}
