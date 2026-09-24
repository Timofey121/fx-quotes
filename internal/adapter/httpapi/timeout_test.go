package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestTimeoutCancelsApplicationWork(t *testing.T) {
	parent := context.Background()
	handler := WithRequestTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			if r.Context().Err() != context.DeadlineExceeded {
				t.Errorf("context error = %v", r.Context().Err())
			}
		case <-time.After(time.Second):
			t.Fatal("application work has no bounded deadline")
		}
	}), 10*time.Millisecond)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(parent))
	if parent.Err() != nil {
		t.Fatal("request deadline canceled parent context")
	}
}

func TestRequestTimeoutPreservesEarlierParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	handler := WithRequestTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := r.Context().Deadline()
		if !ok || !got.Equal(want) {
			t.Errorf("deadline = %v, want %v", got, want)
		}
	}), time.Minute)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
}
