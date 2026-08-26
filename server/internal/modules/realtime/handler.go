package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
	"github.com/eyadsyam/vybe/server/internal/platform/ids"
)

// The WebSocket upgrade endpoint (FR-36–FR-42).

// TicketRedeemer exchanges a single-use ticket for an identity.
//
// A narrow interface rather than an import of identity: realtime must not
// depend on that module (§5.1), and all it needs is "who does this ticket
// belong to".
type TicketRedeemer interface {
	Redeem(ctx context.Context, plaintext string, now time.Time) (userID, sessionID string, err error)
}

// RoomReader is what the socket needs to know about a room.
type RoomReader interface {
	// Membership reports whether a user is in a room, and the room's current
	// sequence number.
	//
	// Returns ErrRoomNotFound when the room does not exist OR the user is not
	// in it — the same conflation the HTTP layer makes, for the same reason.
	Membership(ctx context.Context, roomID, userID string) (currentSeq int64, err error)

	// EventsSince returns events in (fromSeq, toSeq], oldest first.
	EventsSince(ctx context.Context, roomID string, fromSeq, toSeq int64) ([]Envelope, error)

	// OldestRetainedSeq is the lowest seq still in the log. A client whose
	// position predates it cannot be served a delta at any threshold.
	OldestRetainedSeq(ctx context.Context, roomID string) (int64, error)

	// Snapshot renders the room's whole state, for when a delta will not do.
	Snapshot(ctx context.Context, roomID string) (json.RawMessage, error)
}

// ErrRoomNotFound means the room does not exist or the caller is not a member.
var ErrRoomNotFound = errors.New("realtime: no such room, or not a member")

// Handler serves the WebSocket upgrade.
type Handler struct {
	hub       *Hub
	tickets   TicketRedeemer
	rooms     RoomReader
	threshold int
	logger    *slog.Logger
	now       func() time.Time
}

// NewHandler returns a Handler.
//
// threshold is FR-31's delta/snapshot boundary, passed in rather than read
// from the constant because FR-31 requires it be configuration — the right
// value is a bandwidth/database tradeoff that cannot be known before the load
// test, and hard-coding it means a production tuning change needs a deploy.
func NewHandler(hub *Hub, tickets TicketRedeemer, rooms RoomReader, threshold int, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if threshold <= 0 {
		threshold = DefaultDeltaThreshold
	}
	return &Handler{hub: hub, tickets: tickets, rooms: rooms, threshold: threshold, logger: logger, now: time.Now}
}

// SetClock replaces the time source. Tests only.
func (h *Handler) SetClock(now func() time.Time) { h.now = now }

// Problem vocabulary for the upgrade path.
var (
	ErrProblemTicketInvalid = httpx.NewProblem(http.StatusUnauthorized,
		"WS_TICKET_INVALID", "Ticket Invalid",
		"The WebSocket ticket is missing, expired, or already used.")

	ErrProblemTokenInQuery = httpx.NewProblem(http.StatusBadRequest,
		"TOKEN_IN_QUERY", "Token In Query String",
		"Access tokens must not be passed as query parameters. Request a ws-ticket instead.")

	ErrProblemRoomNotFound = httpx.NewProblem(http.StatusNotFound,
		"ROOM_NOT_FOUND", "Room Not Found",
		"No such room, or you are not a member of it.")

	ErrProblemRoomRequired = httpx.NewProblem(http.StatusBadRequest,
		"ROOM_REQUIRED", "Room Required", "A room query parameter is required.")
)

// ServeHTTP handles GET /v1/ws?room=<id>&ticket=<t>.
//
// Every failure below happens BEFORE the upgrade, and that ordering is
// deliberate: once a connection is upgraded there is no status code left to
// send, and a client that has to parse a close frame to learn it was
// unauthorised has a much worse time than one that receives a 401 with a
// problem body it already knows how to handle.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ticket, err := validateUpgradeRequest(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		httpx.WriteProblem(w, r, ErrProblemRoomRequired)
		return
	}

	now := h.now()
	userID, sessionID, err := h.tickets.Redeem(r.Context(), ticket, now)
	if err != nil {
		// One response for every ticket failure — unknown, expired, already
		// redeemed. A client cannot act differently on any of them, and
		// distinguishing them tells somebody replaying tickets which of their
		// guesses was real.
		httpx.WriteProblem(w, r, ErrProblemTicketInvalid.WithCause(err))
		return
	}

	currentSeq, err := h.rooms.Membership(r.Context(), roomID, userID)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			httpx.WriteProblem(w, r, ErrProblemRoomNotFound.WithCause(err))
			return
		}
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	ws, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own error response.
		h.logger.DebugContext(r.Context(), "websocket upgrade failed", "err", err)
		return
	}

	conn := NewConn(ws, userID, ids.New().String(), roomID, h.logger)
	h.hub.Join(roomID, conn)

	h.logger.InfoContext(r.Context(), "websocket connected",
		"room", roomID, "user", userID, "session", sessionID, "conn", conn.ConnID())

	go conn.WritePump()

	// HELLO carries the room's position so a client knows, without asking,
	// whether it missed anything — which saves a RESYNC round trip on the
	// common case of a clean reconnect with nothing to catch up on.
	if err := conn.WriteControl(HelloFrame{
		Type:             FrameHello,
		Room:             roomID,
		CurrentSeq:       currentSeq,
		ServerTime:       EpochMillis(now),
		HeartbeatSeconds: int(PingPeriod.Seconds()),
	}); err != nil {
		h.hub.Leave(roomID, conn.ConnID())
		conn.Close("could not send hello")
		return
	}

	// The read pump runs on THIS goroutine, so the request's lifetime is the
	// connection's lifetime. Returning early would let net/http believe the
	// handler is finished and reclaim the hijacked connection's resources.
	conn.ReadPump(func(frame ClientFrame) {
		h.handleFrame(r.Context(), conn, frame)
	})

	h.hub.Leave(roomID, conn.ConnID())
	h.logger.InfoContext(r.Context(), "websocket disconnected",
		"room", roomID, "user", userID, "conn", conn.ConnID())
}

// validateUpgradeRequest enforces FR-5 on the handshake.
//
// An access token in a query string is written to the server's access log,
// every proxy log in between, and the browser's history — permanently, in
// plaintext, somewhere nobody is watching. The ticket exists precisely so the
// credential on this path is single-use and expires in 60 seconds.
func validateUpgradeRequest(r *http.Request) (string, error) {
	q := r.URL.Query()
	for _, banned := range []string{"access_token", "token", "jwt", "bearer", "authorization"} {
		if q.Has(banned) {
			return "", ErrProblemTokenInQuery.WithDetail(
				"The %q query parameter is not accepted on this endpoint.", banned)
		}
	}
	// A bearer header is refused too, so the ticket is the only credential and
	// there is exactly one code path to audit.
	if auth := r.Header.Get("Authorization"); auth != "" {
		return "", ErrProblemTokenInQuery.WithDetail(
			"This endpoint authenticates with a ws-ticket, not an Authorization header.")
	}

	ticket := q.Get("ticket")
	if ticket == "" {
		return "", ErrProblemTicketInvalid.WithDetail("A ticket query parameter is required.")
	}
	return ticket, nil
}

func (h *Handler) handleFrame(ctx context.Context, conn *Conn, frame ClientFrame) {
	switch frame.Type {
	case FramePing:
		// Echo the client's clock back unmodified. It computes
		// offset = ((t1-t0) + (t2-t3)) / 2 from the pair, which is only
		// possible if both timestamps survive the round trip.
		_ = conn.WriteControl(PongFrame{
			Type:       FramePong,
			ClientTime: frame.ClientTime,
			ServerTime: EpochMillis(h.now()),
		})

	case FrameResync:
		h.handleResync(ctx, conn, frame.LastSeq)

	default:
		// Answered, not disconnected. A newer client speaking a frame this
		// server does not know should not lose its connection over it —
		// FR-33's tolerance runs in both directions.
		_ = conn.WriteControl(ErrorFrame{
			Type: FrameError, Code: ErrCodeUnknownFrame,
			Message: "unrecognised frame type " + frame.Type,
		})
	}
}

func (h *Handler) handleResync(ctx context.Context, conn *Conn, lastSeq int64) {
	roomID := conn.RoomID()

	currentSeq, err := h.rooms.Membership(ctx, roomID, conn.UserID())
	if err != nil {
		_ = conn.WriteControl(ErrorFrame{
			Type: FrameError, Code: ErrCodeResyncFailed,
			Message: "could not read the room's position",
		})
		return
	}

	oldest, err := h.rooms.OldestRetainedSeq(ctx, roomID)
	if err != nil {
		_ = conn.WriteControl(ErrorFrame{
			Type: FrameError, Code: ErrCodeResyncFailed,
			Message: "could not read the retention floor",
		})
		return
	}

	decision := DecideResync(lastSeq, currentSeq, oldest, h.threshold)

	switch decision.Mode {
	case ResyncUpToDate:
		_ = conn.WriteControl(DeltaFrame{
			Type: FrameDelta, From: lastSeq, To: currentSeq,
			Events: []Envelope{}, Applied: false,
		})

	case ResyncInvalid:
		// The client claims a position ahead of the server's. Its state is
		// corrupt or it is talking to a stale replica; either way a snapshot
		// is the only safe answer, and it is flagged separately because it is
		// never normal.
		h.logger.WarnContext(ctx, "client reported a seq ahead of the room",
			"room", roomID, "user", conn.UserID(), "clientSeq", lastSeq, "roomSeq", currentSeq)
		h.sendSnapshot(ctx, conn, currentSeq, decision.Reason)

	case ResyncSnapshot:
		h.sendSnapshot(ctx, conn, currentSeq, decision.Reason)

	case ResyncDelta:
		events, err := h.rooms.EventsSince(ctx, roomID, decision.FromSeq-1, decision.ToSeq)
		if err != nil {
			h.sendSnapshot(ctx, conn, currentSeq, "delta read failed; falling back to a snapshot")
			return
		}
		// Shipping a delta with a hole in it is worse than shipping a
		// snapshot: the client applies it, believes it is caught up, and stays
		// silently wrong until something else forces another resync.
		if err := VerifyContiguous(events, decision.FromSeq); err != nil {
			h.logger.ErrorContext(ctx, "the event log has a gap; sending a snapshot instead",
				"room", roomID, "err", err)
			h.sendSnapshot(ctx, conn, currentSeq, "the log has a gap")
			return
		}
		_ = conn.WriteControl(DeltaFrame{
			Type: FrameDelta, From: decision.FromSeq, To: decision.ToSeq,
			Events: events, Applied: true,
		})
	}
}

func (h *Handler) sendSnapshot(ctx context.Context, conn *Conn, currentSeq int64, reason string) {
	state, err := h.rooms.Snapshot(ctx, conn.RoomID())
	if err != nil {
		_ = conn.WriteControl(ErrorFrame{
			Type: FrameError, Code: ErrCodeResyncFailed,
			Message: "could not build a snapshot",
		})
		return
	}
	_ = conn.WriteControl(SnapshotFrame{
		Type: FrameSnapshot, CurrentSeq: currentSeq, State: state, Reason: reason,
	})
}
