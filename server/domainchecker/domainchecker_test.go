package domainchecker

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeRequestMaker is an in-memory stand-in for a *clientmanager.RequestRegistry,
// used to drive handleStdio's line-protocol logic deterministically
// without a real registry/clientmanager/websocket stack underneath it.
type fakeRequestMaker struct {
	mu          sync.Mutex
	verdict     bool
	denyMessage string
	calls       []url.URL
}

func (f *fakeRequestMaker) MakeRequest(u url.URL) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, u)
	return f.verdict, f.denyMessage
}

func (f *fakeRequestMaker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRequestMaker) lastURL() url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func newTestService(verdict bool) (*DomainCheckerService, *fakeRequestMaker) {
	f := &fakeRequestMaker{verdict: verdict}
	s := &DomainCheckerService{
		requestRegister: f,
		handleStdioMu:   &sync.Mutex{},
	}
	return s, f
}

func TestHandleStdioApproved(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	s.handleStdio(strings.NewReader("10.0.0.1 http://example.com/\n"), &stdout)

	if got, want := stdout.String(), "OK\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if f.callCount() != 1 {
		t.Fatalf("MakeRequest called %d times, want 1", f.callCount())
	}
	gotURL := f.lastURL()
	if got := gotURL.String(); got != "http://example.com/" {
		t.Errorf("MakeRequest called with %q, want %q", got, "http://example.com/")
	}
}

func TestHandleStdioDenied(t *testing.T) {
	s, f := newTestService(false)
	var stdout bytes.Buffer

	s.handleStdio(strings.NewReader("10.0.0.1 http://example.com/\n"), &stdout)

	if got, want := stdout.String(), "ERR\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if f.callCount() != 1 {
		t.Errorf("MakeRequest called %d times, want 1", f.callCount())
	}
}

// TestHandleStdioDeniedWithMessage covers the "show the deny message on
// ERR" behavior end to end, including that a message containing
// characters significant to Squid's helper protocol (spaces, a double
// quote, a backslash) comes out correctly quoted rather than desyncing or
// truncating the response line.
func TestHandleStdioDeniedWithMessage(t *testing.T) {
	s, f := newTestService(false)
	f.denyMessage = `Denied: say "no" \ remembered`
	var stdout bytes.Buffer

	s.handleStdio(strings.NewReader("10.0.0.1 http://example.com/\n"), &stdout)

	want := `ERR message="Denied: say \"no\" \\ remembered"` + "\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestHandleStdioConnectStyleURIGetsHostname is a regression test: Squid's
// %URI for a CONNECT request (i.e. every HTTPS request) is bare
// "host:port" with no scheme, which url.Parse doesn't error on but
// mis-parses -- leaving Hostname() empty, which would collapse
// domain-scoped memory across every HTTPS target. handleStdio must hand
// requestMaker a URL whose Hostname() is the real host.
func TestHandleStdioConnectStyleURIGetsHostname(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	s.handleStdio(strings.NewReader("10.0.0.1 example.com:443\n"), &stdout)

	if got, want := stdout.String(), "OK\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	gotURL := f.lastURL()
	if got, want := gotURL.Hostname(), "example.com"; got != want {
		t.Errorf("MakeRequest called with Hostname() = %q, want %q", got, want)
	}
}

func TestHandleStdioInvalidFormat(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	// Only one field: Squid's "<client-ip> <encoded-url> ..." needs at
	// least two.
	s.handleStdio(strings.NewReader("10.0.0.1\n"), &stdout)

	if got, want := stdout.String(), "ERR message=Invalid_Input_Format\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if f.callCount() != 0 {
		t.Errorf("MakeRequest called %d times, want 0 -- a malformed line must never reach it", f.callCount())
	}
}

func TestHandleStdioURLDecodingFailed(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	// "%zz" is not a valid percent-escape, so url.Parse rejects it.
	s.handleStdio(strings.NewReader("10.0.0.1 http://example.com/%zz\n"), &stdout)

	if got, want := stdout.String(), "BH message=URL_Decoding_Failed\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if f.callCount() != 0 {
		t.Errorf("MakeRequest called %d times, want 0 -- an undecodable URL must never reach it", f.callCount())
	}
}

func TestHandleStdioProcessesMultipleLinesInOrder(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	input := "10.0.0.1 http://a.example/\n" +
		"10.0.0.1\n" + // missing the encoded-url field
		"10.0.0.1 http://b.example/\n"
	s.handleStdio(strings.NewReader(input), &stdout)

	wantLines := []string{"OK", "ERR message=Invalid_Input_Format", "OK"}
	gotLines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("stdout = %q, want %d lines matching %v", stdout.String(), len(wantLines), wantLines)
	}
	for i, want := range wantLines {
		if gotLines[i] != want {
			t.Errorf("line %d = %q, want %q", i, gotLines[i], want)
		}
	}

	if f.callCount() != 2 {
		t.Fatalf("MakeRequest called %d times, want 2 (the malformed middle line must be skipped)", f.callCount())
	}
	if got := f.calls[0].String(); got != "http://a.example/" {
		t.Errorf("first call URL = %q, want %q", got, "http://a.example/")
	}
	if got := f.calls[1].String(); got != "http://b.example/" {
		t.Errorf("second call URL = %q, want %q", got, "http://b.example/")
	}
}

func TestHandleStdioReturnsAtEOF(t *testing.T) {
	s, _ := newTestService(true)
	var stdout bytes.Buffer

	done := make(chan struct{})
	go func() {
		s.handleStdio(strings.NewReader("10.0.0.1 http://example.com/\n"), &stdout)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStdio did not return at EOF")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated read error")
}

func TestHandleStdioSurvivesReadError(t *testing.T) {
	s, f := newTestService(true)
	var stdout bytes.Buffer

	done := make(chan struct{})
	go func() {
		s.handleStdio(errReader{}, &stdout)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStdio did not return after a read error")
	}
	if f.callCount() != 0 {
		t.Errorf("MakeRequest called %d times, want 0", f.callCount())
	}
}

func TestHandleStdioMuSerializesConcurrentCalls(t *testing.T) {
	s, _ := newTestService(true)

	release := make(chan struct{})
	first := &fakeRequestMaker{verdict: true}
	s.requestRegister = &blockingRequestMaker{release: release, inner: first}

	var out1, out2 bytes.Buffer
	firstDone := make(chan struct{})
	go func() {
		s.handleStdio(strings.NewReader("10.0.0.1 http://a.example/\n"), &out1)
		close(firstDone)
	}()

	// Give the first call a moment to actually be inside handleStdio
	// (holding handleStdioMu) before the second one tries to start.
	time.Sleep(50 * time.Millisecond)

	secondDone := make(chan struct{})
	go func() {
		s.handleStdio(strings.NewReader("10.0.0.1 http://b.example/\n"), &out2)
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("second handleStdio call returned before the first released handleStdioMu")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("handleStdio call did not complete after handleStdioMu was released")
		}
	}

	if out1.String() != "OK\n" || out2.String() != "OK\n" {
		t.Errorf("out1=%q out2=%q, want both %q", out1.String(), out2.String(), "OK\n")
	}
}

// blockingRequestMaker delays its first call until release is closed, to
// let a test hold handleStdioMu open for a controlled window.
type blockingRequestMaker struct {
	release <-chan struct{}
	inner   requestMaker
}

func (b *blockingRequestMaker) MakeRequest(u url.URL) (bool, string) {
	<-b.release
	return b.inner.MakeRequest(u)
}

func TestHandleWebsocketUpgradesAndRegistersClient(t *testing.T) {
	svc := NewDomainCheckerService()
	t.Cleanup(svc.Close)

	srv := httptest.NewServer(http.HandlerFunc(svc.HandleWebsocket))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	peerConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = peerConn.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := svc.clientManager.GetFirstClient(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("client was never registered with the ClientManager")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
