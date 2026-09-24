package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestStartupLogDoesNotExposeDatabaseCredentials(t *testing.T) {
	for name, dsn := range map[string]string{
		"userinfo":          "postgres://audit:CANARY_SECRET@localhost/db?connect_timeout=invalid",
		"password query":    "postgres://audit@localhost/db?password=CANARY_SECRET&connect_timeout=invalid",
		"sslpassword query": "postgres://audit@localhost/db?sslpassword=CANARY_SECRET&connect_timeout=invalid",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FX_QUOTES_DATABASE_URL", dsn)
			output, err := os.CreateTemp(t.TempDir(), "startup-log")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			original := os.Stdout
			os.Stdout = output
			defer func() { os.Stdout = original }()
			if code := run(); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			log := string(data)
			if strings.Contains(log, "CANARY_SECRET") {
				t.Error("startup log exposes database credentials")
			}
			if !strings.Contains(log, "application startup failed") {
				t.Error("startup failure is not logged")
			}
		})
	}
}

func TestFailureLogPreservesSafeClassification(t *testing.T) {
	for kind, cause := range map[string]error{"canceled": context.Canceled, "deadline_exceeded": context.DeadlineExceeded} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			logFailure(logger, "application stopped with error", fmt.Errorf("CANARY_SECRET: %w", cause))
			if strings.Contains(output.String(), "CANARY_SECRET") || !strings.Contains(output.String(), `"error_kind":"`+kind+`"`) {
				t.Fatalf("unsafe or unclassified diagnostic: %s", &output)
			}
		})
	}
}
