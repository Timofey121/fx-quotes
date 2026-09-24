package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Timofey121/fx-quotes/test/fixture/provider"
)

func main() {
	latency, err := time.ParseDuration(value("FX_QUOTES_FIXTURE_LATENCY", "0s"))
	if err != nil || latency < 0 {
		log.Fatal("FX_QUOTES_FIXTURE_LATENCY must be a non-negative duration")
	}
	status, err := strconv.Atoi(value("FX_QUOTES_FIXTURE_STATUS", "200"))
	if err != nil || status < 100 || status > 599 {
		log.Fatal("FX_QUOTES_FIXTURE_STATUS must be an HTTP status code")
	}
	server := &http.Server{
		Addr:              value("FX_QUOTES_FIXTURE_LISTEN_ADDR", ":8090"),
		Handler:           provider.New(provider.Config{Rate: value("FX_QUOTES_FIXTURE_RATE", "0.9234"), SourceDate: value("FX_QUOTES_FIXTURE_SOURCE_DATE", "2026-09-22"), Status: status, Latency: latency}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("fixture provider listening on %s\n", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func value(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
