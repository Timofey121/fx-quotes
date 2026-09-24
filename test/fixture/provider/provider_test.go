package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixtureReturnsRequestedPairAndExactRate(t *testing.T) {
	fixture := New(Config{Rate: "1.234567890123456789", SourceDate: "2026-09-22"})
	request := httptest.NewRequest(http.MethodGet, "/v2/providers/ecb/rate/USD/EUR", nil)
	response := httptest.NewRecorder()
	fixture.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, `"rate":1.234567890123456789`) || !strings.Contains(body, `"base":"USD"`) || !strings.Contains(body, `"quote":"EUR"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestFixtureConfiguresFailureWithoutPublicNetwork(t *testing.T) {
	fixture := New(Config{Status: http.StatusBadRequest})
	request := httptest.NewRequest(http.MethodGet, "/v2/providers/ecb/rate/USD/EUR", nil)
	response := httptest.NewRecorder()
	fixture.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
}
