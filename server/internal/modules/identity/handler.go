package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

// The HTTP surface for identity (FR-1–FR-6).
//
// Handlers here do three things and nothing else: decode, delegate, encode.
// Every rule lives in Service, which is why this file has no `if` statement
// about age bands or token lifetimes. When a handler starts making a decision,
// that decision has escaped its test coverage.

// Handler serves /v1/auth.
type Handler struct {
	svc     *Service
	tickets TicketStore
	now     func() time.Time
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, tickets TicketStore) *Handler {
	return &Handler{svc: svc, tickets: tickets, now: time.Now}
}

// SetClock replaces the time source. Tests only.
func (h *Handler) SetClock(now func() time.Time) { h.now = now }

// Routes returns the /v1/auth subtree.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/register", h.register)
	r.Post("/login", h.login)
	r.Post("/refresh", h.refresh)

	// Everything below needs a valid access token.
	r.Group(func(r chi.Router) {
		r.Use(h.RequireAuth)
		r.Post("/logout", h.logout)
		r.Get("/me", h.me)
		r.Post("/ws-ticket", h.wsTicket)
	})
	return r
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// Wire types are declared separately from the domain structs on purpose. A
// handler that serialises identity.User directly publishes every field ever
// added to it — which is how a password hash, an internal flag, or a
// soft-delete timestamp ends up in a public response six months later because
// somebody added a column.

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	DateOfBirth string `json:"dateOfBirth"` // RFC 3339 date, YYYY-MM-DD
	Locale      string `json:"locale"`
	Region      string `json:"region"`
	DeviceName  string `json:"deviceName"`
	Platform    string `json:"platform"`
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
	Platform   string `json:"platform"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type userResponse struct {
	ID              string `json:"id"`
	Handle          string `json:"handle"`
	DisplayName     string `json:"displayName"`
	AvatarURL       string `json:"avatarUrl,omitempty"`
	Locale          string `json:"locale"`
	Region          string `json:"region"`
	NumeralSystem   string `json:"numeralSystem"`
	AgeBand         string `json:"ageBand"`
	EntitlementTier string `json:"entitlementTier"`
	IsDiscoverable  bool   `json:"isDiscoverable"`
	CreatedAt       string `json:"createdAt"`
}

type sessionResponse struct {
	AccessToken string `json:"accessToken"`
	// RefreshToken is omitted on the overlap-replay path, where the client
	// already holds a usable one and a second would fork the family.
	RefreshToken string        `json:"refreshToken,omitempty"`
	ExpiresAt    string        `json:"expiresAt"`
	SessionID    string        `json:"sessionId"`
	User         *userResponse `json:"user"`
}

func toUserResponse(u *User) *userResponse {
	if u == nil {
		return nil
	}
	return &userResponse{
		ID:              u.ID,
		Handle:          u.Handle,
		DisplayName:     u.DisplayName,
		AvatarURL:       u.AvatarURL,
		Locale:          u.Locale,
		Region:          u.Region,
		NumeralSystem:   u.NumeralSystem,
		AgeBand:         string(u.AgeBand),
		EntitlementTier: u.EntitlementTier,
		IsDiscoverable:  u.IsDiscoverable,
		CreatedAt:       u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toSessionResponse(p *TokenPair) *sessionResponse {
	return &sessionResponse{
		AccessToken:  p.AccessToken,
		RefreshToken: p.RefreshToken,
		ExpiresAt:    p.ExpiresAt.UTC().Format(time.RFC3339),
		SessionID:    p.SessionID,
		User:         toUserResponse(p.User),
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dob, err := parseDate(req.DateOfBirth)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.ErrValidation.
			WithDetail("dateOfBirth must be a YYYY-MM-DD date.").
			WithFieldErrors(httpx.FieldError{
				Field: "dateOfBirth", Code: "INVALID_DATE",
				Detail: "expected YYYY-MM-DD",
			}))
		return
	}

	pair, err := h.svc.Register(r.Context(), RegisterInput{
		Email:       req.Email,
		Password:    req.Password,
		Handle:      req.Handle,
		DisplayName: req.DisplayName,
		DateOfBirth: dob,
		Locale:      req.Locale,
		Region:      req.Region,
		DeviceName:  req.DeviceName,
		Platform:    req.Platform,
	})
	if err != nil {
		httpx.WriteProblem(w, r, registrationProblem(err))
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, toSessionResponse(pair))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	pair, err := h.svc.Login(r.Context(), LoginInput{
		Email: req.Email, Password: req.Password,
		DeviceName: req.DeviceName, Platform: req.Platform,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			// One response for every failure. See the note in Service.Login:
			// the status, the code, and the detail must not vary with whether
			// the account exists.
			httpx.WriteProblem(w, r, ErrProblemInvalidCredentials)
			return
		}
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	httpx.WriteJSON(w, http.StatusOK, toSessionResponse(pair))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrRefreshRejected) {
			httpx.WriteProblem(w, r, ErrProblemRefreshRejected)
			return
		}
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	httpx.WriteJSON(w, http.StatusOK, toSessionResponse(pair))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	claims, ok := ClaimsFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}
	if err := h.svc.Logout(r.Context(), claims.SessionID); err != nil {
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}
	// 204: there is nothing to say, and inventing a body invites a client to
	// depend on it.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	claims, ok := ClaimsFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}
	user, err := h.svc.repo.UserByID(r.Context(), claims.Subject)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}
	if user == nil {
		// The token is valid and its subject is gone — a deleted account whose
		// access token has not yet expired. 401, not 404: the correct client
		// response is to discard the token, not to retry.
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized.WithDetail("This account no longer exists."))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUserResponse(user))
}

type wsTicketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
	// ExpiresInSeconds spares the client from clock arithmetic against a
	// server timestamp it cannot trust — its own clock may be minutes off,
	// which is the entire premise of ADR-002.
	ExpiresInSeconds int `json:"expiresInSeconds"`
}

func (h *Handler) wsTicket(w http.ResponseWriter, r *http.Request) {
	claims, ok := ClaimsFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.ErrUnauthorized)
		return
	}

	now := h.now()
	ticket, err := NewWSTicket(claims.Subject, claims.SessionID, now)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}
	if err := h.tickets.Put(r.Context(), ticket); err != nil {
		httpx.WriteProblem(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, wsTicketResponse{
		Ticket:           ticket.Plaintext,
		ExpiresAt:        ticket.ExpiresAt.UTC().Format(time.RFC3339),
		ExpiresInSeconds: int(WSTicketTTL.Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Problems
// ---------------------------------------------------------------------------

// The identity problem vocabulary. Declared here rather than inline so the set
// can be diffed in review and mirrored in openapi.yaml and the client's
// Failure mapping.
var (
	// ErrProblemInvalidCredentials is the ONLY response a failed login
	// produces. Its detail deliberately says "email or password", never which.
	ErrProblemInvalidCredentials = httpx.NewProblem(http.StatusUnauthorized,
		"INVALID_CREDENTIALS", "Invalid Credentials",
		"The email or password is incorrect.")

	// ErrProblemRefreshRejected covers expired, unknown, revoked, and
	// reuse-detected alike. The client's correct action is identical for all
	// four — discard and re-authenticate — and distinguishing them would tell
	// a token thief which guess was a real, already-rotated token.
	ErrProblemRefreshRejected = httpx.NewProblem(http.StatusUnauthorized,
		"REFRESH_REJECTED", "Refresh Rejected",
		"This refresh token cannot be used. Sign in again.")

	ErrProblemHandleTaken = httpx.NewProblem(http.StatusConflict,
		"HANDLE_TAKEN", "Handle Taken", "That handle is already in use.")

	ErrProblemEmailTaken = httpx.NewProblem(http.StatusConflict,
		"EMAIL_TAKEN", "Email Registered", "That email is already registered.")

	ErrProblemUnderMinimumAge = httpx.NewProblem(http.StatusForbidden,
		"UNDER_MINIMUM_AGE", "Below Minimum Age",
		"You must be at least 13 to create an account.")

	ErrProblemSessionRevoked = httpx.NewProblem(http.StatusUnauthorized,
		"SESSION_REVOKED", "Session Revoked",
		"This session was signed out. Sign in again.")

	ErrProblemPasswordWeak = httpx.NewProblem(http.StatusUnprocessableEntity,
		"PASSWORD_WEAK", "Password Not Accepted",
		"That password does not meet the minimum requirements.")

	ErrProblemPasswordBreached = httpx.NewProblem(http.StatusUnprocessableEntity,
		"PASSWORD_BREACHED", "Password Breached",
		"That password has appeared in a public breach. Choose a different one.")

	ErrProblemTicketInvalid = httpx.NewProblem(http.StatusUnauthorized,
		"WS_TICKET_INVALID", "Ticket Invalid",
		"The WebSocket ticket is missing, expired, or already used.")

	ErrProblemTokenInQuery = httpx.NewProblem(http.StatusBadRequest,
		"TOKEN_IN_QUERY", "Token In Query String",
		"Access tokens must not be passed as query parameters. Use a ws-ticket.")
)

// registrationProblem maps a Register error onto its wire form.
//
// EMAIL_TAKEN and HANDLE_TAKEN are distinguishable here, and that is a
// deliberate exception to the anti-enumeration rule that governs login. A
// signup form cannot function without telling the user which field to change,
// and a handle is public by definition. The mitigation is rate limiting on the
// endpoint, not a vague error that makes the form unusable.
func registrationProblem(err error) error {
	switch {
	case errors.Is(err, ErrEmailTaken):
		return ErrProblemEmailTaken.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "email", Code: "TAKEN", Detail: "already registered"})
	case errors.Is(err, ErrHandleTaken):
		return ErrProblemHandleTaken.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "handle", Code: "TAKEN", Detail: "already in use"})
	case errors.Is(err, ErrUnderMinimumAge):
		return ErrProblemUnderMinimumAge.WithCause(err)
	case errors.Is(err, ErrPasswordBreached):
		return ErrProblemPasswordBreached.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "password", Code: "BREACHED", Detail: "found in a breach corpus"})
	case errors.Is(err, ErrPasswordTooShort):
		return ErrProblemPasswordWeak.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "password", Code: "TOO_SHORT", Detail: "below the minimum length"})
	case errors.Is(err, ErrPasswordTooLong):
		return ErrProblemPasswordWeak.WithCause(err).
			WithFieldErrors(httpx.FieldError{Field: "password", Code: "TOO_LONG", Detail: "above the maximum length"})
	case errors.Is(err, ErrBreachSetUnavailable):
		// The policy fails closed. A missing breach set is an operational
		// fault, not a reason to accept a password we cannot check — so this
		// is a 503, and the operator sees it in the logs.
		return httpx.ErrUnavailable.WithCause(err).
			WithDetail("Registration is temporarily unavailable.")
	case errors.Is(err, ErrInvalidHandle):
		return httpx.ErrValidation.WithCause(err).
			WithDetail("The handle must be 3–30 characters: a–z, 0–9, underscore or dot.").
			WithFieldErrors(httpx.FieldError{Field: "handle", Code: "INVALID", Detail: "disallowed characters or length"})
	case errors.Is(err, ErrInvalidEmail):
		return httpx.ErrValidation.WithCause(err).
			WithDetail("The email address is not valid.").
			WithFieldErrors(httpx.FieldError{Field: "email", Code: "INVALID", Detail: "not a valid address"})
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type claimsKey struct{}

// ClaimsFromContext returns the authenticated claims set by RequireAuth.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(*Claims)
	return c, ok
}

// ContextWithClaims is for tests and for the WebSocket hub, which authenticates
// through a ticket rather than this middleware.
func ContextWithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// RequireAuth verifies a bearer token and rejects the request if it cannot.
//
// It also populates httpx's actor id, which FR-57 needs: idempotency keys are
// scoped per actor, and an unscoped key lets one user replay another's
// response. Forgetting this line would not fail any identity test — it would
// fail in the idempotency layer, far from the cause — so it is here rather
// than left to each handler.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}

		claims, err := h.svc.Authenticate(r.Context(), token)
		if err != nil {
			httpx.WriteProblem(w, r, authProblem(err))
			return
		}

		ctx := ContextWithClaims(r.Context(), claims)
		ctx = httpx.ContextWithActorID(ctx, claims.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken extracts a token from the Authorization header.
//
// The header only. A token in a query string is written to access logs, proxy
// logs, and browser history in plaintext, and accepting one "for convenience"
// means it will appear there — see ValidateWSUpgrade, which refuses the same
// thing on the socket path.
func bearerToken(r *http.Request) (string, error) {
	for _, banned := range []string{"access_token", "token", "jwt"} {
		if r.URL.Query().Has(banned) {
			return "", ErrProblemTokenInQuery
		}
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", httpx.ErrUnauthorized.WithDetail("An Authorization: Bearer header is required.")
	}
	scheme, token, found := strings.Cut(auth, " ")
	if !found || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
		return "", httpx.ErrUnauthorized.WithDetail("The Authorization header must be `Bearer <token>`.")
	}
	return strings.TrimSpace(token), nil
}

// authProblem maps a verification failure onto its wire form.
//
// TOKEN_EXPIRED is distinguished from UNAUTHORIZED because the client's
// response genuinely differs: expiry means "refresh and retry silently",
// while anything else means "send the user to login". Collapsing them would
// make every expiry a visible logout.
func authProblem(err error) error {
	switch {
	case errors.Is(err, ErrTokenExpired):
		return httpx.ErrTokenExpired.WithCause(err)
	case errors.Is(err, ErrSessionRevoked):
		return ErrProblemSessionRevoked.WithCause(err)
	case errors.Is(err, ErrMalformedToken),
		errors.Is(err, ErrBadAlgorithm),
		errors.Is(err, ErrBadSignature),
		errors.Is(err, ErrTokenNotYetValid),
		errors.Is(err, ErrWrongIssuerAudience):
		// All four collapse to one response. A client cannot act differently
		// on "bad signature" than on "wrong audience", and telling an attacker
		// which check failed is a free oracle for forging tokens.
		return httpx.ErrUnauthorized.WithCause(err).WithDetail("The access token is not valid.")
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

// parseDate accepts a YYYY-MM-DD date.
//
// Strictly date-only. Accepting a full RFC 3339 timestamp would make the age
// calculation depend on a client-supplied timezone, and a birth date does not
// have one — "born 2013-08-26" is the same fact in Cairo and in São Paulo,
// while "born 2013-08-26T00:00:00+14:00" quietly shifts a band boundary.
func parseDate(s string) (time.Time, error) {
	return time.ParseInLocation(time.DateOnly, strings.TrimSpace(s), time.UTC)
}
