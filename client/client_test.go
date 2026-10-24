package client_test

// The networked half of DomainCheckerClient's test coverage: handleMessages,
// answerUrlRequest, and sendJSON, all driven through the exported
// NewDomainCheckerClient over a real (loopback) websocket connection.
// EvaluateUrlRequest's own tests live in evaluate_internal_test.go instead,
// since they need to construct a DomainCheckerClient directly with a
// custom stdinHandler -- an unexported field -- to avoid the cost of a
// real connection for logic that never touches the network.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/client"
	"ethanstewart.net/domainchecker/shared"
	"github.com/gorilla/websocket"
)

var testURL = url.URL{Scheme: "https", Host: "example.com", Path: "/"}

// newTestClient dials a real (loopback) websocket server and returns the
// live DomainCheckerClient under test alongside the server-side end of the
// connection, which the test drives directly to send Heartbeats/
// UrlRequests and read back what the client sends.
func newTestClient(t *testing.T, humanInput io.Reader) (dcc *client.DomainCheckerClient, serverConn *websocket.Conn) {
	t.Helper()

	connCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		connCh <- conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dcc = client.NewDomainCheckerClient(wsURL, "http://localhost/", humanInput, "")
	t.Cleanup(dcc.Close)

	select {
	case serverConn = <-connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received a connection")
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	return dcc, serverConn
}

func TestHandleMessagesRepliesToHeartbeat(t *testing.T) {
	_, serverConn := newTestClient(t, strings.NewReader(""))

	if err := serverConn.WriteJSON(shared.Heartbeat{}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := serverConn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if _, ok := shared.ParseBinaryIntoMessageTypes(data).(shared.Heartbeat); !ok {
		t.Errorf("received %s, want a Heartbeat reply", data)
	}
}

func TestAnswerUrlRequestRespondsWithHumanAnswer(t *testing.T) {
	humanR, humanW := io.Pipe()
	t.Cleanup(func() { _ = humanW.Close() })
	_, serverConn := newTestClient(t, humanR)

	req := shared.UrlRequest{Id: shared.RequestID(42), Url: testURL, Attempt: 3}
	if err := serverConn.WriteJSON(req); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	// Give the request time to arrive and the prompt to actually start
	// before "typing" the answer: a reader pre-loaded with the answer (as
	// opposed to one fed live, like this) would have it read and stamped
	// before the prompt even starts, so it would be discarded as stale
	// leftover input instead of being treated as the real answer.
	time.Sleep(100 * time.Millisecond)
	if _, err := humanW.Write([]byte("y\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := serverConn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	resp, ok := shared.ParseBinaryIntoMessageTypes(data).(shared.UrlResponse)
	if !ok {
		t.Fatalf("received %s, want a UrlResponse", data)
	}
	if want := (shared.UrlResponse{Id: req.Id, IsAccepted: true, Attempt: req.Attempt}); resp != want {
		t.Errorf("UrlResponse = %+v, want %+v", resp, want)
	}
}

// TestSendJSONSerializesHeartbeatAndResponseWrites is a regression test
// for sendMu: a heartbeat reply (sent inline from handleMessages) and a
// UrlResponse (sent from answerUrlRequest's own goroutine) can race as two
// concurrent writers on the same connection, which gorilla/websocket does
// not tolerate on its own.
func TestSendJSONSerializesHeartbeatAndResponseWrites(t *testing.T) {
	humanR, humanW := io.Pipe()
	t.Cleanup(func() { _ = humanW.Close() })
	_, serverConn := newTestClient(t, humanR)

	req := shared.UrlRequest{Id: shared.RequestID(1), Url: testURL, Attempt: 1}
	if err := serverConn.WriteJSON(req); err != nil {
		t.Fatalf("WriteJSON(UrlRequest) error = %v", err)
	}
	if err := serverConn.WriteJSON(shared.Heartbeat{}); err != nil {
		t.Fatalf("WriteJSON(Heartbeat) error = %v", err)
	}

	// See TestAnswerUrlRequestRespondsWithHumanAnswer for why this can't
	// be a reader pre-loaded with the answer.
	time.Sleep(100 * time.Millisecond)
	if _, err := humanW.Write([]byte("y\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var gotHeartbeat, gotResponse bool
	for i := 0; i < 2; i++ {
		_, data, err := serverConn.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage() error = %v", err)
		}
		switch v := shared.ParseBinaryIntoMessageTypes(data).(type) {
		case shared.Heartbeat:
			gotHeartbeat = true
		case shared.UrlResponse:
			if v.Id != req.Id || !v.IsAccepted {
				t.Errorf("UrlResponse = %+v, want Id=%v IsAccepted=true", v, req.Id)
			}
			gotResponse = true
		default:
			t.Fatalf("received a corrupted/unrecognized frame: %#v (data=%s)", v, data)
		}
	}
	if !gotHeartbeat || !gotResponse {
		t.Errorf("gotHeartbeat=%v gotResponse=%v, want both", gotHeartbeat, gotResponse)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dcc, _ := newTestClient(t, strings.NewReader(""))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dcc.Close()
		}()
	}
	wg.Wait()

	select {
	case <-dcc.Shutdown:
	default:
		t.Error("Shutdown channel not closed after Close()")
	}
}

func TestClosesWhenServerDisconnects(t *testing.T) {
	dcc, serverConn := newTestClient(t, strings.NewReader(""))

	if err := serverConn.Close(); err != nil {
		t.Fatalf("serverConn.Close() error = %v", err)
	}

	select {
	case <-dcc.Shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown was not closed after the server disconnected")
	}
}
