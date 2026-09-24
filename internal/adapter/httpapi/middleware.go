package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"
)

// WithRequestTimeout ограничивает обработку запроса, включая ожидание БД.
// ReadTimeout и WriteTimeout сервера отдельно ограничивают сетевой ввод-вывод.
func WithRequestTimeout(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (h *handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		ids := r.Header.Values("X-Request-ID")
		id := ""
		if len(ids) == 1 && visibleASCII(ids[0]) {
			id = ids[0]
		} else {
			var randomBytes [16]byte
			_, _ = rand.Read(randomBytes[:])
			id = hex.EncodeToString(randomBytes[:])
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		tracked := &responseWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				h.logger.ErrorContext(r.Context(), "http handler panicked", "request_id", id,
					"panic_type", fmt.Sprintf("%T", recovered), "stack", string(debug.Stack()))
				if tracked.status == 0 {
					writeProblem(tracked, 500, "internal_error")
				}
			}
			pattern, _ := route(r.URL.Path)
			h.logger.InfoContext(r.Context(), "http request",
				"request_id", id,
				"method", r.Method,
				"route", pattern,
				"status", tracked.status,
				"duration", time.Since(started),
			)
		}()
		next.ServeHTTP(tracked, r)
	})
}
