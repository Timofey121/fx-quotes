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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/adapter/httpapi"
	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

const operationID = "6c71f3d0-30ba-4b12-90e3-a2f07eb8dc98"

func TestOpenAPIServesSource(t *testing.T) {
	w := request(router(&serviceStub{}), "GET", "/openapi.yaml", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("spec response: %d %v", w.Code, w.Header())
	}
	source, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), source) {
		t.Fatal("served specification differs from source")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing common headers")
	}
}

type serviceStub struct {
	update     entity.QuoteUpdate
	created    bool
	err        error
	panicValue bool
	commands   []quote.CreateUpdateCommand
	queries    []string
	committed  bool
}

func (s *serviceStub) CreateUpdate(_ context.Context, c quote.CreateUpdateCommand) (quote.CreateUpdateResult, error) {
	if s.panicValue {
		panic("secret upstream payload")
	}
	s.commands = append(s.commands, c)
	s.committed = s.err == nil
	return quote.CreateUpdateResult{Update: s.update, Created: s.created}, s.err
}
func (s *serviceStub) GetUpdate(_ context.Context, q quote.GetUpdateQuery) (entity.QuoteUpdate, error) {
	s.queries = append(s.queries, q.ID)
	return s.update, s.err
}
func (s *serviceStub) GetLatest(_ context.Context, q quote.GetLatestQuery) (entity.Quote, error) {
	s.queries = append(s.queries, q.Pair.String())
	return fixtureQuote(), s.err
}

type notifyFunc func()

func (f notifyFunc) Notify() { f() }

type readyFunc func(context.Context) error

func (f readyFunc) Ready(ctx context.Context) error { return f(ctx) }

func fixtureQuote() entity.Quote {
	pair, _ := entity.ParsePair("EUR/MXN")
	rate, _ := entity.ParseRate("123456789012345.123456789012345")
	return entity.Quote{Pair: pair, Rate: rate, Provider: "frankfurter", SourceDate: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 22, 15, 0, 0, 123456789, time.FixedZone("offset", 3*3600))}
}
func fixtureUpdate(status entity.UpdateStatus) entity.QuoteUpdate {
	q := fixtureQuote()
	u := entity.QuoteUpdate{
		ID: operationID, Pair: q.Pair, Status: status, Attempt: 7,
		CreatedAt: time.Date(2026, 9, 22, 14, 0, 0, 987654321, time.FixedZone("offset", 3*3600)),
	}
	if status == entity.UpdateCompleted {
		u.Quote = &q
	}
	if status == entity.UpdateFailed {
		u.FailureCode = entity.FailureAttemptsExhausted
	}
	return u
}
func router(s *serviceStub) http.Handler {
	return httpapi.New(s, nil, readyFunc(func(context.Context) error { return nil }), slog.New(slog.NewTextHandler(io.Discard, nil)))
}
func request(h http.Handler, method, path, body, media string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if media != "" {
		r.Header.Set("Content-Type", media)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func object(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %s: %v", w.Body.String(), err)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("missing common headers: %v", w.Header())
	}
	return body
}
func problem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	b := object(t, w)
	if len(b) != 4 || b["status"] != float64(status) || b["code"] != code || b["type"] != "urn:fx-quotes:problem:"+code || b["title"] == "" {
		t.Fatalf("unexpected problem: %#v", b)
	}
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("wrong media type: %v", w.Header())
	}
}

func TestCreateAndReplaySnapshots(t *testing.T) {
	for _, status := range []entity.UpdateStatus{entity.UpdatePending, entity.UpdateProcessing, entity.UpdateCompleted, entity.UpdateFailed} {
		for _, created := range []bool{false, true} {
			if created && status != entity.UpdatePending {
				continue
			}
			t.Run(fmt.Sprintf("%s/%t", status, created), func(t *testing.T) {
				s := &serviceStub{update: fixtureUpdate(status), created: created}
				notified := 0
				h := httpapi.New(s, notifyFunc(func() {
					if !s.committed {
						t.Error("notification before durable result")
					}
					notified++
				}), readyFunc(func(context.Context) error { return nil }), slog.New(slog.NewTextHandler(io.Discard, nil)))
				r := httptest.NewRequest("POST", "/v1/quote-updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
				r.Header.Set("Content-Type", "application/json; charset=utf-8")
				r.Header.Set("Idempotency-Key", "Opaque,Case-Sensitive!")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 200
				if created {
					want = 202
				}
				if w.Code != want || w.Header().Get("Location") != "/v1/quote-updates/"+operationID {
					t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body.String())
				}
				assertOperation(t, w, status)
				if len(s.commands) != 1 || s.commands[0].Pair.String() != "EUR/MXN" || s.commands[0].IdempotencyKey != "Opaque,Case-Sensitive!" {
					t.Fatalf("command: %+v", s.commands)
				}
				if (notified == 1) != created {
					t.Fatalf("notifications %d, created %t", notified, created)
				}
			})
		}
	}
}
func assertOperation(t *testing.T, w *httptest.ResponseRecorder, status entity.UpdateStatus) {
	t.Helper()
	b := object(t, w)
	if w.Header().Get("Content-Type") != "application/json" || b["id"] != operationID || b["pair"] != "EUR/MXN" || b["status"] != string(status) || b["created_at"] != "2026-09-22T11:00:00.987654321Z" {
		t.Fatalf("operation: %#v", b)
	}
	wantFields := 4
	if status == entity.UpdateCompleted {
		wantFields++
		assertQuote(t, b["quote"].(map[string]any))
	} else if _, ok := b["quote"]; ok {
		t.Fatal("quote outside completed")
	}
	if status == entity.UpdateFailed {
		wantFields++
		if b["failure_code"] != "attempts_exhausted" {
			t.Fatalf("failure: %#v", b)
		}
	} else if _, ok := b["failure_code"]; ok {
		t.Fatal("failure outside failed")
	}
	if len(b) != wantFields {
		t.Fatalf("unexpected public fields: %#v", b)
	}
}
func assertQuote(t *testing.T, b map[string]any) {
	t.Helper()
	if len(b) != 5 || b["pair"] != "EUR/MXN" || b["rate"] != "123456789012345.123456789012345" || b["provider"] != "frankfurter" || b["source_date"] != "2026-09-21" || b["updated_at"] != "2026-09-22T12:00:00.123456789Z" {
		t.Fatalf("quote: %#v", b)
	}
}
func TestGetStatesAndLatest(t *testing.T) {
	for _, status := range []entity.UpdateStatus{entity.UpdatePending, entity.UpdateProcessing, entity.UpdateCompleted, entity.UpdateFailed} {
		t.Run(string(status), func(t *testing.T) {
			s := &serviceStub{update: fixtureUpdate(status)}
			w := request(router(s), "GET", "/v1/quote-updates/"+operationID, "", "")
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			assertOperation(t, w, status)
			if len(s.queries) != 1 || s.queries[0] != operationID {
				t.Fatal(s.queries)
			}
		})
	}
	s := &serviceStub{}
	w := request(router(s), "GET", "/v1/quotes/latest?pair=EUR%2FMXN", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	assertQuote(t, object(t, w))
	if len(s.queries) != 1 || s.queries[0] != "EUR/MXN" {
		t.Fatal(s.queries)
	}
}

func TestInvalidStoredOperationNeverLeaksInternalFailure(t *testing.T) {
	for _, u := range []entity.QuoteUpdate{
		{ID: operationID, Status: entity.UpdateFailed, FailureCode: "secret upstream text"},
		{ID: operationID, Status: entity.UpdateCompleted},
		{ID: operationID, Status: "internal_status"},
	} {
		for _, method := range []string{"GET", "POST"} {
			path := "/v1/quote-updates"
			if method == "GET" {
				path += "/" + operationID
			}
			w := request(router(&serviceStub{update: u}), method, path, `{"pair":"EUR/MXN"}`, "application/json")
			problem(t, w, 500, "internal_error")
		}
	}
}
func TestStrictPOST(t *testing.T) {
	cases := []struct {
		name, body, media string
		status            int
		code              string
	}{
		{"unknown", `{"pair":"EUR/MXN","rate":"1"}`, "application/json", 400, "invalid_request"},
		{"trailing", `{"pair":"EUR/MXN"} {}`, "application/json", 400, "invalid_request"},
		{"garbage", `{"pair":"EUR/MXN"} junk`, "application/json", 400, "invalid_request"},
		{"null", `null`, "application/json", 400, "invalid_request"},
		{"array", `[]`, "application/json", 400, "invalid_request"},
		{"empty", "", "application/json", 400, "invalid_request"},
		{"missing", `{}`, "application/json", 400, "invalid_request"},
		{"null pair", `{"pair":null}`, "application/json", 400, "invalid_request"},
		{"case", `{"pair":"eur/mxn"}`, "application/json", 400, "invalid_request"},
		{"spaces", `{"pair":" EUR/MXN"}`, "application/json", 400, "invalid_request"},
		{"unsupported", `{"pair":"EUR/JPY"}`, "application/json", 400, "invalid_request"},
		{"same", `{"pair":"EUR/EUR"}`, "application/json", 400, "invalid_request"},
		{"case field", `{"Pair":"EUR/MXN"}`, "application/json", 400, "invalid_request"},
		{"duplicate field", `{"pair":"EUR/MXN","pair":"EUR/MXN"}`, "application/json", 400, "invalid_request"},
		{"wrong media", `{"pair":"EUR/MXN"}`, "text/plain", 415, "unsupported_media_type"},
		{"missing media", `{"pair":"EUR/MXN"}`, "", 415, "unsupported_media_type"},
		{"broken media", `{}`, "application/json;broken", 415, "unsupported_media_type"},
		{"oversize", `{"pair":"` + strings.Repeat("A", 1024) + `"}`, "application/json", 413, "payload_too_large"},
		{"oversize whitespace", `{"pair":"EUR/MXN"}` + strings.Repeat(" ", 1024), "application/json", 413, "payload_too_large"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := &serviceStub{}
			problem(t, request(router(s), "POST", "/v1/quote-updates", tt.body, tt.media), tt.status, tt.code)
			if len(s.commands) != 0 {
				t.Fatal("invalid input reached service")
			}
		})
	}
}
func TestCanonicalPairsAndBodyLimit(t *testing.T) {
	for _, pair := range []string{"EUR/MXN", "MXN/EUR", "EUR/USD", "USD/EUR", "USD/MXN", "MXN/USD"} {
		t.Run(pair, func(t *testing.T) {
			s := &serviceStub{update: fixtureUpdate(entity.UpdatePending), created: true}
			body := fmt.Sprintf(`{"pair":%q}`, pair)
			body += strings.Repeat(" ", 1024-len(body))
			w := request(router(s), "POST", "/v1/quote-updates", body, "application/json")
			if w.Code != 202 || len(s.commands) != 1 || s.commands[0].Pair.String() != pair {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestIdempotencyHeaderValidation(t *testing.T) {
	for _, keys := range [][]string{nil, {"A"}, {strings.Repeat("k", 128)}, {""}, {"a", "b"}, {"has space"}, {"tab\t"}, {"é"}, {strings.Repeat("k", 129)}, {"\x7f"}} {
		t.Run(fmt.Sprintf("%q", keys), func(t *testing.T) {
			s := &serviceStub{update: fixtureUpdate(entity.UpdatePending), created: true}
			r := httptest.NewRequest("POST", "/v1/quote-updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
			r.Header.Set("Content-Type", "application/json")
			for _, k := range keys {
				r.Header.Add("Idempotency-Key", k)
			}
			w := httptest.NewRecorder()
			router(s).ServeHTTP(w, r)
			valid := keys == nil || len(keys) == 1 && (keys[0] == "A" || len(keys[0]) == 128)
			if valid {
				if w.Code != 202 {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			} else {
				problem(t, w, 400, "invalid_request")
				if len(s.commands) != 0 {
					t.Fatal("invalid key reached service")
				}
			}
		})
	}
}
func TestQueryAndIDValidation(t *testing.T) {
	for _, path := range []string{"/v1/quotes/latest", "/v1/quotes/latest?pair=", "/v1/quotes/latest?pair=EUR%2FMXN&pair=EUR%2FMXN", "/v1/quotes/latest?pair=eur%2Fmxn", "/v1/quotes/latest?pair=EUR%2FMXN&extra=1", "/v1/quotes/latest?pair=%ZZ", "/v1/quotes/latest?pair=EUR%2FMXN;ignored=1", "/v1/quote-updates/nope", "/v1/quote-updates/6C71F3D0-30BA-4B12-90E3-A2F07EB8DC98", "/v1/quote-updates/6c71f3d030ba4b1290e3a2f07eb8dc98"} {
		t.Run(path, func(t *testing.T) {
			s := &serviceStub{}
			problem(t, request(router(s), "GET", path, "", ""), 400, "invalid_request")
			if len(s.queries) != 0 {
				t.Fatal("invalid input reached service")
			}
		})
	}
}
func TestApplicationErrors(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{{quote.ErrNotFound, 404, "not_found"}, {quote.ErrIdempotencyConflict, 409, "idempotency_conflict"}, {quote.ErrCapacityExceeded, 503, "queue_full"}, {quote.ErrInvalidIdempotencyKey, 400, "invalid_request"}, {errors.New("SQL secret password"), 503, "storage_unavailable"}, {quote.ErrNoWork, 503, "storage_unavailable"}, {quote.ErrLostOwnership, 503, "storage_unavailable"}} {
		t.Run(tt.code+tt.err.Error(), func(t *testing.T) {
			s := &serviceStub{err: fmt.Errorf("wrapped: %w", tt.err)}
			h := httpapi.New(s, notifyFunc(func() { t.Error("notified on failure") }), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			for _, route := range []struct{ method, path, body string }{{"POST", "/v1/quote-updates", `{"pair":"EUR/MXN"}`}, {"GET", "/v1/quote-updates/" + operationID, ""}, {"GET", "/v1/quotes/latest?pair=EUR%2FMXN", ""}} {
				w := request(h, route.method, route.path, route.body, "application/json")
				problem(t, w, tt.status, tt.code)
				if tt.code == "queue_full" && w.Header().Get("Retry-After") != "1" {
					t.Fatal("missing retry delay")
				}
			}
		})
	}
}
func TestHealthAndReadinessDeadline(t *testing.T) {
	called := 0
	ready := readyFunc(func(ctx context.Context) error {
		called++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("readiness is not bounded")
		}
		return errors.New("database secret")
	})
	h := httpapi.New(nil, nil, ready, nil)
	w := request(h, "GET", "/health/live", "", "")
	if w.Code != 200 || object(t, w)["status"] != "ok" || called != 0 {
		t.Fatal("liveness used dependency")
	}
	problem(t, request(h, "GET", "/health/ready", "", ""), 503, "storage_unavailable")
	if called != 1 {
		t.Fatal(called)
	}
	w = request(router(&serviceStub{}), "GET", "/health/ready", "", "")
	if w.Code != 200 || object(t, w)["status"] != "ok" {
		t.Fatal(w.Body.String())
	}
	problem(t, request(httpapi.New(nil, nil, nil, nil), "GET", "/health/ready", "", ""), 503, "storage_unavailable")
}
func TestRequestIDsLoggingAndRecovery(t *testing.T) {
	var logs bytes.Buffer
	s := &serviceStub{panicValue: true}
	h := httpapi.New(s, nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, ids := range [][]string{{"client-123"}, nil, {""}, {"bad id"}, {"one", "two"}, {strings.Repeat("r", 129)}, {"é"}} {
		r := httptest.NewRequest("POST", "/v1/quote-updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "secret-key")
		for _, id := range ids {
			r.Header.Add("X-Request-ID", id)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		problem(t, w, 500, "internal_error")
		got := w.Header().Get("X-Request-ID")
		if len(ids) == 1 && ids[0] == "client-123" {
			if got != "client-123" {
				t.Fatal(got)
			}
		} else if len(got) != 32 || strings.Contains(got, " ") {
			t.Fatalf("generated ID %q", got)
		}
	}
	w := request(h, "GET", "/health/live?secret-key=rate-secret", "", "")
	if w.Code != 200 {
		t.Fatal("server did not survive panic")
	}
	for _, secret := range []string{"secret-key", "secret upstream payload", "rate-secret", "EUR/MXN", "123456789012345"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaked %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"request_id":"client-123"`) || !strings.Contains(logs.String(), `"status":500`) {
		t.Fatal(logs.String())
	}
	for _, field := range []string{`"level":"ERROR"`, `"panic_type":"string"`, `"stack":`, "serviceStub).Create"} {
		if !strings.Contains(logs.String(), field) {
			t.Errorf("panic diagnostic missing %s", field)
		}
	}
}
func TestNotifierPanicDoesNotUndoAcceptance(t *testing.T) {
	h := httpapi.New(&serviceStub{update: fixtureUpdate(entity.UpdatePending), created: true}, notifyFunc(func() { panic("wakeup failure") }), nil, nil)
	w := request(h, "POST", "/v1/quote-updates", `{"pair":"EUR/MXN"}`, "application/json")
	if w.Code != 202 {
		t.Fatalf("durable create lost: %d %s", w.Code, w.Body.String())
	}
}
func TestMethodsAndUnknownRoutes(t *testing.T) {
	for _, tt := range []struct{ path, allow string }{{"/v1/quote-updates", "POST"}, {"/v1/quote-updates/" + operationID, "GET"}, {"/v1/quotes/latest", "GET"}, {"/health/live", "GET"}, {"/health/ready", "GET"}, {"/openapi.yaml", "GET"}} {
		for _, method := range []string{"PUT", "DELETE", "OPTIONS", "HEAD"} {
			t.Run(method+tt.path, func(t *testing.T) {
				w := request(router(&serviceStub{}), method, tt.path, "", "")
				problem(t, w, 405, "method_not_allowed")
				if w.Header().Get("Allow") != tt.allow {
					t.Fatal(w.Header())
				}
			})
		}
	}
	problem(t, request(router(&serviceStub{}), "GET", "/not-a-route", "", ""), 404, "not_found")
}
