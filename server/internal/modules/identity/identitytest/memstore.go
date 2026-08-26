// Package identitytest provides an in-memory identity.Repository.
//
// It lives in its own package, rather than beside the production code, so that
// §13.3 is enforceable by inspection: nothing under cmd/ imports this path, and
// a grep proves it. An in-memory user store that could be reached in a release
// build is not a convenience — it is an authentication bypass with a friendly
// name.
//
// The store is safe for concurrent use because the refresh-rotation tests
// deliberately race two goroutines through MarkRotated, which is the whole
// point of that method returning a boolean.
package identitytest

import (
	"context"
	"sync"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
)

// Store is an in-memory identity.Repository.
type Store struct {
	mu sync.Mutex

	users    map[string]*identity.User        // by id
	creds    map[string]*identity.Credentials // by lowercased email
	handles  map[string]string                // handle -> user id
	sessions map[string]*identity.Session
	families map[string]*family
	tokens   map[string]*identity.RefreshTokenState // by hex(hash)

	// FailNext, when set, makes the next call to the named method return it.
	// Used to exercise the handler's 500 path without a broken database.
	FailNext map[string]error

	// BeforeMarkRotated runs at the top of MarkRotated, before the lock is
	// taken. It exists so a test can force the interleaving that the
	// conditional update guards against.
	//
	// Without a hook like this the race is unobservable: every goroutine
	// completes its whole read-decide-write before the next one starts, so the
	// second caller sees an already-rotated token and takes the overlap-replay
	// path instead. The bug this method's boolean prevents — two callers both
	// reading rotated_at IS NULL and both minting a successor — then never
	// happens, and a test asserting on it passes whether or not the guard is
	// there. A barrier here makes it happen every run.
	BeforeMarkRotated func()
}

type family struct {
	id            string
	userID        string
	sessionID     string
	revokedAt     *time.Time
	revokedReason string
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		users:    map[string]*identity.User{},
		creds:    map[string]*identity.Credentials{},
		handles:  map[string]string{},
		sessions: map[string]*identity.Session{},
		families: map[string]*family{},
		tokens:   map[string]*identity.RefreshTokenState{},
		FailNext: map[string]error{},
	}
}

func (s *Store) fail(method string) error {
	if err, ok := s.FailNext[method]; ok {
		delete(s.FailNext, method)
		return err
	}
	return nil
}

func key(hash []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(hash)*2)
	for _, b := range hash {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out)
}

// CreateUser inserts the user and its credentials atomically.
func (s *Store) CreateUser(_ context.Context, u *identity.User, email, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("CreateUser"); err != nil {
		return err
	}
	// The unique constraints the real schema enforces. Re-checking here is not
	// redundant: the service's pre-check races, and a fake that never conflicts
	// would let a test pass that production would reject.
	if _, exists := s.creds[email]; exists {
		return identity.ErrEmailTaken
	}
	if _, exists := s.handles[u.Handle]; exists {
		return identity.ErrHandleTaken
	}
	copied := *u
	s.users[u.ID] = &copied
	s.creds[email] = &identity.Credentials{UserID: u.ID, Email: email, PasswordHash: passwordHash}
	s.handles[u.Handle] = u.ID
	return nil
}

// UserByEmail returns the user and credentials, or (nil, nil, nil) when absent.
func (s *Store) UserByEmail(_ context.Context, email string) (*identity.User, *identity.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("UserByEmail"); err != nil {
		return nil, nil, err
	}
	c, ok := s.creds[email]
	if !ok {
		// Not an error. "No such account" is an ordinary outcome the service
		// must handle without distinguishing it to the caller.
		return nil, nil, nil
	}
	u := s.users[c.UserID]
	uc, cc := *u, *c
	return &uc, &cc, nil
}

// UserByID returns the user, or (nil, nil) when absent.
func (s *Store) UserByID(_ context.Context, id string) (*identity.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("UserByID"); err != nil {
		return nil, err
	}
	u, ok := s.users[id]
	if !ok {
		return nil, nil
	}
	c := *u
	return &c, nil
}

// HandleTaken reports whether a handle is in use.
func (s *Store) HandleTaken(_ context.Context, handle string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("HandleTaken"); err != nil {
		return false, err
	}
	_, ok := s.handles[handle]
	return ok, nil
}

// EmailTaken reports whether an email is registered.
func (s *Store) EmailTaken(_ context.Context, email string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("EmailTaken"); err != nil {
		return false, err
	}
	_, ok := s.creds[email]
	return ok, nil
}

// UpdatePasswordHash rewrites a stored hash.
func (s *Store) UpdatePasswordHash(_ context.Context, userID, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("UpdatePasswordHash"); err != nil {
		return err
	}
	for _, c := range s.creds {
		if c.UserID == userID {
			c.PasswordHash = hash
			return nil
		}
	}
	return nil
}

// CreateSession inserts a session.
func (s *Store) CreateSession(_ context.Context, sess *identity.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("CreateSession"); err != nil {
		return err
	}
	c := *sess
	s.sessions[sess.ID] = &c
	return nil
}

// SessionByID returns a session, or (nil, nil) when absent.
func (s *Store) SessionByID(_ context.Context, id string) (*identity.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("SessionByID"); err != nil {
		return nil, err
	}
	sess, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	c := *sess
	return &c, nil
}

// RevokeSession marks a session revoked. Idempotent.
func (s *Store) RevokeSession(_ context.Context, sessionID, reason string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RevokeSession"); err != nil {
		return err
	}
	sess, ok := s.sessions[sessionID]
	if !ok {
		// Revoking an unknown session succeeds. FR-5's logout must be
		// idempotent, and a client retrying after a timeout should not be told
		// its successful request failed.
		return nil
	}
	if sess.RevokedAt == nil {
		t := at
		sess.RevokedAt = &t
	}
	// Revoke the families bound to it, or a live refresh token outlives the
	// logout that was supposed to kill it.
	for _, f := range s.families {
		if f.sessionID == sessionID && f.revokedAt == nil {
			t := at
			f.revokedAt = &t
			f.revokedReason = reason
			s.propagateRevocationLocked(f)
		}
	}
	return nil
}

// CreateFamily opens a refresh-token family.
func (s *Store) CreateFamily(_ context.Context, familyID, userID, sessionID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("CreateFamily"); err != nil {
		return err
	}
	s.families[familyID] = &family{id: familyID, userID: userID, sessionID: sessionID}
	return nil
}

// RevokeFamily revokes every token in a family.
func (s *Store) RevokeFamily(_ context.Context, familyID, reason string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RevokeFamily"); err != nil {
		return err
	}
	f, ok := s.families[familyID]
	if !ok {
		return nil
	}
	if f.revokedAt == nil {
		t := at
		f.revokedAt = &t
		f.revokedReason = reason
	}
	s.propagateRevocationLocked(f)
	return nil
}

// propagateRevocationLocked stamps the family's revocation onto its tokens.
//
// The real schema reaches this through a join, so the token rows carry no
// revocation column of their own. Here the denormalised copy has to be kept in
// step, or EvaluateRefresh would never see RefreshFamilyRevoked.
func (s *Store) propagateRevocationLocked(f *family) {
	for _, t := range s.tokens {
		if t.FamilyID == f.id {
			t.FamilyRevokedAt = f.revokedAt
			t.FamilyRevokedWhy = f.revokedReason
		}
	}
}

// InsertRefreshToken stores a new token.
func (s *Store) InsertRefreshToken(_ context.Context, _, familyID string, hash []byte, _, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("InsertRefreshToken"); err != nil {
		return err
	}
	f, ok := s.families[familyID]
	if !ok {
		return nil
	}
	st := &identity.RefreshTokenState{
		TokenHash:        append([]byte(nil), hash...),
		FamilyID:         familyID,
		SessionID:        f.sessionID,
		UserID:           f.userID,
		ExpiresAt:        expiresAt,
		FamilyRevokedAt:  f.revokedAt,
		FamilyRevokedWhy: f.revokedReason,
	}
	s.tokens[key(hash)] = st
	return nil
}

// RefreshTokenByHash returns a token's state, or (nil, nil) when unknown.
func (s *Store) RefreshTokenByHash(_ context.Context, hash []byte) (*identity.RefreshTokenState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RefreshTokenByHash"); err != nil {
		return nil, err
	}
	t, ok := s.tokens[key(hash)]
	if !ok {
		return nil, nil
	}
	c := *t
	return &c, nil
}

// MarkRotated stamps rotation and reports whether it changed anything.
//
// The `rotatedAt == nil` guard is the concurrency contract: it makes this the
// equivalent of `UPDATE ... WHERE rotated_at IS NULL`, so exactly one of two
// racing refreshes wins. A fake that always returned true would hide the very
// bug the boolean exists to prevent.
func (s *Store) MarkRotated(_ context.Context, hash []byte, rotatedAt, validUntilOverlap time.Time) (bool, error) {
	// Before the lock, so a barrier here does not deadlock the other callers
	// trying to reach it.
	if s.BeforeMarkRotated != nil {
		s.BeforeMarkRotated()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("MarkRotated"); err != nil {
		return false, err
	}
	t, ok := s.tokens[key(hash)]
	if !ok || t.RotatedAt != nil {
		return false, nil
	}
	r, v := rotatedAt, validUntilOverlap
	t.RotatedAt = &r
	t.ValidUntilOverlap = &v
	return true, nil
}

// ---------------------------------------------------------------------------
// Inspection helpers for assertions
// ---------------------------------------------------------------------------

// SessionRevoked reports whether a session has been revoked.
func (s *Store) SessionRevoked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return ok && sess.RevokedAt != nil
}

// FamilyRevokedReason returns why a family was revoked, or "" if it is live.
func (s *Store) FamilyRevokedReason(familyID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.families[familyID]
	if !ok || f.revokedAt == nil {
		return ""
	}
	return f.revokedReason
}

// TokenCount reports how many refresh tokens exist, across all families.
func (s *Store) TokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// OnlyFamilyID returns the id of the single family, failing if there is not
// exactly one. Tests that create one account should not have to thread the
// generated id through the service's return value.
func (s *Store) OnlyFamilyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.families) != 1 {
		return ""
	}
	for id := range s.families {
		return id
	}
	return ""
}

var _ identity.Repository = (*Store)(nil)

// DeleteUser removes a user row, leaving its sessions and tokens behind.
//
// Models §6.5's hard delete, which is exactly the state that makes the
// "user is gone but the refresh token has not expired" path reachable.
func (s *Store) DeleteUser(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[id]; ok {
		delete(s.handles, u.Handle)
	}
	delete(s.users, id)
	for email, c := range s.creds {
		if c.UserID == id {
			delete(s.creds, email)
		}
	}
}
