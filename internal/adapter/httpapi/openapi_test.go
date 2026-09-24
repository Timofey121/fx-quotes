package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Timofey121/fx-quotes/internal/adapter/httpapi"
	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"go.yaml.in/yaml/v4"
)

func TestOpenAPIDuplicateMappingKeys(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		duplicate    bool
	}{
		{"root", "openapi: 3.1.0\nopenapi: 3.0.0\n", true},
		{"nested", "components:\n  schemas:\n    Problem:\n      properties:\n        status: {type: integer}\n        status: {type: string}\n", true},
		{"sequence", "parameters:\n  - name: pair\n    schema: {type: string, type: integer}\n", true},
		{"quoted", "properties: {status: 200, 'status': 500}\n", true},
		{"separate mappings", "first: {status: 200}\nsecond: {status: 500}\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var root yaml.Node
			if err := yaml.Unmarshal([]byte(tt.source), &root); err != nil {
				t.Fatal(err)
			}
			err := duplicateMappingKeys(&root)
			if (err != nil) != tt.duplicate {
				t.Fatalf("duplicate detection: error=%v, want duplicate=%t", err, tt.duplicate)
			}
		})
	}
}

func duplicateMappingKeys(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]int)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if line, exists := seen[key.Value]; exists {
				return fmt.Errorf("duplicate YAML key %q at line %d (first at line %d)", key.Value, key.Line, line)
			}
			seen[key.Value] = key.Line
		}
	}
	for _, child := range node.Content {
		if err := duplicateMappingKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func TestOpenAPIContract(t *testing.T) {
	source := request(router(&serviceStub{}), "GET", "/openapi.yaml", "", "")
	if source.Code != 200 {
		t.Fatalf("OpenAPI source unavailable: %d", source.Code)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(source.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if err := duplicateMappingKeys(&root); err != nil {
		t.Fatal(err)
	}
	doc, err := libopenapi.NewDocument(source.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	v, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if valid, errs := v.ValidateDocument(); !valid {
		t.Fatalf("invalid OpenAPI document: %+v", errs)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(source.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatal("contract must use OpenAPI 3.1.0")
	}
	for _, path := range []string{"/v1/quote-updates/{id}", "/v1/quotes/latest"} {
		op := spec["paths"].(map[string]any)[path].(map[string]any)["get"].(map[string]any)
		if _, exists := op["responses"].(map[string]any)["409"]; exists {
			t.Errorf("read-only %s declares an idempotency conflict", path)
		}
		requestPath := strings.ReplaceAll(path, "{id}", operationID)
		if path == "/v1/quotes/latest" {
			requestPath += "?pair=EUR%2FMXN"
		}
		r := httptest.NewRequest("GET", requestPath, nil)
		response := &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/problem+json"}, "X-Request-Id": []string{"study-001"}, "X-Content-Type-Options": []string{"nosniff"}, "Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(`{"type":"urn:fx-quotes:problem:queue_full","title":"Service Unavailable","status":503,"code":"queue_full"}`))}
		if valid, _ := v.ValidateHttpResponse(r, response); valid {
			t.Errorf("read-only %s accepts queue_full", path)
		}
	}
	create := spec["paths"].(map[string]any)["/v1/quote-updates"].(map[string]any)["post"].(map[string]any)
	if _, exists := create["responses"].(map[string]any)["404"]; exists {
		t.Error("create declares unreachable not_found")
	}

	type scenario struct {
		name, method, path, body, media string
		status                          int
		update                          entity.UpdateStatus
		created                         bool
		err                             error
		panicValue, unready             bool
	}
	cases := []scenario{
		{name: "accepted", method: "POST", path: "/v1/quote-updates", body: `{"pair":"EUR/MXN"}`, media: "application/json", status: 202, update: entity.UpdatePending, created: true},
		{name: "latest", method: "GET", path: "/v1/quotes/latest?pair=EUR%2FMXN", status: 200},
		{name: "liveness", method: "GET", path: "/health/live", status: 200},
		{name: "readiness", method: "GET", path: "/health/ready", status: 200},
		{name: "unready", method: "GET", path: "/health/ready", status: 503, unready: true},
		{name: "bad request", method: "POST", path: "/v1/quote-updates", body: `{}`, media: "application/json", status: 400},
		{name: "not found", method: "GET", path: "/v1/quote-updates/" + operationID, status: 404, err: quote.ErrNotFound},
		{name: "conflict", method: "POST", path: "/v1/quote-updates", body: `{"pair":"EUR/MXN"}`, media: "application/json", status: 409, err: quote.ErrIdempotencyConflict},
		{name: "capacity", method: "POST", path: "/v1/quote-updates", body: `{"pair":"EUR/MXN"}`, media: "application/json", status: 503, err: quote.ErrCapacityExceeded},
		{name: "storage", method: "GET", path: "/v1/quotes/latest?pair=EUR%2FMXN", status: 503, err: errors.New("secret SQL")},
		{name: "media", method: "POST", path: "/v1/quote-updates", body: `{}`, media: "text/plain", status: 415},
		{name: "large", method: "POST", path: "/v1/quote-updates", body: strings.Repeat("x", 1025), media: "application/json", status: 413},
		{name: "panic", method: "POST", path: "/v1/quote-updates", body: `{"pair":"EUR/MXN"}`, media: "application/json", status: 500, panicValue: true},
	}
	for _, state := range []entity.UpdateStatus{entity.UpdatePending, entity.UpdateProcessing, entity.UpdateCompleted, entity.UpdateFailed} {
		cases = append(cases, scenario{name: "get " + string(state), method: "GET", path: "/v1/quote-updates/" + operationID, status: 200, update: state}, scenario{name: "replay " + string(state), method: "POST", path: "/v1/quote-updates", body: `{"pair":"EUR/MXN"}`, media: "application/json", status: 200, update: state})
	}
	for _, path := range []string{"/v1/quote-updates", "/v1/quote-updates/" + operationID, "/v1/quotes/latest", "/health/live", "/health/ready", "/openapi.yaml"} {
		cases = append(cases, scenario{name: "method " + path, method: "DELETE", path: path, status: 405})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := &serviceStub{update: fixtureUpdate(tt.update), created: tt.created, err: tt.err, panicValue: tt.panicValue}
			h := httpapi.New(s, nil, readyFunc(func(context.Context) error {
				if tt.unready {
					return errors.New("unavailable")
				}
				return nil
			}), slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.media != "" {
				r.Header.Set("Content-Type", tt.media)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.status, w.Body.String())
			}
			selector := r.Clone(r.Context())
			// В OpenAPI ответ 405 описан у поддерживаемой операции маршрута.
			// По её схеме проверяем фактический ответ на неподдерживаемый метод.
			if tt.status == 405 {
				selector.Method = w.Header().Get("Allow")
			}
			if valid, errs := v.ValidateHttpResponse(selector, w.Result()); !valid {
				t.Fatalf("response violates contract: %+v; body=%s", errs, w.Body.String())
			}
			assertDeclaredHeadersAndMedia(t, spec, selector, w)
		})
	}
	// Невалидные примеры проверяют, что схема отклоняет неверные поля и формат курса.
	for _, body := range []string{
		`{"id":"6c71f3d0-30ba-4b12-90e3-a2f07eb8dc98","pair":"EUR/MXN","status":"pending","created_at":"2026-09-22T11:00:00Z","quote":{}}`,
		`{"id":"6c71f3d0-30ba-4b12-90e3-a2f07eb8dc98","pair":"EUR/MXN","status":"completed","created_at":"2026-09-22T11:00:00Z"}`,
		`{"id":"6c71f3d0-30ba-4b12-90e3-a2f07eb8dc98","pair":"EUR/MXN","status":"failed","created_at":"2026-09-22T11:00:00Z","failure_code":"secret SQL"}`,
		`{"id":"6c71f3d0-30ba-4b12-90e3-a2f07eb8dc98","pair":"EUR/MXN","status":"completed","created_at":"2026-09-22T11:00:00Z","quote":{"pair":"EUR/MXN","rate":18.5,"provider":"frankfurter","source_date":"2026-09-21","updated_at":"2026-09-22T12:00:00Z"}}`,
	} {
		r := httptest.NewRequest("GET", "/v1/quote-updates/"+operationID, nil)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.WriteString(body)
		if valid, _ := v.ValidateHttpResponse(r, w.Result()); valid {
			t.Fatalf("invalid operation accepted: %s", body)
		}
	}
	for _, rate := range []string{"0", "01", "1.0", "0.0000000000000001", "1000000000000000", "1e2"} {
		b := map[string]any{"pair": "EUR/MXN", "rate": rate, "provider": "frankfurter", "source_date": "2026-09-21", "updated_at": "2026-09-22T12:00:00Z"}
		data, _ := json.Marshal(b)
		r := httptest.NewRequest("GET", "/v1/quotes/latest?pair=EUR%2FMXN", nil)
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(data))}
		if valid, _ := v.ValidateHttpResponse(r, response); valid {
			t.Fatalf("noncanonical rate accepted: %s", rate)
		}
	}
}

func assertDeclaredHeadersAndMedia(t *testing.T, spec map[string]any, r *http.Request, w *httptest.ResponseRecorder) {
	t.Helper()
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/quote-updates/") {
		path = "/v1/quote-updates/{id}"
	}
	op := spec["paths"].(map[string]any)[path].(map[string]any)[strings.ToLower(r.Method)].(map[string]any)
	response, ok := op["responses"].(map[string]any)[strconv.Itoa(w.Code)]
	if !ok {
		t.Fatal("status absent from operation")
	}
	declared := resolve(spec, response.(map[string]any))
	media := w.Header().Get("Content-Type")
	if _, ok := declared["content"].(map[string]any)[media]; !ok {
		t.Fatalf("undeclared content type %s", media)
	}
	for name, value := range declared["headers"].(map[string]any) {
		header := resolve(spec, value.(map[string]any))
		if required, _ := header["required"].(bool); required && w.Header().Get(name) == "" {
			t.Errorf("missing required header %s", name)
		}
	}
	if strings.Contains(w.Body.String(), `"code":"queue_full"`) && w.Header().Get("Retry-After") == "" {
		t.Fatal("queue_full must include Retry-After")
	}
}
func resolve(spec, node map[string]any) map[string]any {
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	value := any(spec)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		value = value.(map[string]any)[part]
	}
	return value.(map[string]any)
}
