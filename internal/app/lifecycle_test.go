package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeStopsHTTPAndWorkersWhenContextIsCanceled(t *testing.T) {
	server := &fakeServer{serve: make(chan struct{}), shutdown: make(chan struct{})}
	workers := &fakeWorkers{started: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, time.Second, server, workers, nil) }()
	<-workers.started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish")
	}
	select {
	case <-server.shutdown:
	default:
		t.Fatal("HTTP server was not shut down")
	}
	select {
	case <-workers.stopped:
	default:
		t.Fatal("workers were not canceled")
	}
}

func TestServeReturnsListenerFailure(t *testing.T) {
	listenerError := errors.New("bind failed")
	server := &fakeServer{serve: make(chan struct{}), listenErr: listenerError, shutdown: make(chan struct{})}
	workers := &fakeWorkers{started: make(chan struct{}), stopped: make(chan struct{})}
	err := serve(context.Background(), time.Second, server, workers, nil)
	if !errors.Is(err, listenerError) {
		t.Fatalf("Serve() error = %v, want listener failure", err)
	}
	select {
	case <-workers.stopped:
	default:
		t.Fatal("workers were not canceled")
	}
}

func TestServeRunsStoppingHookBeforeHTTPShutdown(t *testing.T) {
	server := &fakeServer{serve: make(chan struct{}), shutdown: make(chan struct{})}
	workers := &fakeWorkers{started: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped atomic.Bool
	server.stopping = &stopped
	done := make(chan error, 1)
	go func() { done <- serve(ctx, time.Second, server, workers, func() { stopped.Store(true) }) }()
	<-workers.started
	cancel()
	<-done
	if !stopped.Load() {
		t.Fatal("stopping hook was not called")
	}
	if server.shutdownBeforeHook.Load() {
		t.Fatal("HTTP shutdown ran before the stopping hook")
	}
}

func TestServeReturnsShutdownFailure(t *testing.T) {
	shutdownFailure := errors.New("drain timed out")
	server := &fakeServer{serve: make(chan struct{}), shutdown: make(chan struct{}), shutdownErr: shutdownFailure}
	workers := &fakeWorkers{started: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, time.Second, server, workers, nil) }()
	<-workers.started
	cancel()
	if err := <-done; !errors.Is(err, shutdownFailure) {
		t.Fatalf("Serve() error = %v, want shutdown failure", err)
	}
	if !server.closed.Load() {
		t.Fatal("timed-out shutdown must close active HTTP connections")
	}
}

type fakeServer struct {
	serve, shutdown        chan struct{}
	listenErr, shutdownErr error
	stopping               *atomic.Bool
	shutdownBeforeHook     atomic.Bool
	once                   sync.Once
	closed                 atomic.Bool
}

func (server *fakeServer) Close() error {
	server.closed.Store(true)
	server.once.Do(func() { close(server.shutdown) })
	return nil
}

func (server *fakeServer) ListenAndServe() error {
	close(server.serve)
	if server.listenErr != nil {
		return server.listenErr
	}
	<-server.shutdown
	return nil
}
func (server *fakeServer) Shutdown(context.Context) error {
	if server.stopping != nil && !server.stopping.Load() {
		server.shutdownBeforeHook.Store(true)
	}
	server.once.Do(func() { close(server.shutdown) })
	return server.shutdownErr
}

type fakeWorkers struct {
	started, stopped chan struct{}
	once             sync.Once
}

func (workers *fakeWorkers) Run(ctx context.Context) {
	close(workers.started)
	<-ctx.Done()
	workers.once.Do(func() { close(workers.stopped) })
}
