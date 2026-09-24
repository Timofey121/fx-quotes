package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

type problemDTO struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeProblem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDTO{
		Type:   "urn:fx-quotes:problem:" + code,
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
	})
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, quote.ErrInvalidIdempotencyKey):
		writeProblem(w, 400, "invalid_request")
	case errors.Is(err, quote.ErrIdempotencyConflict):
		writeProblem(w, 409, "idempotency_conflict")
	case errors.Is(err, quote.ErrNotFound):
		writeProblem(w, 404, "not_found")
	case errors.Is(err, quote.ErrCapacityExceeded):
		w.Header().Set("Retry-After", "1")
		writeProblem(w, 503, "queue_full")
	default:
		writeProblem(w, 503, "storage_unavailable")
	}
}
