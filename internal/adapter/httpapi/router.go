package httpapi

import (
	"io"
	"net/http"
	"strings"

	"github.com/Timofey121/fx-quotes/api"
)

func route(path string) (pattern, method string) {
	switch path {
	case "/v1/quote-updates":
		return path, http.MethodPost
	case "/v1/quotes/latest", "/health/live", "/health/ready", "/openapi.yaml":
		return path, http.MethodGet
	}
	if strings.HasPrefix(path, "/v1/quote-updates/") && !strings.Contains(strings.TrimPrefix(path, "/v1/quote-updates/"), "/") {
		return "/v1/quote-updates/{id}", http.MethodGet
	}
	return "unmatched", ""
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	pattern, method := route(r.URL.Path)
	if method == "" {
		writeProblem(w, 404, "not_found")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeProblem(w, 405, "method_not_allowed")
		return
	}
	switch pattern {
	case "/v1/quote-updates":
		h.create(w, r)
	case "/v1/quote-updates/{id}":
		h.getUpdate(w, r)
	case "/v1/quotes/latest":
		h.getLatest(w, r)
	case "/health/live":
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case "/health/ready":
		h.ready(w, r)
	case "/openapi.yaml":
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, api.OpenAPI())
	}
}
