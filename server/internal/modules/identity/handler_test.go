package identity_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
	"github.com/eyadsyam/vybe/server/internal/modules/identity/identitytest"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

type harness struct {
	t       *testing.T
	router  chi.Router
	handler *identity.Handler
	store   *identitytest.Store
	tickets *identitytest.TicketStore
	setNow  func(time.Time)
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	issuer, err := identity.NewTokenIssuer(priv, "vybe", "vybe-app")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	breaches, err := identity.LoadStaticBreachSet(strings.NewReader("hunter2hunter2\n"))
	if err != nil {
		t.Fatalf("LoadStaticBreachSet: %v", err)
	}

	store := identitytest.New()
	tickets := identitytest.NewTicketStore()
	svc := identity.NewService(store, issuer, identity.PasswordPolicy{Breaches: breaches}, passwords.TestParams)

	now := testNow
	svc.SetClock(func() time.Time { return now })

	h := identity.NewHandler(svc, tickets)
	h.SetClock(func() time.Time { return now })

	r := chi.NewRouter()
	// Trace is mounted because FR-58 requires traceId on every problem, and a
	// handler test that skips it would not notice the field going missing.
	r.Use(httpx.Trace)
	r.Mount("/v1/auth", h.Routes())

	return &harness{
		t: t, router: r, handler: h, store: store, tickets: tickets,
		setNow: func(v time.Time) { now = v },
	}
}

func (h *harness) do(method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()

	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			h.t.Fatalf("marshalling body: %v", err)
		}
		rdr = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	rr := httptest.NewRecorder()
	h.router.ServeHTTP(rr, req)
	return rr
}

// registerBody is a valid registration payload.
func registerBody() map[string]any {
	return map[string]any{
		"email":       "sara@example.com",
		"password":    "a sufficiently long passphrase",
		"handle":      "sara_q",
		"displayName": "سارة",
		"dateOfBirth": "2000-01-01",
		"locale":      "ar",
		"region":      "EG",
		"deviceName":  "Pixel 8",
		"platform":    "android",
	}
}

// problemOf decodes an RFC 9457 body and asserts the invariants FR-58 puts on
// every single one, so each test does not have to repeat them.
func problemOf(t *testing.T, rr *httptest.ResponseRecorder) httpx.ProblemDetails {
	t.Helper()

	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json (FR-58)", ct)
	}
	var p httpx.ProblemDetails
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem body %q: %v", rr.Body.String(), err)
	}
	if p.Status != rr.Code {
		t.Errorf("problem.status = %d but HTTP status = %d; they must agree", p.Status, rr.Code)
	}
	if p.Code == "" {
		t.Error("problem has no code; the client branches on it (§4.4)")
	}
	if p.TraceID == "" {
		t.Error("problem has no traceId; FR-58 requires one on every problem")
	}
	if p.Type == "" {
		t.Error("problem has no type member; RFC 9457 requires one")
	}
	return p
}

func sessionOf(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
	return body
}

// ---------------------------------------------------------------------------
// Register
// ---------------------------------------------------------------------------

func TestRegisterEndpointReturns201AndASession(t *testing.T) {
	h := newHarness(t)
	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body)
	}
	body := sessionOf(t, rr)
	for _, field := range []string{"accessToken", "refreshToken", "expiresAt", "sessionId", "user"} {
		if body[field] == nil || body[field] == "" {
			t.Errorf("response is missing %q: %v", field, body)
		}
	}
	user := body["user"].(map[string]any)
	if user["handle"] != "sara_q" {
		t.Errorf("user.handle = %v, want sara_q", user["handle"])
	}
	if user["displayName"] != "سارة" {
		t.Errorf("user.displayName = %v; Arabic must survive the round trip", user["displayName"])
	}
}

func TestRegisterResponseNeverLeaksACredential(t *testing.T) {
	// The wire types exist precisely to make this true by construction. The
	// test is here because "somebody adds a field to the domain struct" is a
	// realistic future, and serialising the domain type directly is the
	// easiest refactor to make by accident.
	h := newHarness(t)
	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())
	raw := rr.Body.String()

	for _, forbidden := range []string{
		"passwordHash", "password_hash", "$argon2id$",
		"a sufficiently long passphrase", // the plaintext
		"dateOfBirth", "date_of_birth",   // §12.6: exact DOB is not public
		"email", // not part of the public user shape
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the registration response contains %q:\n%s", forbidden, raw)
		}
	}
}

func TestRegisterEndpointMapsDomainErrorsToProblems(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]any)
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{
			name:       "under 13",
			mutate:     func(b map[string]any) { b["dateOfBirth"] = testNow.AddDate(-12, 0, 0).Format(time.DateOnly) },
			wantStatus: http.StatusForbidden,
			wantCode:   "UNDER_MINIMUM_AGE",
		},
		{
			name:       "password too short",
			mutate:     func(b map[string]any) { b["password"] = "short" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PASSWORD_WEAK",
			wantField:  "password",
		},
		{
			name:       "breached password",
			mutate:     func(b map[string]any) { b["password"] = "hunter2hunter2" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PASSWORD_BREACHED",
			wantField:  "password",
		},
		{
			name:       "invalid handle",
			mutate:     func(b map[string]any) { b["handle"] = "no spaces allowed" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "VALIDATION_FAILED",
			wantField:  "handle",
		},
		{
			name:       "invalid email",
			mutate:     func(b map[string]any) { b["email"] = "not-an-email" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "VALIDATION_FAILED",
			wantField:  "email",
		},
		{
			name:       "malformed date",
			mutate:     func(b map[string]any) { b["dateOfBirth"] = "01/01/2000" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "VALIDATION_FAILED",
			wantField:  "dateOfBirth",
		},
		{
			name:       "timestamp instead of a date",
			mutate:     func(b map[string]any) { b["dateOfBirth"] = "2000-01-01T00:00:00Z" },
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "VALIDATION_FAILED",
			wantField:  "dateOfBirth",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			body := registerBody()
			tt.mutate(body)

			rr := h.do(http.MethodPost, "/v1/auth/register", body)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tt.wantStatus, rr.Body)
			}
			p := problemOf(t, rr)
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
			if tt.wantField != "" {
				found := false
				for _, fe := range p.Errors {
					if fe.Field == tt.wantField {
						found = true
					}
				}
				if !found {
					t.Errorf("errors[] does not name %q: %+v", tt.wantField, p.Errors)
				}
			}
		})
	}
}

func TestRegisterEndpointReportsDuplicatesPerField(t *testing.T) {
	// A deliberate exception to the anti-enumeration rule that governs login:
	// a signup form cannot function without telling the user which field to
	// change, and a handle is public by definition.
	h := newHarness(t)
	if rr := h.do(http.MethodPost, "/v1/auth/register", registerBody()); rr.Code != http.StatusCreated {
		t.Fatalf("first register: %d %s", rr.Code, rr.Body)
	}

	dupEmail := registerBody()
	dupEmail["handle"] = "someone_else"
	rr := h.do(http.MethodPost, "/v1/auth/register", dupEmail)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate email status = %d, want 409", rr.Code)
	}
	if p := problemOf(t, rr); p.Code != "EMAIL_TAKEN" {
		t.Errorf("code = %q, want EMAIL_TAKEN", p.Code)
	}

	dupHandle := registerBody()
	dupHandle["email"] = "other@example.com"
	rr = h.do(http.MethodPost, "/v1/auth/register", dupHandle)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate handle status = %d, want 409", rr.Code)
	}
	if p := problemOf(t, rr); p.Code != "HANDLE_TAKEN" {
		t.Errorf("code = %q, want HANDLE_TAKEN", p.Code)
	}
}

// ---------------------------------------------------------------------------
// Body decoding
// ---------------------------------------------------------------------------

func TestMalformedBodiesProduceProblemsNotPanics(t *testing.T) {
	tests := []struct {
		name       string
		body       any
		ct         string
		wantStatus int
		wantCode   string
	}{
		{"not json", "this is not json", "application/json", http.StatusBadRequest, "BAD_REQUEST"},
		{"empty body", "", "application/json", http.StatusBadRequest, "BAD_REQUEST"},
		{"truncated json", `{"email": "a@b.co"`, "application/json", http.StatusBadRequest, "BAD_REQUEST"},
		{"two objects", `{"email":"a@b.co"}{"email":"c@d.co"}`, "application/json", http.StatusBadRequest, "BAD_REQUEST"},
		{"json array", `[]`, "application/json", http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		{"wrong field type", `{"email": 42}`, "application/json", http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		// A typo'd field name must be reported, not ignored. Silently dropping
		// it means the user's password never arrives and the error blames a
		// missing field they can see they typed.
		{"unknown field", `{"emial": "a@b.co"}`, "application/json", http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		{"wrong content type", `{"email":"a@b.co"}`, "text/plain", http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{"form content type", `email=a@b.co`, "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(tt.body.(string)))
			req.Header.Set("Content-Type", tt.ct)
			rr := httptest.NewRecorder()
			h.router.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tt.wantStatus, rr.Body)
			}
			if p := problemOf(t, rr); p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
		})
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t)
	// 2 MiB of valid JSON, over the 1 MiB cap.
	huge := `{"displayName":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.router.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body)
	}
	if p := problemOf(t, rr); p.Code != "PAYLOAD_TOO_LARGE" {
		t.Errorf("code = %q, want PAYLOAD_TOO_LARGE", p.Code)
	}
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLoginEndpointSucceeds(t *testing.T) {
	h := newHarness(t)
	if rr := h.do(http.MethodPost, "/v1/auth/register", registerBody()); rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}

	rr := h.do(http.MethodPost, "/v1/auth/login", map[string]any{
		"email": "sara@example.com", "password": "a sufficiently long passphrase",
		"deviceName": "iPhone", "platform": "ios",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body)
	}
	if sessionOf(t, rr)["accessToken"] == "" {
		t.Error("no access token in the login response")
	}
}

func TestLoginEndpointGivesOneIdenticalResponseForEveryFailure(t *testing.T) {
	// The HTTP-level twin of TestLoginFailuresAreIndistinguishable. Even with
	// a single error sentinel inside the service, a handler can still leak the
	// difference through a status code or a detail string — so the assertion
	// is on the full serialised body, minus the per-request trace id.
	h := newHarness(t)
	if rr := h.do(http.MethodPost, "/v1/auth/register", registerBody()); rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}

	attempts := []map[string]any{
		{"email": "nobody@example.com", "password": "a sufficiently long passphrase"},
		{"email": "sara@example.com", "password": "the wrong passphrase entirely"},
		{"email": "sara@example.com", "password": ""},
		{"email": "not-an-email", "password": "whatever at all"},
		{"email": "", "password": ""},
	}

	var first string
	for i, attempt := range attempts {
		rr := h.do(http.MethodPost, "/v1/auth/login", attempt)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401; body = %s", i, rr.Code, rr.Body)
		}
		p := problemOf(t, rr)
		if p.Code != "INVALID_CREDENTIALS" {
			t.Errorf("attempt %d: code = %q, want INVALID_CREDENTIALS", i, p.Code)
		}

		p.TraceID = "" // varies per request by design
		normalised, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("re-marshalling: %v", err)
		}
		if i == 0 {
			first = string(normalised)
			continue
		}
		if string(normalised) != first {
			t.Errorf("attempt %d produced a different body than attempt 0:\n got %s\nwant %s\n"+
				"any difference here enumerates the user table", i, normalised, first)
		}
	}
}

// ---------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------

func TestRefreshEndpointRotates(t *testing.T) {
	h := newHarness(t)
	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}
	first := sessionOf(t, rr)

	h.setNow(testNow.Add(time.Hour))
	rr = h.do(http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refreshToken": first["refreshToken"],
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.Code, rr.Body)
	}
	second := sessionOf(t, rr)
	if second["refreshToken"] == first["refreshToken"] {
		t.Error("the refresh token did not rotate")
	}
	if second["accessToken"] == first["accessToken"] {
		t.Error("the access token did not change")
	}
}

func TestRefreshEndpointGivesOneResponseForEveryFailure(t *testing.T) {
	h := newHarness(t)
	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}
	pair := sessionOf(t, rr)

	// Rotate once, then present the old token late — reuse detection.
	h.setNow(testNow.Add(time.Hour))
	if rr := h.do(http.MethodPost, "/v1/auth/refresh", map[string]any{"refreshToken": pair["refreshToken"]}); rr.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rr.Code, rr.Body)
	}
	h.setNow(testNow.Add(time.Hour).Add(identity.OverlapWindow + time.Second))

	for _, tt := range []struct {
		name  string
		token any
	}{
		{"unknown token", "never issued"},
		{"empty token", ""},
		{"reused token", pair["refreshToken"]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := h.do(http.MethodPost, "/v1/auth/refresh", map[string]any{"refreshToken": tt.token})
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rr.Code, rr.Body)
			}
			if p := problemOf(t, rr); p.Code != "REFRESH_REJECTED" {
				t.Errorf("code = %q, want REFRESH_REJECTED — a thief must not learn "+
					"which of their guesses was a real rotated token", p.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Authenticated routes
// ---------------------------------------------------------------------------

func (h *harness) registerAndAuth() (accessToken, refreshToken, sessionID string) {
	h.t.Helper()
	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())
	if rr.Code != http.StatusCreated {
		h.t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}
	body := sessionOf(h.t, rr)
	return body["accessToken"].(string), body["refreshToken"].(string), body["sessionId"].(string)
}

func TestMeReturnsTheAuthenticatedUser(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()

	rr := h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body)
	}
	var user map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &user); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if user["handle"] != "sara_q" {
		t.Errorf("handle = %v, want sara_q", user["handle"])
	}
}

func TestProtectedRoutesRejectBadAuthorization(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()

	tests := []struct {
		name     string
		headers  []string
		path     string
		wantCode string
	}{
		{"no header", nil, "/v1/auth/me", "UNAUTHORIZED"},
		{"empty header", []string{"Authorization", ""}, "/v1/auth/me", "UNAUTHORIZED"},
		{"not bearer", []string{"Authorization", "Basic abc123"}, "/v1/auth/me", "UNAUTHORIZED"},
		{"bearer with no token", []string{"Authorization", "Bearer "}, "/v1/auth/me", "UNAUTHORIZED"},
		{"garbage token", []string{"Authorization", "Bearer not.a.token"}, "/v1/auth/me", "UNAUTHORIZED"},
		{"token missing a segment", []string{"Authorization", "Bearer " + strings.Join(strings.Split(token, ".")[:2], ".")}, "/v1/auth/me", "UNAUTHORIZED"},
		// FR-5: a token in the query string lands in access logs and browser
		// history, permanently and in plaintext. Refusing it is not a
		// preference.
		{"token in query string", []string{"Authorization", "Bearer " + token}, "/v1/auth/me?access_token=" + token, "TOKEN_IN_QUERY"},
		{"token param in query string", []string{"Authorization", "Bearer " + token}, "/v1/auth/me?token=x", "TOKEN_IN_QUERY"},
		{"jwt param in query string", []string{"Authorization", "Bearer " + token}, "/v1/auth/me?jwt=x", "TOKEN_IN_QUERY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := h.do(http.MethodGet, tt.path, nil, tt.headers...)
			p := problemOf(t, rr)
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (status %d)", p.Code, tt.wantCode, rr.Code)
			}
		})
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	// RFC 7235 says the scheme is case-insensitive, and real clients send
	// "bearer". Rejecting it would be a spec violation that only shows up
	// against one HTTP library.
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		rr := h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", scheme+" "+token)
		if rr.Code != http.StatusOK {
			t.Errorf("scheme %q rejected: %d %s", scheme, rr.Code, rr.Body)
		}
	}
}

func TestExpiredTokenIsDistinguishableFromInvalid(t *testing.T) {
	// The one distinction worth making. TOKEN_EXPIRED means "refresh and retry
	// silently"; anything else means "send the user to login". Collapsing them
	// turns every 15-minute expiry into a visible logout.
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()

	h.setNow(testNow.Add(identity.AccessTokenTTL + time.Hour))
	rr := h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if p := problemOf(t, rr); p.Code != "TOKEN_EXPIRED" {
		t.Errorf("code = %q, want TOKEN_EXPIRED", p.Code)
	}
}

func TestLogoutRevokesTheSessionImmediately(t *testing.T) {
	h := newHarness(t)
	token, refresh, _ := h.registerAndAuth()

	rr := h.do(http.MethodPost, "/v1/auth/logout", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204; body = %s", rr.Code, rr.Body)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", rr.Body)
	}

	// The access token is still cryptographically valid. That is the point:
	// without the session check, "sign out" would leave it working for the
	// remainder of its 15 minutes.
	rr = h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401", rr.Code)
	}
	if p := problemOf(t, rr); p.Code != "SESSION_REVOKED" {
		t.Errorf("code = %q, want SESSION_REVOKED", p.Code)
	}

	rr = h.do(http.MethodPost, "/v1/auth/refresh", map[string]any{"refreshToken": refresh})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("refresh after logout = %d, want 401", rr.Code)
	}
}

func TestLogoutIsIdempotentOverHTTP(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()
	if rr := h.do(http.MethodPost, "/v1/auth/logout", nil, "Authorization", "Bearer "+token); rr.Code != http.StatusNoContent {
		t.Fatalf("first logout: %d", rr.Code)
	}
	// The second attempt cannot authenticate any more — the session is gone —
	// so it is a 401 rather than a 204. That is still idempotent in the sense
	// that matters: the state is the same and nothing broke.
	rr := h.do(http.MethodPost, "/v1/auth/logout", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("second logout = %d, want 401", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// WebSocket tickets
// ---------------------------------------------------------------------------

func TestWSTicketIsIssuedToAnAuthenticatedCaller(t *testing.T) {
	h := newHarness(t)
	token, _, sessionID := h.registerAndAuth()

	rr := h.do(http.MethodPost, "/v1/auth/ws-ticket", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body)
	}

	var body struct {
		Ticket           string `json:"ticket"`
		ExpiresAt        string `json:"expiresAt"`
		ExpiresInSeconds int    `json:"expiresInSeconds"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Ticket == "" {
		t.Fatal("no ticket in the response")
	}
	if body.ExpiresInSeconds != 60 {
		t.Errorf("expiresInSeconds = %d, want 60 (ADR-011)", body.ExpiresInSeconds)
	}

	// It must redeem exactly once. A ticket that survives redemption is a
	// replayable credential in a URL.
	redeemed, err := h.tickets.Redeem(t.Context(), body.Ticket, testNow)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if redeemed.SessionID != sessionID {
		t.Errorf("ticket session = %q, want %q", redeemed.SessionID, sessionID)
	}
	if _, err := h.tickets.Redeem(t.Context(), body.Ticket, testNow); !errors.Is(err, identity.ErrTicketNotFound) {
		t.Errorf("a ticket redeemed twice = %v, want ErrTicketNotFound", err)
	}
}

func TestWSTicketRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	rr := h.do(http.MethodPost, "/v1/auth/ws-ticket", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if h.tickets.Len() != 0 {
		t.Error("an unauthenticated request created a ticket")
	}
}

func TestWSTicketExpiresAfterSixtySeconds(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()

	rr := h.do(http.MethodPost, "/v1/auth/ws-ticket", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("ws-ticket: %d %s", rr.Code, rr.Body)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	late := testNow.Add(identity.WSTicketTTL + time.Second)
	if _, err := h.tickets.Redeem(t.Context(), body.Ticket, late); !errors.Is(err, identity.ErrTicketNotFound) {
		t.Errorf("redeeming a ticket %v after issue = %v, want ErrTicketNotFound",
			identity.WSTicketTTL+time.Second, err)
	}
}

// ---------------------------------------------------------------------------
// Storage failures
// ---------------------------------------------------------------------------

func TestStorageFailuresBecomeFiveHundredsWithoutLeakingDetail(t *testing.T) {
	// A 500 must never carry the underlying error. Database messages contain
	// table names, column names, and sometimes row values.
	h := newHarness(t)
	h.store.FailNext["EmailTaken"] = errors.New("pq: relation \"user_credentials\" does not exist")

	rr := h.do(http.MethodPost, "/v1/auth/register", registerBody())
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rr.Code, rr.Body)
	}
	p := problemOf(t, rr)
	if p.Code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", p.Code)
	}
	for _, leak := range []string{"pq:", "relation", "user_credentials"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("the 500 body leaked %q:\n%s", leak, rr.Body)
		}
	}
}

func TestTicketStoreFailureIsAFiveHundred(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()
	h.tickets.PutErr = errors.New("redis: connection refused")

	rr := h.do(http.MethodPost, "/v1/auth/ws-ticket", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "redis") {
		t.Errorf("the 500 body named the failing dependency:\n%s", rr.Body)
	}
}

func TestEveryProblemCarriesTheCallersTraceID(t *testing.T) {
	// §14.2: a user reporting "it failed" must be joinable to server logs via
	// the id their app already recorded. A generated id would look right and
	// be useless for that.
	h := newHarness(t)
	const clientTrace = "client-generated-trace-01H5"

	rr := h.do(http.MethodPost, "/v1/auth/login",
		map[string]any{"email": "nobody@example.com", "password": "wrong wrong wrong"},
		httpx.TraceHeader, clientTrace)

	p := problemOf(t, rr)
	if p.TraceID != clientTrace {
		t.Errorf("traceId = %q, want the client's %q", p.TraceID, clientTrace)
	}
	if got := rr.Header().Get(httpx.TraceHeader); got != clientTrace {
		t.Errorf("%s response header = %q, want %q", httpx.TraceHeader, got, clientTrace)
	}
}

func TestMeAfterTheAccountIsDeleted(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()

	var user map[string]any
	rr := h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if err := json.Unmarshal(rr.Body.Bytes(), &user); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	h.store.DeleteUser(user["id"].(string))

	// 401, not 404. The correct client response is to discard the token, not
	// to retry a resource that will never exist again.
	rr = h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("me for a deleted account = %d, want 401; body = %s", rr.Code, rr.Body)
	}
}

func TestMePropagatesAStorageFailure(t *testing.T) {
	h := newHarness(t)
	token, _, _ := h.registerAndAuth()
	h.store.FailNext["UserByID"] = fmt.Errorf("connection pool exhausted")

	rr := h.do(http.MethodGet, "/v1/auth/me", nil, "Authorization", "Bearer "+token)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body = %s", rr.Code, rr.Body)
	}
}
