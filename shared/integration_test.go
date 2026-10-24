package shared_test

// End-to-end tests wiring a real DomainCheckerService (server side) to a
// real DomainCheckerClient (the client package) over an actual websocket
// connection (via httptest), proving the two sides actually compose: a
// Squid-protocol line in on the server's stdin travels over the wire as a
// UrlRequest, gets answered by a scripted "human" on the client, travels
// back as a UrlResponse, and comes out as OK/ERR on the server's stdout.

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/client"
	"ethanstewart.net/domainchecker/server/domainchecker"
	"ethanstewart.net/domainchecker/shared"
)

type integrationStack struct {
	stdin  *io.PipeWriter
	stdout *bufio.Scanner
	human  *io.PipeWriter
}

// newIntegrationStack wires up a real service and a real client connected
// to it over a real (loopback) websocket, and waits long enough for the
// connection to actually register server-side before returning -- there's
// no public hook to wait on directly, since AddClient runs a moment after
// the dial that established the connection returns.
func newIntegrationStack(t *testing.T) *integrationStack {
	t.Helper()

	svc := domainchecker.NewDomainCheckerService()
	t.Cleanup(svc.Close)

	srv := httptest.NewServer(http.HandlerFunc(svc.HandleWebsocket))
	t.Cleanup(srv.Close)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close() })
	svc.Start(stdinR, stdoutW)

	humanR, humanW := io.Pipe()
	t.Cleanup(func() { _ = humanW.Close() })

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dcc := client.NewDomainCheckerClient(wsURL, "http://localhost/", humanR, "")
	t.Cleanup(dcc.Close)

	// Give the handshake + AddClient time to complete before any test
	// sends a classification line, or it could race the "no client
	// connected" immediate-rejection path.
	time.Sleep(100 * time.Millisecond)

	return &integrationStack{
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
		human:  humanW,
	}
}

func (s *integrationStack) sendLine(t *testing.T, line string) {
	t.Helper()
	if _, err := s.stdin.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("Write(stdin) error = %v", err)
	}
}

func (s *integrationStack) answer(t *testing.T, yn string) {
	t.Helper()
	// See client_test.go's TestAnswerUrlRequestRespondsWithHumanAnswer:
	// this must happen after the prompt has actually started, or it gets
	// discarded as stale leftover input instead of being treated as the
	// answer to this request.
	time.Sleep(100 * time.Millisecond)
	if _, err := s.human.Write([]byte(yn + "\n")); err != nil {
		t.Fatalf("Write(human) error = %v", err)
	}
}

func (s *integrationStack) nextLine(t *testing.T) string {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- s.stdout.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("stdout scan failed: %v", s.stdout.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a line on stdout")
	}
	return s.stdout.Text()
}

func TestIntegrationApprovedRequest(t *testing.T) {
	stack := newIntegrationStack(t)

	stack.sendLine(t, "10.0.0.1 http://example.com/")
	stack.answer(t, "y")

	if got, want := stack.nextLine(t), "OK"; got != want {
		t.Errorf("stdout line = %q, want %q", got, want)
	}
}

func TestIntegrationDeniedRequest(t *testing.T) {
	stack := newIntegrationStack(t)

	stack.sendLine(t, "10.0.0.1 http://example.com/")
	stack.answer(t, "n")

	want := `ERR message="` + shared.DenyMessageOnce + `"`
	if got := stack.nextLine(t); got != want {
		t.Errorf("stdout line = %q, want %q", got, want)
	}
}

func TestIntegrationNoClientConnected(t *testing.T) {
	svc := domainchecker.NewDomainCheckerService()
	t.Cleanup(svc.Close)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close() })
	svc.Start(stdinR, stdoutW)

	stdout := bufio.NewScanner(stdoutR)
	if _, err := stdinW.Write([]byte("10.0.0.1 http://example.com/\n")); err != nil {
		t.Fatalf("Write(stdin) error = %v", err)
	}

	done := make(chan bool, 1)
	go func() { done <- stdout.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("stdout scan failed: %v", stdout.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a line on stdout")
	}
	want := `ERR message="` + shared.DenyMessageNoClient + `"`
	if got := stdout.Text(); got != want {
		t.Errorf("stdout line = %q, want %q (no client was ever connected)", got, want)
	}
}

func TestIntegrationMultipleSequentialRequests(t *testing.T) {
	stack := newIntegrationStack(t)

	cases := []struct {
		answer string
		want   string
	}{
		{"y", "OK"},
		{"n", `ERR message="` + shared.DenyMessageOnce + `"`},
		{"y", "OK"},
	}
	for i, c := range cases {
		stack.sendLine(t, "10.0.0.1 http://example.com/")
		stack.answer(t, c.answer)
		if got := stack.nextLine(t); got != c.want {
			t.Errorf("request %d: stdout line = %q, want %q", i, got, c.want)
		}
	}
}
