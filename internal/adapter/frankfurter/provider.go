package frankfurter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

const (
	DefaultBaseURL   = "https://api.frankfurter.dev"
	maxResponseBytes = 64 * 1024
	maxRetryAfter    = time.Minute
)

type Config struct {
	BaseURL string
	Timeout time.Duration
}

type Provider struct {
	client  *http.Client
	baseURL *url.URL
	timeout time.Duration
}

var _ quote.RateProvider = (*Provider)(nil)

// New копирует настройки клиента и запрещает переходы по перенаправлениям.
// Транспорт и пул соединений остаются общими с исходным клиентом.
func New(client *http.Client, config Config) (*Provider, error) {
	if client == nil || config.Timeout <= 0 {
		return nil, fmt.Errorf("HTTP client and positive request timeout are required")
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil || !validBaseURL(base) {
		return nil, fmt.Errorf("provider base URL must be an absolute HTTP URL without credentials, query, or fragment")
	}
	providerClient := *client
	providerClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Provider{client: &providerClient, baseURL: base, timeout: config.Timeout}, nil
}

func validBaseURL(base *url.URL) bool {
	return (base.Scheme == "http" || base.Scheme == "https") &&
		base.Host != "" && base.User == nil &&
		base.RawQuery == "" && !base.ForceQuery && base.Fragment == ""
}

func (provider *Provider) FetchRate(parent context.Context, pair entity.Pair) (entity.Quote, error) {
	if err := parent.Err(); err != nil {
		return entity.Quote{}, err
	}
	validated, err := entity.ParsePair(pair.String())
	if err != nil || validated != pair {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid currency pair")
	}
	ctx, cancel := context.WithTimeout(parent, provider.timeout)
	defer cancel()
	endpoint := provider.baseURL.JoinPath("v2", "providers", "ecb", "rate", string(pair.Base), string(pair.Quote))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid provider request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "fx-quotes/1.0")
	response, err := provider.client.Do(request)
	if err != nil {
		if parent.Err() != nil {
			return entity.Quote{}, parent.Err()
		}
		return entity.Quote{}, failure(quote.FailureTemporary, "provider request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if parent.Err() != nil {
		return entity.Quote{}, parent.Err()
	}
	if ctx.Err() != nil {
		return entity.Quote{}, failure(quote.FailureTemporary, "provider request timed out")
	}
	if len(body) > maxResponseBytes {
		return entity.Quote{}, failure(quote.FailurePermanent, "provider response exceeds size limit")
	}
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return entity.Quote{}, failure(quote.FailureTemporary, "truncated provider response")
		}
		return entity.Quote{}, failure(quote.FailureTemporary, "provider response read failed")
	}
	if response.StatusCode != http.StatusOK {
		return entity.Quote{}, responseFailure(response)
	}
	return decodeQuote(body, pair)
}

func responseFailure(response *http.Response) *quote.ProviderError {
	kind := quote.FailurePermanent
	switch response.StatusCode {
	case http.StatusTooManyRequests:
		kind = quote.FailureThrottled
	case http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = quote.FailureTemporary
	}
	err := failure(kind, "provider returned HTTP "+strconv.Itoa(response.StatusCode))
	if kind != quote.FailurePermanent {
		err.RetryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	return err
}

func decodeQuote(body []byte, pair entity.Pair) (entity.Quote, error) {
	var payload struct {
		Date  string `json:"date"`
		Base  string `json:"base"`
		Quote string `json:"quote"`
		Rate  any    `json:"rate"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid provider JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid provider JSON suffix")
	}
	if payload.Base != string(pair.Base) || payload.Quote != string(pair.Quote) {
		return entity.Quote{}, failure(quote.FailurePermanent, "provider pair mismatch")
	}
	number, ok := payload.Rate.(json.Number)
	if !ok {
		return entity.Quote{}, failure(quote.FailurePermanent, "provider rate must be a JSON number")
	}
	rate, err := entity.ParseRate(number.String())
	if err != nil {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid provider rate")
	}
	date, err := time.Parse(time.DateOnly, payload.Date)
	if err != nil || date.Year() < 1 || date.Format(time.DateOnly) != payload.Date {
		return entity.Quote{}, failure(quote.FailurePermanent, "invalid provider source date")
	}
	return entity.Quote{
		Pair:       pair,
		Rate:       rate,
		Provider:   "frankfurter-ecb",
		SourceDate: date,
	}, nil
}

func failure(kind quote.FailureKind, message string) *quote.ProviderError {
	return &quote.ProviderError{Kind: kind, Err: errors.New(message)}
}

func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	digits := true
	for _, r := range header {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(header, 10, 64)
		if err != nil || seconds >= uint64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(header); err == nil {
		return min(maxRetryAfter, max(0, date.Sub(now)))
	}
	return 0
}
