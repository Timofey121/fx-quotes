package worker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/Timofey121/fx-quotes/internal/entity"
	"github.com/Timofey121/fx-quotes/internal/usecase/quote"
)

type sqlFailure struct{}

func (sqlFailure) Error() string    { return "secret DSN and row data" }
func (sqlFailure) SQLState() string { return "55P03" }

func TestWorkerDiagnosticsClassifyErrorsWithoutLeakingSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		processor := fakeProcessor{
			process: func(context.Context, quote.ProcessNextUpdateCommand) (quote.ProcessResult, error) {
				return quote.ProcessResult{}, fmt.Errorf("secret DSN: %w", context.DeadlineExceeded)
			},
			recover: func(context.Context) ([]entity.QuoteUpdate, error) {
				return nil, fmt.Errorf("secret: %w", sqlFailure{})
			},
		}
		pool, err := New(processor, testConfig(), slog.New(slog.NewJSONHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := runPool(t, pool)
		synctest.Wait()
		stopped(t, cancel, done)
		for _, field := range []string{`"error_kind":"deadline_exceeded"`, `"sqlstate":"55P03"`, `"error_type":`} {
			if !strings.Contains(logs.String(), field) {
				t.Errorf("missing %s in %s", field, logs.String())
			}
		}
		if strings.Contains(logs.String(), "secret") {
			t.Fatal("diagnostic log leaked error payload")
		}
	})
}
