package rooms

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

// The HTTP surface for rooms (FR-11–FR-20).

// Publisher hands a durable event to connected clients.
//
// Failure here is deliberately NOT fatal to the request. The event is already
// committed to the log, so a client that misses the push will pick it up on
// its next resync — that is precisely the property ADR-003 buys. Failing the
// request instead would roll back nothing (the transaction has committed) and
// would tell the caller their action did not happen when it did.
type Publisher interface {
	Publish(ctx context.Context, e realtime.Envelope) error
}

// Actor identifies the authenticated caller.
//
// A function rather than an import of identity: §5.1 forbids rooms depending
// on identity's package, and all this layer needs is "who is calling".
type Actor func(ctx context.Context) (userID string, ok bool)

// Handler serves /v1/rooms.
type Handler struct {
	svc       *Service
	publisher Publisher
	actor     Actor
	logger    *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, publisher Publisher, actor Actor, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, publisher: publisher, actor: actor, logger: logger}
}

// Routes returns the /v1/rooms subtree. The caller mounts it behind auth.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Post("/join", h.join)
	r.Route("/{roomId}", func(r chi.Router) {
		r.Get("/", h.get)
		r.Post("/leave", h.leave)
		r.Post("/end", h.end)
		r.Post("/transition", h.transition)
	})
	return r
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type createRoomRequest struct {
	ContentID  string `json:"contentId"`
	Title      string `json:"title"`
	Visibility string `json:"visibility"`
	SyncMode   string `json:"syncMode"`
}

type joinRoomRequest struct {
	JoinCode string `json:"joinCode"`
}

type transitionRequest struct {
	Event string `json:"event"`
}

type participantResponse struct {
	UserID    string `json:"userId"`
	IsHost    bool   `json:"isHost"`
	Connected bool   `json:"connected"`
	JoinedAt  string `json:"joinedAt"`
}

type roomResponse struct {
	ID         string `json:"id"`
	ContentID  string `json:"contentId"`
	HostUserID string `json:"hostUserId"`

	// JoinCode is present only for members. It is the credential that admits
	// somebody to the room, so it must not appear in any response a
	// non-member can obtain — and the list endpoint returns rooms the caller
	// is in, which is why it is safe there.
	JoinCode string `json:"joinCode,omitempty"`
	ShareURL string `json:"shareUrl,omitempty"`

	Visibility      string `json:"visibility"`
	State           string `json:"state"`
	SyncMode        string `json:"syncMode"`
	Title           string `json:"title,omitempty"`
	MaxParticipants int    `json:"maxParticipants"`

	// CurrentSeq lets a reconnecting client compute its own gap before asking
	// for a resync (FR-30).
	CurrentSeq int64 `json:"currentSeq"`

	AnchorServerTime string `json:"anchorServerTime,omitempty"`
	AnchorOffsetMS   int64  `json:"anchorOffsetMs"`
	ReanchorCount    int    `json:"reanchorCount"`

	CreatedAt string `json:"createdAt"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	EndReason string `json:"endReason,omitempty"`

	Participants []participantResponse `json:"participants,omitempty"`

	// ServerTime is the anchor for ADR-002's clock. Every response that can
	// inform a timed decision carries it, so a client always has a fresh
	// reference without a second round trip.
	ServerTime string `json:"serverTime"`
}

func toRoomResponse(r *Room, participants []Participant, includeCode bool, now time.Time) roomResponse {
	out := roomResponse{
		ID:              r.ID,
		ContentID:       r.ContentID,
		HostUserID:      r.HostUserID,
		Visibility:      r.Visibility,
		State:           string(r.State),
		SyncMode:        r.SyncMode,
		Title:           r.Title,
		MaxParticipants: r.MaxParticipants,
		CurrentSeq:      r.CurrentSeq,
		AnchorOffsetMS:  r.AnchorOffsetMS,
		ReanchorCount:   r.ReanchorCount,
		CreatedAt:       r.CreatedAt.UTC().Format(time.RFC3339Nano),
		EndReason:       r.EndReason,
		ServerTime:      now.UTC().Format(time.RFC3339Nano),
	}
	if includeCode {
		out.JoinCode = r.JoinCode
		out.ShareURL = ShareURL(r.JoinCode)
	}
	if r.AnchorServerTime != nil {
		out.AnchorServerTime = r.AnchorServerTime.UTC().Format(time.RFC3339Nano)
	}
	if r.StartedAt != nil {
		out.StartedAt = r.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if r.EndedAt != nil {
		out.EndedAt = r.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, p := range participants {
		out.Participants = append(out.Participants, participantResponse{
			UserID:    p.UserID,
			IsHost:    p.IsHost,
			Connected: p.Connected,
			JoinedAt:  p.JoinedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	var req createRoomRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	m, err := h.svc.Create(r.Context(), CreateInput{
		HostUserID: userID,
		ContentID:  req.ContentID,
		Title:      req.Title,
		Visibility: req.Visibility,
		SyncMode:   req.SyncMode,
	})
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}

	h.publish(r.Context(), m)
	httpx.WriteJSON(w, http.StatusCreated, toRoomResponse(m.Room, nil, true, h.svc.now()))
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	var req joinRoomRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	m, err := h.svc.JoinByCode(r.Context(), req.JoinCode, userID)
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}
	h.publish(r.Context(), m)

	// Re-read the participants so the joiner sees who else is present —
	// otherwise the first thing every client does after joining is a second
	// request for exactly this.
	participants, err := h.svc.repo.Participants(r.Context(), m.Room.ID)
	if err != nil {
		// The join succeeded and is durable. Returning 500 here would tell the
		// caller their join failed when it did not, and they would retry into
		// an ALREADY_JOINED conflict.
		h.logger.WarnContext(r.Context(), "join succeeded but the participant list could not be read",
			"room", m.Room.ID, "err", err)
		participants = nil
	}
	httpx.WriteJSON(w, http.StatusOK, toRoomResponse(m.Room, participants, true, h.svc.now()))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	room, participants, err := h.svc.Get(r.Context(), chi.URLParam(r, "roomId"), userID)
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRoomResponse(room, participants, true, h.svc.now()))
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	m, err := h.svc.Leave(r.Context(), chi.URLParam(r, "roomId"), userID)
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}
	h.publish(r.Context(), m)

	// The join code is withheld: the caller is no longer a member, and echoing
	// the credential back to somebody on their way out is exactly the leak
	// FR-14 is about.
	httpx.WriteJSON(w, http.StatusOK, toRoomResponse(m.Room, nil, false, h.svc.now()))
}

func (h *Handler) end(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	m, err := h.svc.End(r.Context(), chi.URLParam(r, "roomId"), userID)
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}
	h.publish(r.Context(), m)
	httpx.WriteJSON(w, http.StatusOK, toRoomResponse(m.Room, nil, false, h.svc.now()))
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	var req transitionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ev, err := parseEvent(req.Event)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.ErrValidation.
			WithDetail("Unknown transition %q.", req.Event).
			WithFieldErrors(httpx.FieldError{
				Field: "event", Code: "UNKNOWN_EVENT",
				Detail: "expected one of ARM, START, REANCHOR, CANCEL, END",
			}))
		return
	}

	m, err := h.svc.Transition(r.Context(), chi.URLParam(r, "roomId"), userID, ev)
	if err != nil {
		httpx.WriteProblem(w, r, roomProblem(err))
		return
	}
	h.publish(r.Context(), m)
	httpx.WriteJSON(w, http.StatusOK, toRoomResponse(m.Room, nil, true, h.svc.now()))
}

// DefaultPageLimit and MaxPageLimit bound the room list (FR-59).
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.actor(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	params, err := httpx.ParsePageParams(r, DefaultPageLimit, MaxPageLimit)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var before *time.Time
	var beforeID string
	if params.Cursor != nil {
		t := params.Cursor.CreatedAt
		before, beforeID = &t, params.Cursor.ID
	}

	// One extra row, so "is there another page?" is answered by the query
	// rather than guessed from a count. A COUNT(*) over the same predicate is
	// a second scan of the same index for information the first scan already
	// had.
	found, err := h.svc.ListForUser(r.Context(), userID, before, beforeID, params.Limit+1)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	now := h.svc.now()
	items := make([]roomResponse, 0, len(found))
	for i := range found {
		items = append(items, toRoomResponse(&found[i], nil, true, now))
	}

	page := httpx.NewPage(items, params.Limit, func(rr roomResponse) httpx.Cursor {
		created, err := time.Parse(time.RFC3339Nano, rr.CreatedAt)
		if err != nil {
			// Unreachable: we formatted it two lines ago.
			created = now
		}
		return httpx.Cursor{CreatedAt: created, ID: rr.ID}
	})
	httpx.WriteJSON(w, http.StatusOK, page)
}

// publish hands the event to connected clients, logging rather than failing.
//
// See the note on Publisher: the event is already durable, so a delivery
// failure costs a client one extra resync, whereas failing the request would
// report a mutation that did happen as if it had not.
func (h *Handler) publish(ctx context.Context, m *Mutation) {
	if h.publisher == nil || m == nil || m.Event.ID == "" {
		return
	}
	if err := h.publisher.Publish(ctx, m.Event); err != nil {
		h.logger.WarnContext(ctx, "event committed but not published; clients will resync",
			"room", m.Event.Room, "seq", m.Event.Seq, "type", m.Event.Type, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Problems
// ---------------------------------------------------------------------------

// The rooms problem vocabulary.
var (
	ErrProblemRoomNotFound = httpx.NewProblem(http.StatusNotFound,
		"ROOM_NOT_FOUND", "Room Not Found",
		"No such room, or you are not a member of it.")

	ErrProblemRoomFull = httpx.NewProblem(http.StatusConflict,
		"ROOM_FULL", "Room Full", "This room has reached its participant limit.")

	ErrProblemRoomEnded = httpx.NewProblem(http.StatusConflict,
		"ROOM_ENDED", "Room Ended", "This room has already ended.")

	ErrProblemAlreadyJoined = httpx.NewProblem(http.StatusConflict,
		"ALREADY_JOINED", "Already Joined", "You are already in this room.")

	ErrProblemNotTheHost = httpx.NewProblem(http.StatusForbidden,
		"NOT_THE_HOST", "Not The Host", "Only the host can do that.")

	ErrProblemNotAParticipant = httpx.NewProblem(http.StatusForbidden,
		"NOT_A_PARTICIPANT", "Not A Participant", "You are not in this room.")

	ErrProblemIllegalTransition = httpx.NewProblem(http.StatusConflict,
		"ILLEGAL_TRANSITION", "Illegal Transition",
		"That action is not available from the room's current state.")

	ErrProblemContentNotFound = httpx.NewProblem(http.StatusUnprocessableEntity,
		"CONTENT_NOT_FOUND", "Content Not Found", "No such title in the catalogue.")

	ErrProblemJoinCodeUnavailable = httpx.NewProblem(http.StatusServiceUnavailable,
		"JOIN_CODE_UNAVAILABLE", "Join Code Unavailable",
		"Could not allocate a join code. Try again.")
)

// roomProblem maps a service error onto its wire form.
func roomProblem(err error) error {
	var illegal ErrIllegalTransition

	switch {
	case errors.Is(err, ErrRoomNotFound):
		return ErrProblemRoomNotFound.WithCause(err)
	case errors.Is(err, ErrRoomFull):
		return ErrProblemRoomFull.WithCause(err)
	case errors.Is(err, ErrRoomEnded):
		return ErrProblemRoomEnded.WithCause(err)
	case errors.Is(err, ErrAlreadyJoined):
		return ErrProblemAlreadyJoined.WithCause(err)
	case errors.Is(err, ErrNotTheHost):
		return ErrProblemNotTheHost.WithCause(err)
	case errors.Is(err, ErrNotAParticipant):
		return ErrProblemNotAParticipant.WithCause(err)
	case errors.As(err, &illegal):
		// The detail names both ends, because "illegal transition" alone
		// leaves a client author guessing what the room's state actually is.
		return ErrProblemIllegalTransition.WithCause(err).
			WithDetail("Cannot %s from %s.", illegal.Event, illegal.From).
			WithExtra("fromState", string(illegal.From)).
			WithExtra("attemptedEvent", string(illegal.Event))
	case errors.Is(err, ErrContentNotFound):
		return ErrProblemContentNotFound.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "contentId", Code: "NOT_FOUND", Detail: "no such content"})
	case errors.Is(err, ErrJoinCodeConflict):
		return ErrProblemJoinCodeUnavailable.WithCause(err)
	case errors.Is(err, ErrInvalidTitle):
		return httpx.ErrValidation.WithCause(err).
			WithDetail("The title must be at most %d characters.", MaxTitleLength).
			WithFieldErrors(httpx.FieldError{Field: "title", Code: "TOO_LONG", Detail: "over the maximum length"})
	case errors.Is(err, ErrInvalidSyncMode):
		return httpx.ErrValidation.WithCause(err).
			WithDetail("The sync mode must be COMPANION or MANUAL.").
			WithFieldErrors(httpx.FieldError{Field: "syncMode", Code: "INVALID", Detail: "unrecognised mode"})
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

var errUnknownEvent = errors.New("rooms: unknown transition event")

func parseEvent(raw string) (Event, error) {
	switch Event(raw) {
	case EventArm, EventStart, EventReanchor, EventCancel, EventEnd:
		return Event(raw), nil
	default:
		return "", errUnknownEvent
	}
}
