package websocketclient

import (
	"log/slog"
	"net"
	"sync"
	"time"

	"ethanstewart.net/domainchecker/shared"
	"github.com/gorilla/websocket"
)

const TIMEOUT_DURATION = time.Second * 10

type Client struct {
	Id   shared.ClientID
	conn *websocket.Conn
	// LastSeen is updated externally (by ClientManager.updateClientLastSeen
	// on the server side), not by this file's own read-deadline-based
	// liveness check in handleClientConn. The two are separate mechanisms.
	LastSeen time.Time
	start    func()
	shutdown func()
	// writeMu serializes SendJSON calls, since gorilla/websocket permits at
	// most one writer on a connection at a time and callers (e.g. a
	// heartbeat reply racing a response computed on another goroutine) can
	// otherwise call it concurrently.
	writeMu sync.Mutex
}

// NewClient wraps an already-established websocket connection and assigns
// it a time-derived ID. Collision avoidance only checks the last entry of
// existingIds, which is sufficient because IDs come from time.Now() and so
// are naturally increasing: a collision can only happen against the most
// recently generated ID, immediately after it, before the clock advances.
// The returned Client is inert until Start is called.
func NewClient(existingIds []shared.ClientID, conn *websocket.Conn, onMsg func(msg shared.InboundMessage), onClose func(id shared.ClientID)) *Client {
	id := shared.ClientID(time.Now())
	if len(existingIds) > 0 {
		for id == existingIds[len(existingIds)-1] {
			time.Sleep(time.Microsecond)
			id = shared.ClientID(time.Now())
		}
	}

	c := Client{
		Id:       id,
		conn:     conn,
		LastSeen: time.Now(),
		shutdown: sync.OnceFunc(func() { _ = conn.Close(); onClose(id) }),
	}
	c.start = sync.OnceFunc(func() { go c.handleClientConn(onMsg, onClose) })
	return &c
}

// Start begins reading from the connection in the background. Safe to call
// more than once; only the first call has any effect.
func (c *Client) Start() {
	c.start()
}

// Close is safe to call from multiple call sites, but onClose must never
// call Close on this same Client synchronously: Close is still on the
// stack while onClose runs, and sync.OnceFunc's lock isn't reentrant, so
// doing so deadlocks.
func (c *Client) Close() {
	c.shutdown()
}

// SendJSON writes msg to the connection. Safe to call concurrently with
// itself: writeMu serializes access, since gorilla/websocket permits at
// most one writer on a connection at a time.
func (c *Client) SendJSON(msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(msg)
}

// handleClientConn runs for the life of the connection, reading frames in a
// loop and forwarding text/binary payloads to onMsg. Resets a
// TIMEOUT_DURATION read deadline on every successful read, so
// TIMEOUT_DURATION of complete silence from the peer -- not just an
// explicit close -- is enough to be treated as a dead connection. Always
// ends by calling Close (deferred), which synchronously invokes onClose;
// see Close's doc for the reentrancy constraint that follows from that.
// Must run on its own goroutine; started once via Start.
func (client *Client) handleClientConn(onMsg func(shared.InboundMessage), onClose func(clientId shared.ClientID)) {
	defer client.Close()
	_ = client.conn.SetReadDeadline(time.Now().Add(TIMEOUT_DURATION))
	for {
		msgType, msg, err := client.conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Warn("no activity from peer, treating as dead connection", "id", client.Id.ToString())
			} else if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Debug("peer closed connection cleanly", "id", client.Id.ToString())
			} else {
				slog.Warn("connection closed unexpectedly", "id", client.Id.ToString(), "error", err)
			}
			break
		}
		_ = client.conn.SetReadDeadline(time.Now().Add(TIMEOUT_DURATION))

		// gorilla/websocket handles control frames (ping/pong/close)
		// internally; a received close frame surfaces as an error above,
		// never as this message type, so only data frames reach here.
		if msgType == websocket.TextMessage || msgType == websocket.BinaryMessage {
			onMsg(shared.InboundMessage{Id: client.Id, Msg: msg})
			slog.Debug("Recieved msg", "msg", msg)
		}
	}
}
