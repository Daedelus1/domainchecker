package clientmanager_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/server/clientmanager"
	"ethanstewart.net/domainchecker/shared"
	"github.com/gorilla/websocket"
)

// newConnPair spins up a real (loopback) websocket connection, mirroring
// how a real client actually connects via DomainCheckerService.HandleWebsocket.
// serverConn is what AddClient receives in production; peerConn stands in
// for the real client program, driven directly by the test.
func newConnPair(t *testing.T) (serverConn, peerConn *websocket.Conn) {
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
	peerConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	serverConn = <-serverConnCh

	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})

	return serverConn, peerConn
}

func TestAddClientRegistersAndIsReachable(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)

	id := cm.AddClient(serverConn)

	if gotID, ok := cm.GetFirstClient(); !ok || gotID != id {
		t.Fatalf("GetFirstClient() = (%v, %v), want (%v, true)", gotID, ok, id)
	}

	if err := cm.SendJSONToClient(id, shared.Heartbeat{}); err != nil {
		t.Fatalf("SendJSONToClient() error = %v", err)
	}

	_ = peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := peerConn.ReadMessage(); err != nil {
		t.Fatalf("peer did not receive the message: %v", err)
	}
}

func TestGetFirstClientNoClients(t *testing.T) {
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)

	if _, ok := cm.GetFirstClient(); ok {
		t.Error("GetFirstClient() ok = true, want false with no clients connected")
	}
}

func TestSendJSONNoClientsConnected(t *testing.T) {
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)

	if ok, err := cm.SendJSON(shared.Heartbeat{}); ok || err == nil {
		t.Errorf("SendJSON() = (%v, %v), want (false, non-nil error)", ok, err)
	}
}

func TestSendJSONWritesToAClient(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	cm.AddClient(serverConn)

	ok, err := cm.SendJSON(shared.Heartbeat{})
	if !ok || err != nil {
		t.Fatalf("SendJSON() = (%v, %v), want (true, nil)", ok, err)
	}

	_ = peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := peerConn.ReadMessage(); err != nil {
		t.Fatalf("peer did not receive the message: %v", err)
	}
}

func TestSendJSONToClientUnknownID(t *testing.T) {
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)

	err := cm.SendJSONToClient(shared.ClientID(time.Now()), shared.Heartbeat{})
	if err == nil {
		t.Error("SendJSONToClient() error = nil, want an error for an unregistered client")
	}
}

func TestRemoveClientUnregistersAndCloses(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	id := cm.AddClient(serverConn)

	if err := cm.RemoveClient(id); err != nil {
		t.Fatalf("RemoveClient() error = %v", err)
	}

	if _, ok := cm.GetFirstClient(); ok {
		t.Error("GetFirstClient() ok = true after RemoveClient, want false")
	}

	_ = peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := peerConn.ReadMessage(); err == nil {
		t.Error("peer's ReadMessage() succeeded after RemoveClient, want the connection closed")
	}
}

func TestRemoveClientUnknownID(t *testing.T) {
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)

	if err := cm.RemoveClient(shared.ClientID(time.Now())); err == nil {
		t.Error("RemoveClient() error = nil, want an error for an unregistered client")
	}
}

func TestDispatchUpdatesLastSeenOnHeartbeatReply(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	id := cm.AddClient(serverConn)

	initial, ok := cm.ClientLastSeen(id)
	if !ok {
		t.Fatalf("ClientLastSeen(%v) ok = false, want true right after AddClient", id)
	}

	time.Sleep(10 * time.Millisecond) // ensure a measurable time difference
	if err := peerConn.WriteJSON(shared.Heartbeat{}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := cm.ClientLastSeen(id)
		if !ok {
			t.Fatalf("ClientLastSeen(%v) ok = false", id)
		}
		if got.After(initial) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("LastSeen was not updated after the peer sent a heartbeat reply")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDispatchForwardsUrlResponseToResponseChan(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	cm.AddClient(serverConn)

	want := shared.UrlResponse{Id: shared.RequestID(7), IsAccepted: true, Attempt: 1}
	if err := peerConn.WriteJSON(want); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	select {
	case got := <-cm.ResponseChan:
		if got != want {
			t.Errorf("ResponseChan received %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the UrlResponse on ResponseChan")
	}
}

func TestDispatchDropsUnrecognizedMessages(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	cm.AddClient(serverConn)

	if err := peerConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"something_else"}`)); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	// Follow up with something recognizable: proof dispatch moved past the
	// dropped message instead of getting stuck or crashing on it.
	want := shared.UrlResponse{Id: shared.RequestID(1), IsAccepted: true, Attempt: 1}
	if err := peerConn.WriteJSON(want); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	select {
	case got := <-cm.ResponseChan:
		if got != want {
			t.Errorf("ResponseChan received %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch appears stuck after an unrecognized message")
	}
}

// TestSilentClientIsEventuallyRemoved exercises real
// HEARTBEAT_INTERVAL/websocketclient.TIMEOUT_DURATION wall-clock durations
// (a client that never replies gets evicted after ~10-12s, via either the
// heartbeat timeout or the read-deadline liveness check racing to notice
// first), so it genuinely takes several real seconds; skip it in the
// common edit-run-edit loop with `go test -short`.
func TestSilentClientIsEventuallyRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises real heartbeat/timeout wall-clock durations")
	}

	serverConn, _ := newConnPair(t)
	cm := clientmanager.NewClientManager()
	t.Cleanup(cm.Close)
	cm.AddClient(serverConn)

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := cm.GetFirstClient(); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("silent client was never removed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestCloseClosesAllClients(t *testing.T) {
	serverConn, peerConn := newConnPair(t)
	cm := clientmanager.NewClientManager()
	cm.AddClient(serverConn)

	cm.Close()

	_ = peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := peerConn.ReadMessage(); err == nil {
		t.Error("peer's ReadMessage() succeeded after Close(), want the connection closed")
	}
}
