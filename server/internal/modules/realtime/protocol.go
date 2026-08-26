package realtime

import (
	"encoding/json"
	"time"
)

// The client↔server message protocol (§7.2, FR-30, FR-36).
//
// Everything on the socket is a JSON object with a `type`. Events use the
// Envelope shape defined in eventlog.go; the frames below are the control
// messages that carry no sequence number because they are not part of the
// room's history.
//
// The distinction matters: an Envelope is durable and replayable, a control
// frame is not. Conflating them would mean a PONG ends up in the event log and
// occupies a sequence number, which makes the log's length a function of how
// many clients were connected.

// Control frame types, client → server.
const (
	// FrameResync asks for everything since `lastSeq` (FR-30).
	FrameResync = "RESYNC"
	// FramePing carries the client's clock for ADR-002's offset estimate.
	FramePing = "PING"
)

// Control frame types, server → client.
const (
	// FrameHello is the first frame after a successful upgrade.
	FrameHello = "HELLO"
	// FrameEvent wraps an Envelope.
	FrameEvent = "EVENT"
	// FrameDelta answers a RESYNC that fits under the threshold.
	FrameDelta = "DELTA"
	// FrameSnapshot answers a RESYNC that does not (FR-31).
	FrameSnapshot = "SNAPSHOT"
	// FramePong answers a PING.
	FramePong = "PONG"
	// FrameError reports a protocol-level problem without closing the socket.
	FrameError = "ERROR"
)

// ClientFrame is any message from a client.
//
// Decoded permissively into one struct rather than a discriminated union,
// because the alternative is two passes over the same bytes. Unknown types are
// answered with FrameError rather than a disconnect: FR-33's tolerance runs
// both ways, and a newer client speaking a frame this server does not know
// should not lose its connection over it.
type ClientFrame struct {
	Type string `json:"type"`

	// LastSeq is the client's position, for RESYNC.
	LastSeq int64 `json:"lastSeq,omitempty"`

	// ClientTime is the client's wall clock at send, for PING. Milliseconds
	// since the epoch, because a JSON number survives a Dart int and an RFC
	// 3339 string invites a timezone bug in exactly the place that cannot
	// afford one.
	ClientTime int64 `json:"clientTime,omitempty"`
}

// HelloFrame is sent immediately on connect.
//
// It carries the room's current position so a client knows, before asking,
// whether it has missed anything — which saves a RESYNC round trip on the
// overwhelmingly common case of a clean reconnect with nothing to catch up on.
type HelloFrame struct {
	Type       string `json:"type"`
	Room       string `json:"room"`
	CurrentSeq int64  `json:"currentSeq"`
	ServerTime int64  `json:"serverTime"` // epoch ms — ADR-002's reference
	// HeartbeatSeconds tells the client how often to expect a ping, so it can
	// set its own read deadline instead of guessing.
	HeartbeatSeconds int `json:"heartbeatSeconds"`
}

// EventFrame wraps a durable event.
type EventFrame struct {
	Type     string   `json:"type"`
	Envelope Envelope `json:"event"`
}

// DeltaFrame answers a RESYNC that fits (FR-30).
type DeltaFrame struct {
	Type    string     `json:"type"`
	From    int64      `json:"fromSeq"`
	To      int64      `json:"toSeq"`
	Events  []Envelope `json:"events"`
	Applied bool       `json:"applied"` // false when there was nothing to send
}

// SnapshotFrame answers a RESYNC that does not fit (FR-31).
//
// State is opaque here on purpose: the hub does not know what a room's state
// looks like, and giving it that knowledge would drag the rooms module into
// the realtime one. The caller supplies the marshalled body.
type SnapshotFrame struct {
	Type       string          `json:"type"`
	CurrentSeq int64           `json:"currentSeq"`
	State      json.RawMessage `json:"state"`
	Reason     string          `json:"reason"`
}

// PongFrame answers a PING, carrying both clocks (ADR-002).
//
// The client computes `offset = ((t1 - t0) + (t2 - t3)) / 2` from these, which
// is why both the client's original timestamp and the server's are echoed. A
// pong that carried only the server's time would let the client measure
// latency but not correct its own clock, and correcting the clock is the whole
// point.
type PongFrame struct {
	Type       string `json:"type"`
	ClientTime int64  `json:"clientTime"` // echoed, unmodified
	ServerTime int64  `json:"serverTime"`
}

// ErrorFrame reports a problem without closing the socket.
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Protocol error codes.
const (
	ErrCodeUnknownFrame = "UNKNOWN_FRAME"
	ErrCodeBadFrame     = "BAD_FRAME"
	ErrCodeResyncFailed = "RESYNC_FAILED"
	ErrCodeSeqAhead     = "SEQ_AHEAD"
)

// EpochMillis renders a time as milliseconds since the Unix epoch.
func EpochMillis(t time.Time) int64 { return t.UnixMilli() }
