package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type server interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

type workerRunner interface{ Run(context.Context) }

// serve останавливает HTTP до отмены запросов провайдера и фоновых обработчиков.
func serve(ctx context.Context, shutdownTimeout time.Duration, httpServer server, workers workerRunner, stopping func()) error {
	if ctx == nil || httpServer == nil || workers == nil || shutdownTimeout <= 0 {
		return fmt.Errorf("lifecycle context, server, workers, and positive shutdown timeout are required")
	}
	workerContext, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	var workersDone sync.WaitGroup
	workersDone.Add(1)
	go func() {
		defer workersDone.Done()
		workers.Run(workerContext)
	}()
	listenerDone := make(chan error, 1)
	go func() { listenerDone <- httpServer.ListenAndServe() }()

	select {
	case err := <-listenerDone:
		_ = httpServer.Close()
		stopWorkers()
		workersDone.Wait()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP listener: %w", err)
	case <-ctx.Done():
		if stopping != nil {
			stopping()
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := httpServer.Shutdown(shutdownContext)
		cancel()
		if shutdownErr != nil {
			// По истечении дедлайна Shutdown оставляет активные соединения открытыми.
			// Close отменяет контексты их запросов до закрытия пула БД.
			shutdownErr = errors.Join(shutdownErr, httpServer.Close())
		}
		stopWorkers()
		workersDone.Wait()
		if shutdownErr != nil {
			return fmt.Errorf("HTTP shutdown: %w", shutdownErr)
		}
		return nil
	}
}
