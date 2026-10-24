package main

import (
	"log/slog"
	"os"

	"ethanstewart.net/domainchecker/client"
)

const (
	SERVER_URI  = "ws://localhost:8080/ws"
	ORIGIN_URI  = "http://localhost/"
	MEMORY_PATH = "/tmp/domainchecker/domainchecker-memory.json"
)

// main connects to SERVER_URI and blocks until the connection is torn down
// (by either end), then runs cleanup and exits. Expects a human at the
// terminal: EvaluateUrlRequest prompts on stdout and reads answers from
// stdin via stdinhandler.
func main() {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)

	dcc := client.NewDomainCheckerClient(SERVER_URI, ORIGIN_URI, os.Stdin, MEMORY_PATH)
	defer dcc.Close()
	<-dcc.Shutdown
}
