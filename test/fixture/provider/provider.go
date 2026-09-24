// Пакет provider реализует управляемый сервер, совместимый с Frankfurter,
// для локальных проверок отказоустойчивости и нагрузки. Пакет не входит
// в зависимости рабочего приложения.
package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Rate       string
	SourceDate string
	Status     int
	Latency    time.Duration
}

// Fixture допускает безопасную смену настроек между запросами. Block нужен
// для проверки остановки: запрос ждёт закрытия канала или отмены
// своего контекста.
type Fixture struct {
	mu      sync.RWMutex
	config  Config
	block   <-chan struct{}
	started chan<- struct{}
}

func New(config Config) *Fixture {
	if config.Rate == "" {
		config.Rate = "0.9234"
	}
	if config.SourceDate == "" {
		config.SourceDate = "2026-09-22"
	}
	if config.Status == 0 {
		config.Status = http.StatusOK
	}
	return &Fixture{config: config}
}

func (fixture *Fixture) SetStatus(status int) {
	fixture.mu.Lock()
	fixture.config.Status = status
	fixture.mu.Unlock()
}

func (fixture *Fixture) SetLatency(latency time.Duration) {
	fixture.mu.Lock()
	fixture.config.Latency = latency
	fixture.mu.Unlock()
}

func (fixture *Fixture) SetBlock(block <-chan struct{}, started chan<- struct{}) {
	fixture.mu.Lock()
	fixture.block, fixture.started = block, started
	fixture.mu.Unlock()
}

func (fixture *Fixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/health/live" && request.Method == http.MethodGet {
		writer.WriteHeader(http.StatusOK)
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if request.Method != http.MethodGet || len(parts) != 6 || strings.Join(parts[:4], "/") != "v2/providers/ecb/rate" || parts[4] == "" || parts[5] == "" {
		http.NotFound(writer, request)
		return
	}
	fixture.mu.RLock()
	config, block, started := fixture.config, fixture.block, fixture.started
	fixture.mu.RUnlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-request.Context().Done():
			return
		}
	}
	if config.Latency > 0 {
		timer := time.NewTimer(config.Latency)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			return
		}
	}
	if config.Status != http.StatusOK {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(config.Status)
		_, _ = writer.Write([]byte(`{"error":"fixture status"}`))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(struct {
		Date  string      `json:"date"`
		Base  string      `json:"base"`
		Quote string      `json:"quote"`
		Rate  json.Number `json:"rate"`
	}{config.SourceDate, parts[4], parts[5], json.Number(config.Rate)})
}
