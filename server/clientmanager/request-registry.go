package clientmanager

import (
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"ethanstewart.net/domainchecker/shared"
)

const (
	REQUEST_TIMEOUT_DURATION time.Duration = time.Second * 12
	REQUEST_QUEUE_SIZE       int           = 100
)

// allowedInfraDomains is the set of hosts that MakeRequest auto-approves
// without ever reaching a human or a client -- critical infrastructure a
// tool like Claude Code needs just to function (auth, API, telemetry), as
// opposed to anything it fetches as part of its actual work. Go has no
// constant sets, so this is a var; treat it as one anyway -- it isn't
// meant to be mutated at runtime. Keys are lowercase; lookups normalize
// the hostname before checking, since hostnames are case-insensitive.
//
// See README.md's "Exempting Claude Code's own infrastructure" for the
// full reasoning, and https://code.claude.com/docs/en/network-config for
// the authoritative source this list is drawn from.
//
// Keep this set to infra only. Do NOT add package registries,
// github.com, or anything else a tool reaches to do its actual work --
// that traffic is exactly what this whole project exists to put a human
// in front of; auto-approving it here defeats the point.
var allowedInfraDomains = map[string]struct{}{
	"api.anthropic.com": {},
	// Only needed for interactive OAuth/claude.ai login, not a static
	// ANTHROPIC_API_KEY -- uncomment if this deployment logs in that way.
	// "claude.ai":             {},
	// "claude.com":            {},
	// "platform.claude.com":   {},
	// "sso.claude.com":        {},
}

// isAllowedInfraDomain reports whether host (as returned by url.Hostname())
// is in allowedInfraDomains, case-insensitively.
func isAllowedInfraDomain(host string) bool {
	_, ok := allowedInfraDomains[strings.ToLower(host)]
	return ok
}

// clientDispatcher is the subset of ClientManager that RequestRegistry
// needs to find and talk to connected clients, kept narrow so tests can
// substitute an in-memory fake instead of a real websocket stack.
// *ClientManager satisfies it already.
type clientDispatcher interface {
	GetFirstClient() (shared.ClientID, bool)
	SendJSONToClient(id shared.ClientID, msg any) error
	RemoveClient(id shared.ClientID) error
	Responses() chan shared.UrlResponse
}

type RequestRegistry struct {
	clientManager       clientDispatcher
	haveConnectedClient bool
	currentClientId     shared.ClientID
	// InboundResponseChan aliases ClientManager.ResponseChan; it is owned and
	// closed by ClientManager, not by RequestRegistry.
	InboundResponseChan chan shared.UrlResponse
	requestChan         chan shared.UrlRequest
	retryRequests       chan shared.UrlRequest
	// connReset is currently a no-op: handleRequests signals it on a send
	// failure, but handleResponses only drains it without acting, so retry
	// timing today is governed entirely by REQUEST_TIMEOUT_DURATION.
	connReset chan time.Time

	// waiters tracks, per in-flight RequestID, the channel MakeRequest is
	// blocked on and which Attempt is currently live. A request that times
	// out gets resent under a new Attempt while the original prompt may
	// still be pending on the client, so a response is only delivered if
	// its Attempt matches the current one -- otherwise it's answering an
	// attempt that's already been superseded and is dropped instead of
	// racing to resolve the request.
	waitersMu sync.Mutex
	waiters   map[shared.RequestID]*pendingRequest

	lastID int
	IdMu   *sync.Mutex
}

// pendingRequest pairs a single MakeRequest call's response channel with
// the attempt number it's currently allowed to accept a response for.
type pendingRequest struct {
	respChan       chan shared.UrlResponse
	currentAttempt int
}

// NewRequestRegistry constructs a registry bound to c. Its initial
// currentClientId is whichever client (if any) happens to be connected to c
// at construction time. Start must be called before any request can be
// serviced, or MakeRequest will just queue requests that are never sent.
func (c *ClientManager) NewRequestRegistry() *RequestRegistry {
	return newRequestRegistry(c)
}

// newRequestRegistry is NewRequestRegistry's actual constructor, taking the
// clientDispatcher interface rather than a concrete *ClientManager so tests
// can pass an in-memory fake.
func newRequestRegistry(c clientDispatcher) *RequestRegistry {
	client, exists := c.GetFirstClient()
	return &RequestRegistry{
		clientManager:       c,
		haveConnectedClient: exists,
		currentClientId:     client,
		InboundResponseChan: c.Responses(),
		connReset:           make(chan time.Time, 10),
		requestChan:         make(chan shared.UrlRequest, REQUEST_QUEUE_SIZE),
		retryRequests:       make(chan shared.UrlRequest, REQUEST_QUEUE_SIZE),
		waiters:             make(map[shared.RequestID]*pendingRequest),
		lastID:              0,
		IdMu:                &sync.Mutex{},
	}
}

// Start launches the registry's two background loops -- dispatching queued
// requests to clients, and matching incoming responses back to waiters --
// and returns immediately. Must be called once, before MakeRequest is
// used; both loops run until Close.
func (r *RequestRegistry) Start() {
	go r.handleRequests()
	go r.handleResponses()
}

// Close shuts down the request and retry queues, which ends handleRequests.
// Must be called at most once. Does not close InboundResponseChan, which is
// owned by ClientManager -- so handleResponses only exits once that
// channel is separately closed by its owner. Deliberately does not close
// connReset either: handleResponses' read of it never checks for closure,
// so closing it would make that select case spuriously ready forever,
// spinning handleResponses instead of leaving it blocked.
func (r *RequestRegistry) Close() {
	close(r.requestChan)
	close(r.retryRequests)
}

// MakeRequest submits url for approval and blocks until a client answers
// the current attempt, or returns false immediately if no client is ever
// connected -- except for url.Hostname() in allowedInfraDomains, which is
// approved immediately without ever involving a human or a client. An
// attempt that goes unanswered for REQUEST_TIMEOUT_DURATION
// is retried under a new Attempt against whichever client is then current;
// only a response tagged with the attempt that's current at the moment it
// arrives can resolve the call, so a late answer to an already-superseded
// attempt is ignored rather than racing to decide the outcome. Safe to call
// concurrently; each call gets its own RequestID and waits independently.
// The second return value is the resolving UrlResponse's DenyMessage,
// verbatim; it is "" whenever the first return value is true.
func (r *RequestRegistry) MakeRequest(url url.URL) (bool, string) {
	slog.Debug("Received request, processing...", "url", url.String())

	if isAllowedInfraDomain(url.Hostname()) {
		slog.Debug("Auto-approved infra domain, skipping human review", "url", url.String())
		return true, ""
	}

	id := r.getNextId()
	request := shared.UrlRequest{
		Id:      id,
		Url:     url,
		Attempt: 1,
	}

	pending := &pendingRequest{
		respChan:       make(chan shared.UrlResponse, 1),
		currentAttempt: request.Attempt,
	}

	r.waitersMu.Lock()
	r.waiters[id] = pending
	r.waitersMu.Unlock()

	defer func() {
		r.waitersMu.Lock()
		delete(r.waiters, id)
		r.waitersMu.Unlock()
	}()

	r.requestChan <- request

	for {
		select {
		case response := <-pending.respChan:
			if request.Id != response.Id {
				slog.Error("RESPONSE ORDER ASSERTION BROKEN!", "req", request.Id, "resp", response.Id)
				return false, "internal error: response mismatch"
			}
			return response.IsAccepted, response.DenyMessage
		case <-time.After(REQUEST_TIMEOUT_DURATION):
			slog.Warn("Request timed out, requeuing for next client", "id", id)
			request.Attempt++
			r.waitersMu.Lock()
			pending.currentAttempt = request.Attempt
			r.waitersMu.Unlock()
			r.retryRequests <- request
			continue
		}
	}
}

// handleRequests runs for the life of the registry, serializing delivery of
// queued and retried requests to whichever client is currently considered
// connected. A send failure is treated as that client being dead -- it's
// removed, and the same request (unchanged) is retried against the next
// available client without involving MakeRequest's own attempt/timeout
// logic. If no client is connected at all, synthesizes an immediate
// rejection rather than leaving the caller to wait out the full timeout.
// Must run on its own goroutine, started via Start.
func (r *RequestRegistry) handleRequests() {
	for {
		var request shared.UrlRequest

		select {
		case req, ok := <-r.requestChan:
			if !ok {
				return
			}
			request = req
		case req, ok := <-r.retryRequests:
			if !ok {
				return
			}
			request = req
		}

		for {
			if !r.haveConnectedClient {
				r.currentClientId, r.haveConnectedClient = r.clientManager.GetFirstClient()
				if !r.haveConnectedClient {
					// Must not be a non-blocking send: handleResponses is
					// always draining InboundResponseChan, so this can only
					// block briefly, and a dropped notification would
					// otherwise leave MakeRequest waiting out the full
					// timeout instead of failing fast.
					r.InboundResponseChan <- shared.UrlResponse{Id: request.Id, IsAccepted: false, Attempt: request.Attempt, DenyMessage: shared.DenyMessageNoClient}
					break
				}
			}

			err := r.clientManager.SendJSONToClient(r.currentClientId, request)
			if err != nil {
				r.connReset <- time.Now()
				_ = r.clientManager.RemoveClient(r.currentClientId)
				r.currentClientId, r.haveConnectedClient = r.clientManager.GetFirstClient()
				continue
			}
			break
		}
	}
}

// handleResponses runs for the life of the registry, matching every
// inbound UrlResponse to the MakeRequest call waiting on its RequestID, if
// any. Drops (and logs) a response whose Attempt no longer matches the
// current attempt for that request, and silently drops a response for a
// RequestID nobody is waiting on (e.g. because MakeRequest already
// returned via its own timeout). Must run on its own goroutine, started
// via Start.
func (r *RequestRegistry) handleResponses() {
	for {
		select {
		case resp, ok := <-r.InboundResponseChan:
			if !ok {
				return
			}
			r.waitersMu.Lock()
			pending, exists := r.waiters[resp.Id]
			if exists && resp.Attempt != pending.currentAttempt {
				r.waitersMu.Unlock()
				slog.Warn("dropping response for superseded attempt", "id", resp.Id, "attempt", resp.Attempt, "current", pending.currentAttempt)
				continue
			}
			r.waitersMu.Unlock()
			if exists {
				pending.respChan <- resp
			}
		case <-r.connReset:
		}
	}
}

// getNextId returns a fresh, monotonically increasing RequestID. Safe for
// concurrent use.
func (r *RequestRegistry) getNextId() shared.RequestID {
	r.IdMu.Lock()
	defer r.IdMu.Unlock()
	r.lastID++
	return shared.RequestID(r.lastID)
}
