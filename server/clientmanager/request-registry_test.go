package clientmanager

import (
	"errors"
	"net/url"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"ethanstewart.net/domainchecker/shared"
)

// fakeDispatcher is an in-memory stand-in for ClientManager, used to drive
// RequestRegistry's dispatch/retry/attempt logic deterministically without
// any real websocket connections -- everything it does is either plain
// bookkeeping or a channel operation, so it imposes no un-virtualizable
// blocking on a synctest bubble.
type fakeDispatcher struct {
	mu      sync.Mutex
	clients []shared.ClientID
	sendErr map[shared.ClientID]error
	// failAfterCalls, if set for a client, delays sendErr for that client
	// until its call count reaches the given value (1-indexed) -- e.g. to
	// let an initial send succeed but a later retry to the same client
	// fail, instead of failing every call.
	failAfterCalls map[shared.ClientID]int
	callCount      map[shared.ClientID]int
	sent           []sentCall
	removed        []shared.ClientID
	responses      chan shared.UrlResponse
}

type sentCall struct {
	clientID shared.ClientID
	request  shared.UrlRequest
}

func newFakeDispatcher(clients ...shared.ClientID) *fakeDispatcher {
	return &fakeDispatcher{
		clients:        append([]shared.ClientID(nil), clients...),
		sendErr:        make(map[shared.ClientID]error),
		failAfterCalls: make(map[shared.ClientID]int),
		callCount:      make(map[shared.ClientID]int),
		responses:      make(chan shared.UrlResponse, 10),
	}
}

func (f *fakeDispatcher) GetFirstClient() (shared.ClientID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.clients) == 0 {
		return shared.ClientID{}, false
	}
	return f.clients[0], true
}

func (f *fakeDispatcher) SendJSONToClient(id shared.ClientID, msg any) error {
	req, ok := msg.(shared.UrlRequest)
	if !ok {
		return errors.New("fakeDispatcher: SendJSONToClient called with a non-UrlRequest message")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount[id]++
	f.sent = append(f.sent, sentCall{clientID: id, request: req})

	err, configured := f.sendErr[id]
	if !configured {
		return nil
	}
	if threshold, ok := f.failAfterCalls[id]; ok && f.callCount[id] < threshold {
		return nil
	}
	return err
}

func (f *fakeDispatcher) RemoveClient(id shared.ClientID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	for i, c := range f.clients {
		if c == id {
			f.clients = append(f.clients[:i], f.clients[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeDispatcher) Responses() chan shared.UrlResponse {
	return f.responses
}

func (f *fakeDispatcher) sentCalls() []sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentCall(nil), f.sent...)
}

func (f *fakeDispatcher) removedClients() []shared.ClientID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]shared.ClientID(nil), f.removed...)
}

// makeRequestResult carries MakeRequest's two return values over a
// channel, since a chan of a single bool can't.
type makeRequestResult struct {
	allowed     bool
	denyMessage string
}

func callMakeRequest(r *RequestRegistry, u url.URL) makeRequestResult {
	allowed, denyMessage := r.MakeRequest(u)
	return makeRequestResult{allowed, denyMessage}
}

func mustParseURL(t *testing.T, raw string) url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return *u
}

// startRegistry wires a RequestRegistry to f and starts it. Callers must
// defer shutdown (r.Close and closing f.responses) themselves rather than
// via t.Cleanup: cleanup functions run outside the synctest bubble, and
// operating on a bubble-associated channel from outside the bubble panics.
func startRegistry(f *fakeDispatcher) *RequestRegistry {
	r := newRequestRegistry(f)
	r.Start()
	return r
}

func shutdown(r *RequestRegistry, f *fakeDispatcher) {
	r.Close()
	close(f.responses)
}

func TestMakeRequestResolvesFromMatchingResponse(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run("accepted="+boolString(want), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
				f := newFakeDispatcher(client)
				r := startRegistry(f)
				defer shutdown(r, f)

				result := make(chan makeRequestResult, 1)
				go func() {
					result <- callMakeRequest(r, mustParseURL(t, "https://example.com"))
				}()

				synctest.Wait()

				sent := f.sentCalls()
				if len(sent) != 1 {
					t.Fatalf("sent %d requests, want 1", len(sent))
				}
				if sent[0].request.Attempt != 1 {
					t.Errorf("Attempt = %d, want 1", sent[0].request.Attempt)
				}

				wantMsg := ""
				if !want {
					wantMsg = "denied by test"
				}
				f.responses <- shared.UrlResponse{Id: sent[0].request.Id, IsAccepted: want, Attempt: 1, DenyMessage: wantMsg}

				got := <-result
				if got.allowed != want {
					t.Errorf("MakeRequest() = %v, want %v", got.allowed, want)
				}
				if got.denyMessage != wantMsg {
					t.Errorf("MakeRequest() denyMessage = %q, want %q", got.denyMessage, wantMsg)
				}
			})
		})
	}
}

func TestMakeRequestNoClientConnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeDispatcher()
		r := startRegistry(f)
		defer shutdown(r, f)

		allowed, denyMessage := r.MakeRequest(mustParseURL(t, "https://example.com"))

		if allowed {
			t.Errorf("MakeRequest() = true, want false when no client is connected")
		}
		if denyMessage == "" {
			t.Error("MakeRequest() denyMessage = \"\", want a reason")
		}
		if sent := f.sentCalls(); len(sent) != 0 {
			t.Errorf("sent %d requests, want 0", len(sent))
		}
	})
}

func TestMakeRequestRetriesAfterSendFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		good := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 1, 0, time.UTC))
		f := newFakeDispatcher(bad, good)
		f.sendErr[bad] = errors.New("write: broken pipe")
		r := startRegistry(f)
		defer shutdown(r, f)

		result := make(chan makeRequestResult, 1)
		go func() {
			result <- callMakeRequest(r, mustParseURL(t, "https://example.com"))
		}()

		synctest.Wait()

		sent := f.sentCalls()
		if len(sent) != 2 {
			t.Fatalf("sent %d requests, want 2 (failed send, then a retry against the next client)", len(sent))
		}
		if sent[0].clientID != bad || sent[1].clientID != good {
			t.Errorf("sent to %v then %v, want %v then %v", sent[0].clientID, sent[1].clientID, bad, good)
		}
		if sent[0].request.Attempt != 1 || sent[1].request.Attempt != 1 {
			t.Errorf("attempts = %d, %d, want 1, 1 -- a send-failure retry is not an Attempt bump", sent[0].request.Attempt, sent[1].request.Attempt)
		}
		if removed := f.removedClients(); len(removed) != 1 || removed[0] != bad {
			t.Errorf("removed clients = %v, want [%v]", removed, bad)
		}

		f.responses <- shared.UrlResponse{Id: sent[1].request.Id, IsAccepted: true, Attempt: 1}

		if got := <-result; !got.allowed {
			t.Errorf("MakeRequest() = false, want true")
		}
	})
}

func TestMakeRequestTimeoutRetriesWithNewAttemptAndDropsStaleResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		f := newFakeDispatcher(client)
		r := startRegistry(f)
		defer shutdown(r, f)

		result := make(chan makeRequestResult, 1)
		go func() {
			result <- callMakeRequest(r, mustParseURL(t, "https://example.com"))
		}()

		synctest.Wait()
		sent := f.sentCalls()
		if len(sent) != 1 {
			t.Fatalf("sent %d requests before timeout, want 1", len(sent))
		}
		firstAttempt := sent[0].request

		// Let the client stay silent until MakeRequest's own timeout fires
		// and requeues the request under a new attempt.
		time.Sleep(REQUEST_TIMEOUT_DURATION)
		synctest.Wait()

		sent = f.sentCalls()
		if len(sent) != 2 {
			t.Fatalf("sent %d requests after the timeout, want 2 (original + retry)", len(sent))
		}
		retryAttempt := sent[1].request
		if retryAttempt.Id != firstAttempt.Id {
			t.Errorf("retry Id = %v, want the same Id %v", retryAttempt.Id, firstAttempt.Id)
		}
		if retryAttempt.Attempt != firstAttempt.Attempt+1 {
			t.Errorf("retry Attempt = %d, want %d", retryAttempt.Attempt, firstAttempt.Attempt+1)
		}

		// A late answer to the now-superseded first attempt must not
		// resolve the call.
		f.responses <- shared.UrlResponse{Id: firstAttempt.Id, IsAccepted: false, Attempt: firstAttempt.Attempt}
		synctest.Wait()
		select {
		case got := <-result:
			t.Fatalf("MakeRequest() resolved to %v from a stale attempt's response, want it to still be waiting", got.allowed)
		default:
		}

		// The answer to the current attempt does resolve it.
		f.responses <- shared.UrlResponse{Id: retryAttempt.Id, IsAccepted: true, Attempt: retryAttempt.Attempt}

		if got := <-result; !got.allowed {
			t.Errorf("MakeRequest() = false, want true")
		}
	})
}

// TestMakeRequestTimeoutRetryCanAlsoFailoverToAnotherClient composes the
// two retry mechanisms exercised individually above: MakeRequest's own
// timeout bumps Attempt and requeues the request, and *that* retry's send
// also fails, which must trigger handleRequests' separate failover logic
// -- skipping past a second dead client -- while still carrying the
// bumped Attempt through every hop.
func TestMakeRequestTimeoutRetryCanAlsoFailoverToAnotherClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		b := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 1, 0, time.UTC))
		c := shared.ClientID(time.Date(2024, 1, 1, 0, 0, 2, 0, time.UTC))
		f := newFakeDispatcher(a, b, c)
		// a's first send (the original attempt) succeeds, but a has gone
		// dead by the time the timeout-triggered retry resends to it.
		f.sendErr[a] = errors.New("write: broken pipe")
		f.failAfterCalls[a] = 2
		// b is dead outright, so failover must skip past it too.
		f.sendErr[b] = errors.New("write: connection reset")
		r := startRegistry(f)
		defer shutdown(r, f)

		result := make(chan makeRequestResult, 1)
		go func() {
			result <- callMakeRequest(r, mustParseURL(t, "https://example.com"))
		}()

		synctest.Wait()
		sent := f.sentCalls()
		if len(sent) != 1 || sent[0].clientID != a || sent[0].request.Attempt != 1 {
			t.Fatalf("initial send = %+v, want one send to %v at attempt 1", sent, a)
		}
		requestID := sent[0].request.Id

		// Let MakeRequest's own timeout fire and requeue under Attempt 2.
		time.Sleep(REQUEST_TIMEOUT_DURATION)
		synctest.Wait()

		sent = f.sentCalls()
		wantClients := []shared.ClientID{a, a, b, c}
		if len(sent) != len(wantClients) {
			t.Fatalf("sent %d requests after the timeout, want %d (original to a, failed retry to a, failed failover to b, successful failover to c)", len(sent), len(wantClients))
		}
		for i, s := range sent {
			if s.clientID != wantClients[i] {
				t.Errorf("send %d went to %v, want %v", i, s.clientID, wantClients[i])
			}
			if s.request.Id != requestID {
				t.Errorf("send %d Id = %v, want the same Id %v throughout", i, s.request.Id, requestID)
			}
		}
		for i := 1; i < len(sent); i++ {
			if sent[i].request.Attempt != 2 {
				t.Errorf("send %d Attempt = %d, want 2 -- the timeout-bumped attempt must carry through every failover hop", i, sent[i].request.Attempt)
			}
		}

		if removed := f.removedClients(); len(removed) != 2 || removed[0] != a || removed[1] != b {
			t.Errorf("removed clients = %v, want [%v %v]", removed, a, b)
		}

		f.responses <- shared.UrlResponse{Id: requestID, IsAccepted: true, Attempt: 2}

		if got := <-result; !got.allowed {
			t.Errorf("MakeRequest() = false, want true")
		}
	})
}

// TestCloseStopsBothLoops is as much about synctest.Test returning at all
// as it is about any explicit assertion: if handleRequests or
// handleResponses failed to exit after Close, synctest.Test would hang or
// panic rather than let this test pass.
func TestCloseStopsBothLoops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeDispatcher()
		r := startRegistry(f)
		shutdown(r, f)
		synctest.Wait()
	})
}

func TestGetNextIdIsMonotonicUnderConcurrency(t *testing.T) {
	r := &RequestRegistry{IdMu: &sync.Mutex{}}

	const n = 50
	ids := make([]shared.RequestID, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = r.getNextId()
		}(i)
	}
	wg.Wait()

	seen := make(map[shared.RequestID]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate RequestID %d", id)
		}
		seen[id] = true
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
