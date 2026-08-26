// Package passwords hashes and verifies user passwords with Argon2id.
//
// Argon2id rather than bcrypt because §12.1 names it, and the reason it names
// it is that bcrypt's 72-byte input truncation and fixed memory cost are both
// liabilities: the first silently discards a long passphrase's entropy, and
// the second means an attacker with a GPU gains ground every year while our
// cost stays put. Argon2id's memory parameter is what actually blunts that.
//
// The encoded hash carries its own parameters, so raising the cost later does
// not need a schema change or a migration — verification still succeeds
// against old hashes, and NeedsRehash reports which ones to upgrade on the
// next successful login.
package passwords

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters.
type Params struct {
	// Memory in KiB. The dominant defence: an attacker must pay this per
	// concurrent guess, and unlike CPU it cannot be traded away cheaply on a
	// GPU.
	Memory uint32
	// Iterations over that memory.
	Iterations uint32
	// Parallelism (lanes).
	Parallelism uint8
	// SaltLength in bytes.
	SaltLength uint32
	// KeyLength of the derived hash in bytes.
	KeyLength uint32
}

// DefaultParams follows the OWASP Password Storage Cheat Sheet's Argon2id
// recommendation of 19 MiB.
//
// The tempting move is to raise iterations instead of memory, because it is
// the number that sounds like effort. It is the wrong knob: memory is what an
// attacker cannot parallelise away, so a "t=10, m=4MiB" configuration is far
// weaker than this one despite doing five times the work per guess.
//
// Parallelism is deliberately 1 rather than NumCPU. A server hashing under
// load wants each hash to occupy one predictable slot; lanes multiply the
// concurrency the process must schedule and make p99 login latency depend on
// how busy the box happens to be.
var DefaultParams = Params{
	Memory:      19 * 1024,
	Iterations:  2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

var (
	// ErrInvalidHash means the stored string is not a PHC-format Argon2id hash.
	ErrInvalidHash = errors.New("passwords: hash is not in the expected format")
	// ErrIncompatibleVersion means the hash was produced by a different
	// Argon2 version than this binary understands.
	ErrIncompatibleVersion = errors.New("passwords: incompatible argon2 version")
	// ErrMismatch means the password does not match the hash. This is the only
	// error a caller should ever surface to a user, and even then only as a
	// generic failure — see the note on Verify.
	ErrMismatch = errors.New("passwords: password does not match")
)

// Hash derives an Argon2id hash and returns it in PHC string format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
func Hash(password string, p Params) (string, error) {
	if p.SaltLength == 0 || p.KeyLength == 0 {
		return "", fmt.Errorf("passwords: salt and key lengths must both be positive")
	}
	if p.Memory == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		// argon2.IDKey panics on a zero cost parameter. Refusing here turns a
		// process-killing panic into a returned error.
		return "", fmt.Errorf("passwords: cost parameters must all be positive")
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		// Never fall back to a weaker source. A predictable salt collapses the
		// whole password table into one rainbow-table lookup.
		return "", fmt.Errorf("passwords: reading salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches encoded.
//
// Returns ErrMismatch for a wrong password and a different error for a corrupt
// hash. Callers must NOT distinguish the two to the user: "that account exists
// but its stored hash is broken" is an information leak, and the login handler
// collapses every failure into one message with one timing profile.
func Verify(password, encoded string) error {
	p, salt, want, err := Decode(encoded)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	// Constant time: a byte-by-byte comparison leaks the length of the correct
	// prefix through timing, which is enough to recover the hash given enough
	// attempts.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was produced with weaker
// parameters than the current policy.
//
// Called after a SUCCESSFUL login, the only moment the plaintext is available
// to rehash with. Skipping it means a cost increase applies only to new
// accounts, leaving the oldest — and most valuable — hashes at the weakest
// setting forever.
func NeedsRehash(encoded string, want Params) bool {
	got, _, _, err := Decode(encoded)
	if err != nil {
		// An unparseable hash cannot be verified against either, so the user
		// is about to fail login regardless. True is the safe direction: it
		// can only cause an upgrade, never a downgrade.
		return true
	}
	return got.Memory < want.Memory ||
		got.Iterations < want.Iterations ||
		got.Parallelism != want.Parallelism ||
		got.KeyLength < want.KeyLength
}

// Decode parses a PHC-format Argon2id string.
func Decode(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		// argon2i and argon2d are real algorithms, but we do not issue them,
		// so accepting one would mean verifying against a variant whose
		// tradeoffs we never chose.
		return p, nil, nil, fmt.Errorf("%w: variant %q is not argon2id", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: hash is v%d, this build is v%d",
			ErrIncompatibleVersion, version, argon2.Version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.Memory == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		// argon2.IDKey panics on a zero parameter, so a malformed hash would
		// take the process down rather than fail one login.
		return p, nil, nil, fmt.Errorf("%w: zero cost parameter", ErrInvalidHash)
	}

	if salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, fmt.Errorf("%w: empty salt or key", ErrInvalidHash)
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}

// DummyHash is verified against when no account matches, so a login attempt
// for a non-existent email costs the same as one for a real account.
//
// Without it, response time alone enumerates the user table: a miss returns in
// microseconds and a hit takes however long Argon2id takes. That is a
// user-enumeration oracle, and no amount of careful wording in the error
// message closes it.
var DummyHash = func() string {
	h, err := Hash("vybe-timing-equaliser", DefaultParams)
	if err != nil {
		// Unreachable: Hash only fails if crypto/rand fails, and a process
		// with no random source cannot serve authentication at all.
		panic("passwords: cannot derive the timing-equaliser hash: " + err.Error())
	}
	return h
}()

// TestParams are deliberately weak, for tests only.
//
// Exported so test binaries in other packages can hash quickly. Using
// DefaultParams in a fifty-case table test spends fifty times 19 MiB and turns
// a fast suite into a slow one — which is how cost parameters end up quietly
// lowered in production "to make the tests pass".
var TestParams = Params{
	Memory:      8,
	Iterations:  1,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}
