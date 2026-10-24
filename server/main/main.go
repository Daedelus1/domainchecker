package main

import (
	"log/slog"
	"net/http"
	"os"

	"ethanstewart.net/domainchecker/server/domainchecker"
)

// main starts the domain-checker server: registers the websocket handler
// and blocks serving HTTP on :8080 until ListenAndServe fails, at which
// point it exits non-zero. Logs at Info by default; switch to LevelDebug
// below to see heartbeat traffic.
func main() {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)

	domainCheckerSvc := domainchecker.NewDomainCheckerService()

	domainCheckerSvc.Start(os.Stdin, os.Stdout)

	http.Handle("/ws", http.HandlerFunc(domainCheckerSvc.HandleWebsocket))

	slog.Info("Main: HTTP Server starting on :8080/ws")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		slog.Error("Main: Error starting server", "error", err)
		os.Exit(1)
	}
}
