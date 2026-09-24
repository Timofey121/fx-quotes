package frankfurter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

const validResponse = `{"date":"2026-09-21","base":"USD","quote":"EUR","rate":0.123456789012345,"future":{"field":true}}`

func testPair() entity.Pair { return entity.Pair{Base: entity.USD, Quote: entity.EUR} }

func newTestProvider(t *testing.T, server *httptest.Server, timeout time.Duration) *Provider {
	t.Helper()
	p, err := New(server.Client(), Config{BaseURL: server.URL, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchRateUsesVersionedECBRouteAndPreservesExactDecimal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.EscapedPath() != "/gateway%2Ftenant/v2/providers/ecb/rate/USD/EUR" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Accept") != "application/json" || !strings.Contains(r.UserAgent(), "fx-quotes") {
			t.Errorf("headers = %v", r.Header)
		}
		fmt.Fprint(w, validResponse)
	}))
	defer server.Close()
	p, err := New(server.Client(), Config{BaseURL: server.URL + "/gateway%2Ftenant/", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.FetchRate(context.Background(), testPair())
	if err != nil {
		t.Fatal(err)
	}
	if got.Rate.String() != "0.123456789012345" || got.Pair != testPair() || got.Provider != "frankfurter-ecb" || got.SourceDate.Format(time.DateOnly) != "2026-09-21" || !got.UpdatedAt.IsZero() {
		t.Fatalf("quote = %#v", got)
	}
}

func TestFetchRateRejectsInvalidOrInjectedPairWithoutSendingRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, validResponse) }))
	defer server.Close()
	p := newTestProvider(t, server, time.Second)
	for _, pair := range []entity.Pair{
		{Base: "USD/../MXN", Quote: entity.EUR}, {Base: "USD?secret=value", Quote: entity.EUR},
		{Base: "USD%2FMXN", Quote: entity.EUR}, {Base: entity.EUR, Quote: entity.EUR}, {},
	} {
		_, err := p.FetchRate(context.Background(), pair)
		assertFailure(t, err, quote.FailurePermanent)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid pair reached server %d times", calls.Load())
	}
}

func TestFetchRatePreservesAllThirtyDecimalDigits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Replace(validResponse, "0.123456789012345", "123456789012345.123456789012345", 1))
	}))
	defer server.Close()
	got, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
	if err != nil || got.Rate.String() != "123456789012345.123456789012345" {
		t.Fatalf("exact thirty-digit rate = %s, %v", got.Rate.String(), err)
	}
}

func TestFetchRateClassifiesHTTPStatusWithoutLeakingBody(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   quote.FailureKind
	}{
		{429, quote.FailureThrottled}, {500, quote.FailureTemporary}, {502, quote.FailureTemporary},
		{503, quote.FailureTemporary}, {504, quote.FailureTemporary}, {501, quote.FailurePermanent},
		{400, quote.FailurePermanent}, {401, quote.FailurePermanent}, {403, quote.FailurePermanent},
		{404, quote.FailurePermanent}, {408, quote.FailureTemporary}, {422, quote.FailurePermanent},
		{204, quote.FailurePermanent}, {302, quote.FailurePermanent},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "sensitive upstream detail")
			}))
			defer server.Close()
			_, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
			assertFailure(t, err, tc.kind)
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatal("provider error leaked upstream body")
			}
		})
	}
}

func TestFetchRateRejectsRedirectBeforeSendingAnotherRequest(t *testing.T) {
	for _, route := range []string{"cross-origin", "HTTPS-to-HTTP", "away-from-ECB"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", route, status), func(t *testing.T) {
				var redirected, callerRedirectChecks atomic.Int64
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redirected.Add(1)
					fmt.Fprint(w, validResponse)
				}))
				defer destination.Close()
				location := destination.URL + "/v2/providers/ecb/rate/USD/EUR"
				if route == "away-from-ECB" {
					location = "/v2/providers/other/rate/USD/EUR"
				}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v2/providers/ecb/rate/USD/EUR" {
						redirected.Add(1)
						fmt.Fprint(w, validResponse)
						return
					}
					http.Redirect(w, r, location, status)
				})
				var origin *httptest.Server
				if route == "HTTPS-to-HTTP" {
					origin = httptest.NewTLSServer(handler)
				} else {
					origin = httptest.NewServer(handler)
				}
				defer origin.Close()
				client := origin.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error { callerRedirectChecks.Add(1); return nil }
				provider, err := New(client, Config{BaseURL: origin.URL, Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				_, err = provider.FetchRate(context.Background(), testPair())
				if redirected.Load() != 0 {
					t.Errorf("sent %d requests to the redirect destination", redirected.Load())
				}
				if callerRedirectChecks.Load() != 0 {
					t.Errorf("adapter used caller redirect policy %d times", callerRedirectChecks.Load())
				}
				assertFailure(t, err, quote.FailurePermanent)
				// Запрет перенаправлений в адаптере не должен менять исходный общий клиент.
				response, err := client.Get(origin.URL + "/v2/providers/ecb/rate/USD/EUR")
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if redirected.Load() != 1 || callerRedirectChecks.Load() != 1 {
					t.Fatal("adapter changed the caller's redirect policy")
				}
			})
		}
	}
}

func TestFetchRateAcceptsOnlySupportedISODateYears(t *testing.T) {
	for _, tc := range []struct {
		date  string
		valid bool
	}{
		{"0000-09-21", false}, {"0001-09-21", true}, {"2026-09-21", true}, {"9999-12-31", true},
		{"10000-09-21", false}, {"-001-09-21", false}, {"2026-9-21", false}, {"2026-09-1", false},
	} {
		t.Run(tc.date, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.Replace(validResponse, "2026-09-21", tc.date, 1))
			}))
			defer server.Close()
			got, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
			if !tc.valid {
				assertFailure(t, err, quote.FailurePermanent)
				return
			}
			if err != nil || got.SourceDate.Format(time.DateOnly) != tc.date {
				t.Fatalf("source date=%s, error=%v", got.SourceDate, err)
			}
		})
	}
}

func TestFetchRateParsesAndClampsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		header   string
		min, max time.Duration
	}{
		{"12", 12 * time.Second, 12 * time.Second}, {"0", 0, 0}, {"-1", 0, 0}, {"garbage", 0, 0},
		{"1.5", 0, 0}, {"999999999999999999999999999", time.Minute, time.Minute},
		{time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat), 25 * time.Second, 30 * time.Second},
		{time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), time.Minute, time.Minute},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0, 0},
	} {
		t.Run(tc.header, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			_, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
			failure := assertFailure(t, err, quote.FailureThrottled)
			if failure.RetryAfter < tc.min || failure.RetryAfter > tc.max {
				t.Fatalf("delay=%s outside [%s,%s]", failure.RetryAfter, tc.min, tc.max)
			}
		})
	}
}

func TestFetchRateRejectsMalformedAndOversizedSuccess(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "truncated": `{"date":`, "trailing": validResponse + `{}`, "trailing garbage": validResponse + "oops",
		"array": `[]`, "null": `null`, "missing": `{}`, "invalid rate type": strings.Replace(validResponse, "0.123456789012345", "true", 1),
		"zero":                 strings.Replace(validResponse, "0.123456789012345", "0", 1),
		"negative":             strings.Replace(validResponse, "0.123456789012345", "-1", 1),
		"too precise":          strings.Replace(validResponse, "0.123456789012345", "0.1234567890123456", 1),
		"invalid date":         strings.Replace(validResponse, "2026-09-21", "2026-02-30", 1),
		"date time":            strings.Replace(validResponse, "2026-09-21", "2026-09-21T00:00:00Z", 1),
		"wrong base":           strings.Replace(validResponse, `"base":"USD"`, `"base":"MXN"`, 1),
		"wrong quote":          strings.Replace(validResponse, `"quote":"EUR"`, `"quote":"MXN"`, 1),
		"unsupported currency": strings.Replace(validResponse, `"base":"USD"`, `"base":"XYZ"`, 1),
		"oversized":            validResponse + strings.Repeat(" ", 64*1024),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			_, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
			assertFailure(t, err, quote.FailurePermanent)
		})
	}
}

func TestFetchRateTreatsTruncatedHTTPBodyAsTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, validResponse)
	}))
	defer server.Close()
	_, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
	assertFailure(t, err, quote.FailureTemporary)
}

func TestFetchRateRejectsQuotedJSONRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Replace(validResponse, "0.123456789012345", `"0.123456789012345"`, 1))
	}))
	defer server.Close()
	_, err := newTestProvider(t, server, time.Second).FetchRate(context.Background(), testPair())
	assertFailure(t, err, quote.FailurePermanent)
}

func TestFetchRateNetworkErrorsAreTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	p := newTestProvider(t, server, time.Second)
	server.Close()
	_, err := p.FetchRate(context.Background(), testPair())
	assertFailure(t, err, quote.FailureTemporary)
}

func TestFetchRateTimeoutAndParentCancellation(t *testing.T) {
	for _, duringBody := range []bool{false, true} {
		for _, cancelParent := range []bool{false, true} {
			t.Run(fmt.Sprintf("body=%t/cancel=%t", duringBody, cancelParent), func(t *testing.T) {
				started := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if duringBody {
						w.WriteHeader(http.StatusOK)
						fmt.Fprint(w, `{"date":`)
						w.(http.Flusher).Flush()
					}
					close(started)
					<-r.Context().Done()
				}))
				defer server.Close()
				timeout := 30 * time.Millisecond
				if cancelParent {
					timeout = time.Second
				}
				p := newTestProvider(t, server, timeout)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := p.FetchRate(ctx, testPair()); done <- err }()
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("request did not start")
				}
				if cancelParent {
					cancel()
				}
				select {
				case err := <-done:
					if cancelParent {
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("canceled parent = %v", err)
						}
						var classified *quote.ProviderError
						if errors.As(err, &classified) {
							t.Fatal("parent cancellation classified as provider failure")
						}
					} else {
						assertFailure(t, err, quote.FailureTemporary)
					}
				case <-time.After(time.Second):
					t.Fatal("request did not stop")
				}
			})
		}
	}
}

func TestFetchRateAlwaysClosesResponseBody(t *testing.T) {
	for _, body := range []string{validResponse, "bad", strings.Repeat("x", 65*1024)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		tracker := &trackingTransport{transport: server.Client().Transport}
		p, err := New(&http.Client{Transport: tracker}, Config{BaseURL: server.URL, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = p.FetchRate(context.Background(), testPair())
		server.Close()
		if !tracker.closed.Load() {
			t.Fatal("response body not closed")
		}
	}
}

func TestNewRejectsUnsafeOrIncompleteConfiguration(t *testing.T) {
	for _, base := range []string{"ftp://example.test", "/relative", "http://", "http://u:p@example.test", "http://example.test?token=x", "http://example.test#fragment", "http://example.test/%zz"} {
		if _, err := New(http.DefaultClient, Config{BaseURL: base, Timeout: time.Second}); err == nil {
			t.Fatalf("accepted base URL %q", base)
		}
	}
	if _, err := New(nil, Config{Timeout: time.Second}); err == nil {
		t.Fatal("accepted nil client")
	}
	if _, err := New(http.DefaultClient, Config{}); err == nil {
		t.Fatal("accepted unbounded timeout")
	}
}

func assertFailure(t *testing.T, err error, kind quote.FailureKind) *quote.ProviderError {
	t.Helper()
	var failure *quote.ProviderError
	if !errors.As(err, &failure) || failure.Kind != kind {
		t.Fatalf("failure = %v, want %s", err, kind)
	}
	if failure.Code != "" && failure.Code != entity.FailureProviderPermanent {
		t.Fatalf("unstable public failure code %q", failure.Code)
	}
	return failure
}

type trackingTransport struct {
	transport http.RoundTripper
	closed    atomic.Bool
}

func (d *trackingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := d.transport.RoundTrip(r)
	if err == nil {
		response.Body = &trackingBody{ReadCloser: response.Body, closed: &d.closed}
	}
	return response, err
}

type trackingBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (b *trackingBody) Close() error { b.closed.Store(true); return b.ReadCloser.Close() }
