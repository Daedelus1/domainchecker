package websocketclient_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/shared"
	"ethanstewart.net/domainchecker/shared/websocketclient"
	"github.com/gorilla/websocket"
)

// newConnPair spins up a real (loopback) websocket connection and returns
// both ends: serverWS is what ClientManager.AddClient would receive from
// Upgrade in production, and clientWS stands in for the real client
// program, driven directly by the test to send and receive messages.
// gorilla/websocket v1.5.3 exposes no constructor for wrapping an
// arbitrary net.Conn directly, so a real loopback server is the practical
// way to get a connected pair.
func newConnPair(t *testing.T) (serverWS, clientWS *websocket.Conn) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		serverConnCh <- conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	serverWS = <-serverConnCh

	t.Cleanup(func() {
		_ = serverWS.Close()
		_ = clientWS.Close()
	})

	return serverWS, clientWS
}

func TestNewClientAvoidsCollisionWithLastExistingID(t *testing.T) {
	serverWS, _ := newConnPair(t)

	existing := shared.ClientID(time.Now())
	c := websocketclient.NewClient([]shared.ClientID{existing}, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {})

	if c.Id == existing {
		t.Errorf("NewClient() Id collided with the last existing Id %v", existing)
	}
}

func TestClientStartForwardsMessages(t *testing.T) {
	tests := []struct {
		name    string
		msgType int
	}{
		{"text", websocket.TextMessage},
		{"binary", websocket.BinaryMessage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverWS, clientWS := newConnPair(t)

			received := make(chan shared.InboundMessage, 1)
			c := websocketclient.NewClient(nil, serverWS, func(msg shared.InboundMessage) {
				received <- msg
			}, func(shared.ClientID) {})
			c.Start()
			t.Cleanup(c.Close)

			if err := clientWS.WriteMessage(tt.msgType, []byte("hello")); err != nil {
				t.Fatalf("WriteMessage() error = %v", err)
			}

			select {
			case msg := <-received:
				if msg.Id != c.Id {
					t.Errorf("InboundMessage.Id = %v, want %v", msg.Id, c.Id)
				}
				if string(msg.Msg) != "hello" {
					t.Errorf("InboundMessage.Msg = %q, want %q", msg.Msg, "hello")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for onMsg")
			}
		})
	}
}

func TestClientSendJSONWritesToConn(t *testing.T) {
	serverWS, clientWS := newConnPair(t)
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {})

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- c.SendJSON(shared.Heartbeat{})
	}()

	_ = clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := clientWS.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("SendJSON() error = %v", err)
	}

	marshaled, err := json.Marshal(shared.Heartbeat{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	// WriteJSON encodes via json.Encoder, which (unlike json.Marshal) adds
	// a trailing newline.
	want := append(marshaled, '\n')
	if !bytes.Equal(data, want) {
		t.Errorf("peer received %q, want %q", data, want)
	}
}

// TestClientSendJSONSerializesConcurrentWrites is a regression test for
// writeMu: gorilla/websocket permits at most one writer on a connection at
// a time, so without serializing concurrent SendJSON calls, the peer would
// see corrupted/interleaved frames instead of n complete, valid messages.
func TestClientSendJSONSerializesConcurrentWrites(t *testing.T) {
	serverWS, clientWS := newConnPair(t)
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {})

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.SendJSON(shared.UrlResponse{Id: shared.RequestID(i)}); err != nil {
				t.Errorf("SendJSON() error = %v", err)
			}
		}(i)
	}

	seen := make(map[shared.RequestID]bool)
	_ = clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < n; i++ {
		_, data, err := clientWS.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage() error = %v", err)
		}
		var resp shared.UrlResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("peer received a corrupted frame: %v (data=%s)", err, data)
		}
		seen[resp.Id] = true
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("received %d distinct messages, want %d", len(seen), n)
	}
}

func TestClientCloseIsIdempotent(t *testing.T) {
	serverWS, _ := newConnPair(t)

	var onCloseCount int32
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {
		atomic.AddInt32(&onCloseCount, 1)
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Close()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&onCloseCount); got != 1 {
		t.Errorf("onClose called %d times, want exactly 1", got)
	}
}

func TestHandleClientConnDetectsPeerClose(t *testing.T) {
	serverWS, clientWS := newConnPair(t)

	closed := make(chan struct{})
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {
		close(closed)
	})
	c.Start()

	if err := clientWS.Close(); err != nil {
		t.Fatalf("clientWS.Close() error = %v", err)
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("onClose was not called after the peer closed the connection")
	}
}

// TestHandleClientConnTimesOutOnSilence exercises the real
// websocketclient.TIMEOUT_DURATION wall-clock deadline (handleClientConn's
// SetReadDeadline), so it genuinely takes just over 10 real seconds; skip
// it in the common edit-run-edit loop with `go test -short`.
func TestHandleClientConnTimesOutOnSilence(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the real websocketclient.TIMEOUT_DURATION wall-clock deadline")
	}

	serverWS, _ := newConnPair(t)

	closed := make(chan struct{})
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {}, func(shared.ClientID) {
		close(closed)
	})
	c.Start()

	select {
	case <-closed:
	case <-time.After(websocketclient.TIMEOUT_DURATION + 5*time.Second):
		t.Fatal("connection was not closed after TIMEOUT_DURATION of silence")
	}
}

func TestLastSeenIsNotMutatedByHandleClientConn(t *testing.T) {
	serverWS, clientWS := newConnPair(t)

	processed := make(chan struct{})
	c := websocketclient.NewClient(nil, serverWS, func(shared.InboundMessage) {
		close(processed)
	}, func(shared.ClientID) {})
	initial := c.LastSeen
	c.Start()
	t.Cleanup(c.Close)

	if err := clientWS.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	select {
	case <-processed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message to be processed")
	}

	if c.LastSeen != initial {
		t.Errorf("LastSeen changed from %v to %v; handleClientConn must not update it itself", initial, c.LastSeen)
	}
}
