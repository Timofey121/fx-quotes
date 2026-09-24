package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

type Service interface {
	CreateUpdate(context.Context, quote.CreateUpdateCommand) (quote.CreateUpdateResult, error)
	GetUpdate(context.Context, quote.GetUpdateQuery) (entity.QuoteUpdate, error)
	GetLatest(context.Context, quote.GetLatestQuery) (entity.Quote, error)
}

type Notifier interface{ Notify() }

type Readiness interface{ Ready(context.Context) error }

func New(service Service, notifier Notifier, readiness Readiness, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{service: service, notifier: notifier, readiness: readiness, logger: logger}
	return h.middleware(http.HandlerFunc(h.serveHTTP))
}

type handler struct {
	service   Service
	notifier  Notifier
	readiness Readiness
	logger    *slog.Logger
}

// Notifier не должен блокировать вызов. Потерю уведомления после сохранения
// задания компенсирует периодический опрос очереди.
func (h *handler) notify() {
	defer func() {
		if recover() != nil {
			h.logger.Warn("worker notification failed")
		}
	}()
	if h.notifier != nil {
		h.notifier.Notify()
	}
}

func (h *handler) getUpdate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/quote-updates/")
	if !uuidPattern.MatchString(id) {
		writeProblem(w, 400, "invalid_request")
		return
	}
	update, err := h.service.GetUpdate(r.Context(), quote.GetUpdateQuery{ID: id})
	if err != nil {
		writeError(w, err)
		return
	}
	body, ok := mapUpdate(update)
	if !ok {
		writeProblem(w, 500, "internal_error")
		return
	}
	writeJSON(w, 200, body)
}

func (h *handler) getLatest(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 1 || len(values["pair"]) != 1 {
		writeProblem(w, 400, "invalid_request")
		return
	}
	pair, ok := canonicalPair(values.Get("pair"))
	if !ok {
		writeProblem(w, 400, "invalid_request")
		return
	}
	latest, err := h.service.GetLatest(r.Context(), quote.GetLatestQuery{Pair: pair})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, mapQuote(latest))
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if h.readiness == nil || h.readiness.Ready(ctx) != nil {
		writeProblem(w, 503, "storage_unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(r.Header.Values("Content-Type")) != 1 {
		writeProblem(w, 415, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, 413, "payload_too_large")
		} else {
			writeProblem(w, 400, "invalid_request")
		}
		return
	}
	pair, ok := decodePair(body)
	if !ok {
		writeProblem(w, 400, "invalid_request")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	key := ""
	if len(keys) > 0 {
		if len(keys) != 1 || !visibleASCII(keys[0]) {
			writeProblem(w, 400, "invalid_request")
			return
		}
		key = keys[0]
	}
	result, err := h.service.CreateUpdate(r.Context(), quote.CreateUpdateCommand{Pair: pair, IdempotencyKey: key})
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusAccepted
		h.notify()
	}
	response, ok := mapUpdate(result.Update)
	if !ok {
		writeProblem(w, 500, "internal_error")
		return
	}
	w.Header().Set("Location", "/v1/quote-updates/"+result.Update.ID)
	writeJSON(w, status, response)
}
