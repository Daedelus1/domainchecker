package client

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ethanstewart.net/domainchecker/shared"
	"ethanstewart.net/domainchecker/shared/stdinhandler"
	"ethanstewart.net/domainchecker/shared/websocketclient"
	"github.com/gorilla/websocket"
)

const (
	USER_INPUT_TIMEOUT_DURATION = time.Second * 10
)

type DomainCheckerClient struct {
	websocketClient *websocketclient.Client
	inboundMessages chan shared.InboundMessage
	stdinHandler    *stdinhandler.StdinHandler
	Shutdown        chan struct{}
	closeOnce       func()
	// sendMu serializes writes to websocketClient, since gorilla/websocket
	// allows only one writer at a time and handleMessages' heartbeat replies
	// can run concurrently with answerUrlRequest's own goroutine.
	sendMu sync.Mutex
	// memory holds "remember this" answers across requests. Zero value is
	// ready to use.
	memory decisionMemory
}

// NewDomainCheckerClient dials domainCheckerServiceURI and starts servicing
// it in the background, returning immediately once the connection is
// established. Exits the process if the dial itself fails. humanInput is
// where EvaluateUrlRequest reads y/n answers from -- normally os.Stdin,
// but callers (tests, in particular) can supply any io.Reader to script
// answers without a real terminal. memoryPath is where remembered
// decisions are loaded from on startup and saved to on every update; pass
// "" to keep memory in-process only (e.g. in tests, to avoid touching
// disk). The returned client's Shutdown channel closes once the
// connection ends, from either side; callers should still defer Close to
// release resources promptly on their own exit path.
func NewDomainCheckerClient(domainCheckerServiceURI string, origin string, humanInput io.Reader, memoryPath string) *DomainCheckerClient {
	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment,
	}
	header := make(http.Header)
	header.Set("Origin", origin)
	ws, _, err := dialer.Dial(domainCheckerServiceURI, header)
	if err != nil {
		slog.Error("Connection error", "error", err)
		os.Exit(1)
	}

	inboundMsgs := make(chan shared.InboundMessage)

	dcc := &DomainCheckerClient{
		inboundMessages: inboundMsgs,
		stdinHandler:    stdinhandler.New(humanInput),
		Shutdown:        make(chan struct{}),
	}
	if memoryPath != "" {
		dcc.memory.LoadFromFile(memoryPath)
	}
	dcc.closeOnce = sync.OnceFunc(func() {
		close(dcc.inboundMessages)
		close(dcc.Shutdown)
	})

	client := websocketclient.NewClient(nil, ws, func(msg shared.InboundMessage) {
		inboundMsgs <- msg
	}, func(id shared.ClientID) { dcc.Close() })
	client.Start()

	dcc.websocketClient = client

	go dcc.handleMessages()
	return dcc
}

// Close tears down the connection and releases dcc's channels. Safe to call
// more than once, including concurrently, and from both the owning code and
// the connection's own onClose callback.
func (dcc *DomainCheckerClient) Close() {
	dcc.closeOnce()
}

// sendJSON writes msg to the server through sendMu; callers must not call
// dcc.websocketClient.SendJSON directly.
func (dcc *DomainCheckerClient) sendJSON(msg any) error {
	dcc.sendMu.Lock()
	defer dcc.sendMu.Unlock()
	return dcc.websocketClient.SendJSON(msg)
}

// handleMessages dispatches every inbound message by type for the life of
// the connection, and only returns once inboundMessages is closed (i.e. the
// connection has ended) or a send fails. Must run on its own goroutine. A
// UrlRequest is handed off to answerUrlRequest on a separate goroutine
// rather than handled inline, so a human taking a while to answer one
// request can't delay this loop from acknowledging heartbeats in the
// meantime.
func (dcc *DomainCheckerClient) handleMessages() {
	for msg := range dcc.inboundMessages {
		parsedMsg := shared.ParseBinaryIntoMessageTypes(msg.Msg)

		switch v := parsedMsg.(type) {
		case shared.Heartbeat:
			slog.Debug("received heartbeat, responding...")
			err := dcc.sendJSON(shared.Heartbeat{})
			if err != nil {
				slog.Warn("Unable to send heartbeat reply, closing...", "err", err)
				dcc.websocketClient.Close()
				return
			}
		case shared.UrlResponse:
			slog.Warn("Unexpected Response from server", "Response", v)
		case shared.UrlRequest:
			slog.Debug("Recieved urlRequest", "request", v)
			go dcc.answerUrlRequest(v)

		case []byte:
			slog.Warn("Unexpected JSON: ", "msg", string(v))
		}
	}
}

// answerUrlRequest evaluates request and sends the resulting UrlResponse
// back, tagged with the same Attempt so the server can tell this answer
// apart from one for a since-superseded retry of the same RequestID.
// Intended to run on its own goroutine per request.
func (dcc *DomainCheckerClient) answerUrlRequest(request shared.UrlRequest) {
	allowed, denyMessage := dcc.EvaluateUrlRequest(request)
	err := dcc.sendJSON(shared.UrlResponse{
		Id:          request.Id,
		IsAccepted:  allowed,
		Attempt:     request.Attempt,
		DenyMessage: denyMessage,
	})
	if err != nil {
		slog.Warn("Unable to send reply, closing...", "err", err)
		dcc.websocketClient.Close()
	}
}

// EvaluateUrlRequest first checks dcc.memory for a remembered decision
// covering request.Url, returning it immediately without prompting if one
// exists. Otherwise it prompts on stdout and blocks until a human answers,
// or USER_INPUT_TIMEOUT_DURATION elapses; a timeout is treated as a
// one-off denial, never remembered. Discards any input line timestamped
// before the prompt was shown, since a previous call's reader may have
// left buffered input that hasn't been consumed yet. The second return
// value explains a denial (and whether it was remembered); it is always
// "" when the first return value is true.
func (dcc *DomainCheckerClient) EvaluateUrlRequest(request shared.UrlRequest) (bool, string) {
	if allowed, known := dcc.memory.Lookup(request.Url); known {
		fmt.Printf("Remembered decision for %q: %s\n", request.Url.String(), decisionWord(allowed))
		if allowed {
			return true, ""
		}
		return false, shared.DenyMessagePreviouslyRemembered
	}

	timeOfRequest := time.Now()

	// Created once so a stale buffered stdin message (filtered below) can't
	// restart the countdown by looping back into a fresh select.
	timer := time.NewTimer(USER_INPUT_TIMEOUT_DURATION)
	defer timer.Stop()

	fmt.Printf("Allow %q? (y/n, or yu/nu to remember this URL, yd/nd to remember this domain): ", request.Url.String())
	for {
		select {
		case msg, ok := <-dcc.stdinHandler.Stream():
			if !ok {
				slog.Error("Standard in handler has crashed")
			}
			if msg.Timestamp.Before(timeOfRequest) {
				continue
			}

			allowed, scope := parseAnswer(msg.Msg)
			if scope != decisionOnce {
				dcc.memory.Remember(request.Url, scope, allowed)
				fmt.Printf("(remembered: %s %s)\n", decisionWord(allowed), scopeDescription(scope))
			}
			if allowed {
				return true, ""
			}
			return false, denyMessageForScope(scope)
		case <-timer.C:
			fmt.Printf("\rAllow %q? (y/n): TIMED OUT\n", request.Url.String())
			return false, shared.DenyMessageTimeout
		}
	}
}

// parseAnswer interprets a human's raw answer to the Allow prompt.
// Anything unrecognized denies once, without being remembered -- the same
// fail-safe default a timeout gets.
func parseAnswer(raw string) (allowed bool, scope decisionScope) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "y":
		return true, decisionOnce
	case "yu":
		return true, decisionURL
	case "yd":
		return true, decisionDomain
	case "n":
		return false, decisionOnce
	case "nu":
		return false, decisionURL
	case "nd":
		return false, decisionDomain
	default:
		return false, decisionOnce
	}
}

func decisionWord(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}

func scopeDescription(scope decisionScope) string {
	switch scope {
	case decisionURL:
		return "this URL"
	case decisionDomain:
		return "this domain"
	default:
		return "once"
	}
}

// denyMessageForScope describes a fresh denial for DenyMessage, stating
// whether (and at what scope) it was remembered.
func denyMessageForScope(scope decisionScope) string {
	switch scope {
	case decisionURL:
		return shared.DenyMessageRememberedURL
	case decisionDomain:
		return shared.DenyMessageRememberedDomain
	default:
		return shared.DenyMessageOnce
	}
}
