package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"ethanstewart.net/domainchecker/server/domainchecker"
)

const (
	LOG_PATH = "domainchecker-server.log"
)

// main starts the domain-checker server: registers the websocket handler
// and blocks serving HTTP on :8080 until ListenAndServe fails, at which
// point it exits non-zero. Logs at Info by default; switch to LevelDebug
// below to see heartbeat traffic. Logs to LOG_PATH if it can be opened,
// falling back to stderr otherwise -- but never stdout: stdout is Squid's
// external_acl_type line protocol (handleStdio's OK/ERR/BH replies to
// queries read from stdin), and Squid treats any other text appearing
// there as a corrupted response to whatever query it's currently waiting
// on. Squid also runs this as a managed helper process: if it can't start
// (as it couldn't when this used to exit on a log-open failure), Squid
// loses its only external_acl_type process and aborts entirely -- so
// losing LOG_PATH must degrade to stderr, never take the process down.
func main() {
	logWriter := io.Writer(os.Stderr)
	if logFile, err := os.OpenFile(LOG_PATH, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		// slog isn't set up yet, so this goes straight to stderr -- which,
		// like LOG_PATH, Squid never reads as part of the helper protocol.
		fmt.Fprintf(os.Stderr, "Failed to open log file %q, falling back to stderr: %v\n", LOG_PATH, err)
	} else {
		defer logFile.Close()
		logWriter = logFile
	}

	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	logger := slog.New(slog.NewTextHandler(logWriter, opts))
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
