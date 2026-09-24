package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Timofey121/fx-quotes/internal/app"
	"github.com/Timofey121/fx-quotes/internal/app/config"
)

func main() { os.Exit(run()) }

func run() int {
	configuration, err := config.LoadFromLookup(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	level := slog.LevelInfo
	switch configuration.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.Build(ctx, configuration, logger)
	if err != nil {
		logFailure(logger, "application startup failed", err)
		return 1
	}
	if err = application.Run(ctx); err != nil {
		logFailure(logger, "application stopped with error", err)
		return 1
	}
	return 0
}

// Ошибка драйвера может раскрыть реквизиты БД даже после маскировки учётных данных в URI.
func logFailure(logger *slog.Logger, message string, err error) {
	kind := "internal"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	}
	root := err
	for errors.Unwrap(root) != nil {
		root = errors.Unwrap(root)
	}
	logger.Error(message, "error_kind", kind, "error_type", fmt.Sprintf("%T", root))
}
