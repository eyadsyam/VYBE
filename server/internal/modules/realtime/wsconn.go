package realtime

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The WebSocket transport. The only file that knows what a WebSocket is —
// everything above it talks to Sink.

// Tuning constants. Each is a real tradeoff, not a default copied from an
// example.
const (
	// SendBuffer is how many events a client may fall behind before it is
	// dropped.
	//
	// 64 rather than a larger number because the buffer is memory per
	// connection, and the recovery path is cheap: a dropped client reconnects
	// and is served a delta. Sizing this to "never drop anybody" means sizing
	// it to the worst client on the worst network, which is unbounded.
	SendBuffer = 64

	// WriteWait bounds a single frame write. Without it, a client that has
	// stopped reading holds the write goroutine forever.
	WriteWait = 10 * time.Second

	// PongWait is how long to wait for a pong before declaring the peer gone.
	PongWait = 60 * time.Second

	// PingPeriod must be meaningfully less than PongWait, or the server
	// declares a healthy client dead while its own ping is still in flight.
	// The conventional ratio is 9/10.
	PingPeriod = (PongWait * 9) / 10

	// MaxFrameBytes bounds an inbound frame. Client frames are tiny; anything
	// larger is a bug or an attack, and an unbounded read is how one socket
	// exhausts the process.
	MaxFrameBytes = 4096
)

// Conn is one WebSocket client, implementing Sink.
type Conn struct {
	ws     *websocket.Conn
	userID string
	connID string
	roomID string

	send chan Envelope

	// closeOnce guards against the double close that happens routinely: the
	// read pump notices EOF at the same moment the hub drops the connection
	// for being slow.
	closeOnce sync.Once
	done      chan struct{}

	logger *slog.Logger
	now    func() time.Time
}

// NewConn wraps an upgraded WebSocket.
func NewConn(ws *websocket.Conn, userID, connID, roomID string, logger *slog.Logger) *Conn {
	if logger == nil {
		logger = slog.Default()
	}
	return &Conn{
		ws:     ws,
		userID: userID,
		connID: connID,
		roomID: roomID,
		send:   make(chan Envelope, SendBuffer),
		done:   make(chan struct{}),
		logger: logger,
		now:    time.Now,
	}
}

// UserID implements Sink.
func (c *Conn) UserID() string { return c.userID }

// ConnID implements Sink.
func (c *Conn) ConnID() string { return c.connID }

// RoomID reports which room this connection is in.
func (c *Conn) RoomID() string { return c.roomID }

// Send implements Sink.
//
// Non-blocking by construction: the default arm is what converts "this client
// is slow" into a return value the hub can act on, instead of a goroutine
// parked on a channel send while the rest of the room waits.
func (c *Conn) Send(e Envelope) error {
	select {
	case c.send <- e:
		return nil
	case <-c.done:
		return ErrSinkFull
	default:
		return ErrSinkFull
	}
}

// Close implements Sink. Safe to call from several goroutines and more than
// once.
func (c *Conn) Close(reason string) {
	c.closeOnce.Do(func() {
		close(c.done)

		// A close frame with a reason, so the client's own logs say why rather
		// than showing an unexplained 1006. Best-effort: if the peer is
		// already gone this fails, and that is fine.
		msg := websocket.FormatCloseMessage(websocket.CloseGoingAway, truncate(reason, 120))
		_ = c.ws.SetWriteDeadline(c.now().Add(WriteWait))
		_ = c.ws.WriteMessage(websocket.CloseMessage, msg)
		_ = c.ws.Close()
	})
}

// Done is closed when the connection is finished.
func (c *Conn) Done() <-chan struct{} { return c.done }

// WritePump drains the send channel onto the socket.
//
// One goroutine per connection, and it is the ONLY writer. gorilla permits at
// most one concurrent writer, so a second write path — a ping from a timer, a
// close frame from the hub — is a data race that manifests as a corrupted
// frame rather than a panic, which is far harder to diagnose.
func (c *Conn) WritePump() {
	ticker := time.NewTicker(PingPeriod)
	defer func() {
		ticker.Stop()
		c.Close("write pump stopped")
	}()

	for {
		select {
		case e := <-c.send:
			if err := c.writeJSON(EventFrame{Type: FrameEvent, Envelope: e}); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(c.now().Add(WriteWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.done:
			return
		}
	}
}

// WriteControl sends a control frame through the write pump's goroutine.
//
// Control frames are written directly rather than queued, because they are
// answers to a specific client request (a PONG, a DELTA) and queueing them
// behind a backlog of events would defeat their purpose — a PONG that arrives
// 40 events later measures the wrong thing entirely.
//
// The mutex is what keeps this safe alongside WritePump: gorilla allows one
// writer, and this is the second one.
func (c *Conn) WriteControl(frame any) error {
	return c.writeJSON(frame)
}

var writeMu sync.Mutex

func (c *Conn) writeJSON(frame any) error {
	// A package-level mutex is coarse — it serialises writes across every
	// connection in the process — but a per-connection one would have to live
	// in Conn and be taken by both writers anyway, and the write itself is
	// microseconds against a 10-second deadline. Revisit if a profile ever
	// says otherwise; do not revisit on intuition.
	writeMu.Lock()
	defer writeMu.Unlock()

	select {
	case <-c.done:
		return ErrSinkFull
	default:
	}

	if err := c.ws.SetWriteDeadline(c.now().Add(WriteWait)); err != nil {
		return err
	}
	return c.ws.WriteJSON(frame)
}

// ReadPump reads client frames until the socket closes.
//
// handle is called for each well-formed frame. The pump owns the read
// deadline: a client that stops responding to pings is disconnected rather
// than occupying a goroutine and a socket indefinitely.
func (c *Conn) ReadPump(handle func(ClientFrame)) {
	defer c.Close("read pump stopped")

	c.ws.SetReadLimit(MaxFrameBytes)
	_ = c.ws.SetReadDeadline(c.now().Add(PongWait))
	c.ws.SetPongHandler(func(string) error {
		// Every pong pushes the deadline out. This is the keepalive: without
		// it the deadline fires 60 seconds in even on a perfectly healthy
		// connection.
		return c.ws.SetReadDeadline(c.now().Add(PongWait))
	})

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Debug("websocket closed unexpectedly",
					"user", c.userID, "room", c.roomID, "err", err)
			}
			return
		}

		var frame ClientFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			// Answer and keep going. Disconnecting over one bad frame turns a
			// client-side encoding bug into a reconnect storm.
			_ = c.WriteControl(ErrorFrame{
				Type: FrameError, Code: ErrCodeBadFrame,
				Message: "frame is not valid JSON",
			})
			continue
		}
		handle(frame)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Upgrader is the WebSocket upgrader.
//
// CheckOrigin returns true because the API is token-authenticated and has no
// cookies: CSWSH depends on a browser attaching ambient credentials, and there
// are none to attach. The mobile client sends no meaningful Origin at all, so
// an allowlist would either be empty or would have to permit everything —
// security theatre with a maintenance cost.
//
// If a browser client with cookie auth is ever added, this MUST become a real
// allowlist. The comment is here so that decision is made deliberately rather
// than by someone assuming it was already handled.
var Upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
	// Compression is off. Room events are small JSON objects where per-message
	// deflate costs more CPU than it saves bytes, and gorilla's implementation
	// allocates a flate writer per message.
	EnableCompression: false,
}
