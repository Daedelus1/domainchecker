package client

// EvaluateUrlRequest's tests construct a DomainCheckerClient directly with
// a custom stdinHandler, bypassing NewDomainCheckerClient's real dial --
// EvaluateUrlRequest itself never touches the network, and these need a
// controllable human-input reader without the cost/complexity of a real
// websocket connection. That constructor bypass needs this package's
// unexported stdinHandler field, so it stays an internal test file rather
// than moving to client_test.go.

import (
	"io"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ethanstewart.net/domainchecker/shared"
	"ethanstewart.net/domainchecker/shared/stdinhandler"
)

var testURL = url.URL{Scheme: "https", Host: "example.com", Path: "/"}

// evalResult carries EvaluateUrlRequest's two return values over a
// channel, since a chan of a single bool can't.
type evalResult struct {
	allowed     bool
	denyMessage string
}

func evaluate(dcc *DomainCheckerClient, request shared.UrlRequest) evalResult {
	allowed, denyMessage := dcc.EvaluateUrlRequest(request)
	return evalResult{allowed, denyMessage}
}

func TestEvaluateUrlRequestAnswers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"lowercase y", "y", true},
		{"uppercase Y", "Y", true},
		{"lowercase n", "n", false},
		{"anything else", "whatever", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, w := io.Pipe()
				defer func() { _ = w.Close() }()
				dcc := &DomainCheckerClient{stdinHandler: stdinhandler.New(r)}

				result := make(chan evalResult, 1)
				go func() {
					result <- evaluate(dcc, shared.UrlRequest{Url: testURL})
				}()

				// Let EvaluateUrlRequest reach its select (durably blocked
				// on it) before writing, so the write is guaranteed to be
				// the thing that resolves it rather than racing goroutine
				// scheduling to land before some other outcome.
				synctest.Wait()
				if _, err := w.Write([]byte(tt.input + "\n")); err != nil {
					t.Fatalf("Write() error = %v", err)
				}

				got := <-result
				if got.allowed != tt.want {
					t.Errorf("EvaluateUrlRequest() = %v, want %v", got.allowed, tt.want)
				}
				if got.allowed && got.denyMessage != "" {
					t.Errorf("DenyMessage = %q on an approval, want \"\"", got.denyMessage)
				}
				if !got.allowed && got.denyMessage == "" {
					t.Error("DenyMessage = \"\" on a denial, want a reason")
				}
			})
		})
	}
}

func TestEvaluateUrlRequestTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Already exhausted: readStdin hits EOF and exits immediately,
		// so Stream() never delivers anything and only the timer can fire.
		dcc := &DomainCheckerClient{stdinHandler: stdinhandler.New(strings.NewReader(""))}

		result := make(chan evalResult, 1)
		go func() {
			result <- evaluate(dcc, shared.UrlRequest{Url: testURL})
		}()

		got := <-result
		if got.allowed {
			t.Errorf("EvaluateUrlRequest() allowed = %v, want false on timeout", got.allowed)
		}
		if got.denyMessage == "" {
			t.Error("DenyMessage = \"\" on a timeout, want a reason")
		}
	})
}

// TestEvaluateUrlRequestIgnoresStaleInput is a regression test for the bug
// fixed earlier: a stdin line timestamped before the prompt was shown must
// not count as the answer, and -- just as importantly -- must not reset
// the countdown either. Using synctest lets this advance virtual time
// between queuing the stale line and starting the prompt, rather than
// racing real goroutine scheduling to produce the same ordering.
func TestEvaluateUrlRequestIgnoresStaleInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := io.Pipe()
		defer func() { _ = w.Close() }()
		dcc := &DomainCheckerClient{stdinHandler: stdinhandler.New(r)}

		// Queue a stale "y" before the prompt even starts: readStdin scans
		// it and blocks handing it to Stream(), since nothing is reading
		// yet, so its Timestamp is frozen before any of what follows.
		go func() {
			_, _ = w.Write([]byte("y\n"))
		}()
		synctest.Wait()

		// Advance virtual time so the prompt's timeOfRequest is strictly
		// after the stale message's Timestamp.
		time.Sleep(time.Second)

		result := make(chan evalResult, 1)
		go func() {
			result <- evaluate(dcc, shared.UrlRequest{Url: testURL})
		}()

		// The stale "y" gets consumed and discarded here, without
		// resolving the call and without restarting the timeout.
		synctest.Wait()
		select {
		case got := <-result:
			t.Fatalf("EvaluateUrlRequest() = %v from stale input, want it to still be waiting", got.allowed)
		default:
		}

		// The real, fresh answer does resolve it.
		if _, err := w.Write([]byte("n\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := <-result; got.allowed {
			t.Errorf("EvaluateUrlRequest() = %v, want false", got.allowed)
		}
	})
}

// TestEvaluateUrlRequestRemembersURLScope is an integration test for
// decisionMemory's wiring into EvaluateUrlRequest: answering "yu" once
// must make a later call for the same URL resolve immediately from
// memory, without ever reading from stdin or waiting on the timer.
func TestEvaluateUrlRequestRemembersURLScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := io.Pipe()
		defer func() { _ = w.Close() }()
		dcc := &DomainCheckerClient{stdinHandler: stdinhandler.New(r)}

		result := make(chan evalResult, 1)
		go func() {
			result <- evaluate(dcc, shared.UrlRequest{Url: testURL})
		}()
		synctest.Wait()
		if _, err := w.Write([]byte("yu\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := <-result; !got.allowed {
			t.Fatalf("first EvaluateUrlRequest() = %v, want true", got.allowed)
		}

		// No input is ever provided for this second call; if it fell
		// through to prompting instead of resolving from memory, nothing
		// would ever unblock it.
		got := evaluate(dcc, shared.UrlRequest{Url: testURL})
		if !got.allowed {
			t.Errorf("remembered EvaluateUrlRequest() = %v, want true", got.allowed)
		}
		if got.denyMessage != "" {
			t.Errorf("DenyMessage = %q on a remembered approval, want \"\"", got.denyMessage)
		}
	})
}

// TestEvaluateUrlRequestDenyBeatsRememberedDomainAllow is an integration
// test for the precedence rule end to end. The order matters here: once
// any rule covers a URL, EvaluateUrlRequest never prompts for it again, so
// the specific denial has to be recorded before the broader domain-allow
// -- via a different URL on the same domain -- or there'd be no prompt
// left to record the contradiction through in the first place.
func TestEvaluateUrlRequestDenyBeatsRememberedDomainAllow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := io.Pipe()
		defer func() { _ = w.Close() }()
		dcc := &DomainCheckerClient{stdinHandler: stdinhandler.New(r)}

		badURL := url.URL{Scheme: "https", Host: "example.com", Path: "/bad"}
		goodURL := url.URL{Scheme: "https", Host: "example.com", Path: "/good"}
		otherURL := url.URL{Scheme: "https", Host: "example.com", Path: "/other"}

		// Deny this one specific URL first.
		result := make(chan evalResult, 1)
		go func() {
			result <- evaluate(dcc, shared.UrlRequest{Url: badURL})
		}()
		synctest.Wait()
		if _, err := w.Write([]byte("nu\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := <-result; got.allowed {
			t.Fatalf("url-deny EvaluateUrlRequest() = %v, want false", got.allowed)
		}

		// Then allow the whole domain, via a different, not-yet-covered
		// URL on it.
		result = make(chan evalResult, 1)
		go func() {
			result <- evaluate(dcc, shared.UrlRequest{Url: goodURL})
		}()
		synctest.Wait()
		if _, err := w.Write([]byte("yd\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := <-result; !got.allowed {
			t.Fatalf("domain-allow EvaluateUrlRequest() = %v, want true", got.allowed)
		}

		// The earlier, more specific denial still holds even though the
		// domain is now remembered as allowed.
		if got := evaluate(dcc, shared.UrlRequest{Url: badURL}); got.allowed {
			t.Errorf("remembered EvaluateUrlRequest() = %v, want false (URL-level deny must beat domain-level allow)", got.allowed)
		}

		// A URL on the same domain that was never specifically denied is
		// allowed via the domain-level rule.
		if got := evaluate(dcc, shared.UrlRequest{Url: otherURL}); !got.allowed {
			t.Errorf("remembered EvaluateUrlRequest() = %v, want true", got.allowed)
		}
	})
}
