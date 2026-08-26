package passwords

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashThenVerifyRoundTrips(t *testing.T) {
	for _, pw := range []string{
		"correct horse battery staple",
		"كلمة سر عربية طويلة جدا",    // Arabic: the launch locale
		"emoji password 🎬🍿 with mix", // multi-byte, above the BMP
		strings.Repeat("a", 256),     // the policy maximum
		" leading and trailing  ",    // whitespace is significant
	} {
		encoded, err := Hash(pw, TestParams)
		if err != nil {
			t.Fatalf("Hash(%q): %v", pw, err)
		}
		if err := Verify(pw, encoded); err != nil {
			t.Errorf("Verify(%q) against its own hash: %v", pw, err)
		}
	}
}

func TestPasswordsLongerThan72BytesAreNotTruncated(t *testing.T) {
	// This is the bcrypt failure §12.1 avoids by naming Argon2id. bcrypt
	// silently ignores everything past byte 72, so these two passphrases
	// would hash identically and either would unlock the account.
	base := strings.Repeat("x", 72)
	a, err := Hash(base+"AAAA", TestParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(base+"BBBB", a); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a password differing only past byte 72 verified; got %v, want ErrMismatch", err)
	}
}

func TestSaltIsRandomPerHash(t *testing.T) {
	// Same password, different hashes. Equal encodings would mean a fixed
	// salt, which makes the whole table one precomputation away from open.
	const pw = "the same password twice"
	seen := map[string]bool{}
	for range 20 {
		h, err := Hash(pw, TestParams)
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if seen[h] {
			t.Fatal("two hashes of the same password were identical; the salt is not random")
		}
		seen[h] = true

		// Each must still verify — a random salt is only useful if it round-trips.
		if err := Verify(pw, h); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	encoded, err := Hash("the real password", TestParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, wrong := range []string{
		"",
		"the real passwor",  // prefix
		"the real password", // exact — control, handled below
		"The Real Password", // case
		"the real password ",
	} {
		err := Verify(wrong, encoded)
		if wrong == "the real password" {
			if err != nil {
				t.Errorf("the correct password was rejected: %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrMismatch) {
			t.Errorf("Verify(%q) = %v, want ErrMismatch", wrong, err)
		}
	}
}

func TestEncodedFormatIsPHC(t *testing.T) {
	encoded, err := Hash("whatever", DefaultParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// The parameters must travel with the hash; that is what lets the cost be
	// raised later without a migration.
	wantPrefix := "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(encoded, wantPrefix) {
		t.Errorf("encoded = %q, want prefix %q", encoded, wantPrefix)
	}
	if n := strings.Count(encoded, "$"); n != 5 {
		t.Errorf("encoded has %d separators, want 5: %q", n, encoded)
	}
}

func TestDecodeRoundTripsParams(t *testing.T) {
	encoded, err := Hash("x", DefaultParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	got, salt, key, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != DefaultParams {
		t.Errorf("Decode params = %+v, want %+v", got, DefaultParams)
	}
	if uint32(len(salt)) != DefaultParams.SaltLength {
		t.Errorf("salt is %d bytes, want %d", len(salt), DefaultParams.SaltLength)
	}
	if uint32(len(key)) != DefaultParams.KeyLength {
		t.Errorf("key is %d bytes, want %d", len(key), DefaultParams.KeyLength)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	valid, err := Hash("x", TestParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	parts := strings.Split(valid, "$")

	tests := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrInvalidHash},
		{"not a hash at all", "hunter2", ErrInvalidHash},
		{"bcrypt hash", "$2a$10$abcdefghijklmnopqrstuv", ErrInvalidHash},
		{"too few fields", "$argon2id$v=19$m=8,t=1,p=1$c2FsdA", ErrInvalidHash},
		{"too many fields", valid + "$extra", ErrInvalidHash},
		{"missing leading separator", strings.TrimPrefix(valid, "$"), ErrInvalidHash},
		// argon2i is a real variant, but not the one we chose. Accepting it
		// would silently verify against different security properties.
		{"argon2i variant", "$argon2i$" + strings.Join(parts[2:], "$"), ErrInvalidHash},
		{"argon2d variant", "$argon2d$" + strings.Join(parts[2:], "$"), ErrInvalidHash},
		{"unparseable version", "$argon2id$v=abc$" + strings.Join(parts[3:], "$"), ErrInvalidHash},
		{"unparseable params", "$argon2id$v=19$nonsense$" + strings.Join(parts[4:], "$"), ErrInvalidHash},
		// A zero cost parameter would make argon2.IDKey PANIC. Catching it
		// here turns a process kill into one failed login.
		{"zero memory", "$argon2id$v=19$m=0,t=1,p=1$" + strings.Join(parts[4:], "$"), ErrInvalidHash},
		{"zero iterations", "$argon2id$v=19$m=8,t=0,p=1$" + strings.Join(parts[4:], "$"), ErrInvalidHash},
		{"zero parallelism", "$argon2id$v=19$m=8,t=1,p=0$" + strings.Join(parts[4:], "$"), ErrInvalidHash},
		{"salt is not base64", "$argon2id$v=19$m=8,t=1,p=1$!!!!$" + parts[5], ErrInvalidHash},
		{"key is not base64", "$argon2id$v=19$m=8,t=1,p=1$" + parts[4] + "$!!!!", ErrInvalidHash},
		{"empty salt", "$argon2id$v=19$m=8,t=1,p=1$$" + parts[5], ErrInvalidHash},
		{"empty key", "$argon2id$v=19$m=8,t=1,p=1$" + parts[4] + "$", ErrInvalidHash},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := Decode(tt.in); !errors.Is(err, tt.want) {
				t.Errorf("Decode(%q) = %v, want %v", tt.in, err, tt.want)
			}
			// Verify must fail the same way rather than panicking.
			if err := Verify("anything", tt.in); err == nil {
				t.Errorf("Verify against %q succeeded; want an error", tt.in)
			}
		})
	}
}

func TestDecodeRejectsAnotherArgon2Version(t *testing.T) {
	// Argon2 v0x10 and v0x13 derive different keys from identical inputs, so
	// verifying a v16 hash with a v19 implementation silently always fails.
	// Saying so explicitly is the difference between a diagnosable error and
	// "the password is right but login is broken".
	other := argon2.Version - 3
	encoded := "$argon2id$v=" + itoa(other) + "$m=8,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5"
	if _, _, _, err := Decode(encoded); !errors.Is(err, ErrIncompatibleVersion) {
		t.Errorf("Decode of a v%d hash = %v, want ErrIncompatibleVersion", other, err)
	}
}

func TestNeedsRehashDetectsWeakerParams(t *testing.T) {
	weak := Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	encoded, err := Hash("x", weak)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if !NeedsRehash(encoded, DefaultParams) {
		t.Error("a hash at 1 MiB was not flagged for rehash against the 19 MiB policy")
	}

	current, err := Hash("x", DefaultParams)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if NeedsRehash(current, DefaultParams) {
		t.Error("a hash at the current policy was flagged for rehash")
	}

	// Stronger than policy is not a reason to rehash — that would DOWNGRADE it.
	stronger := DefaultParams
	stronger.Memory *= 2
	strong, err := Hash("x", stronger)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if NeedsRehash(strong, DefaultParams) {
		t.Error("a stronger-than-policy hash was flagged for rehash; that would weaken it")
	}
}

func TestNeedsRehashOnGarbageIsTrue(t *testing.T) {
	// Fails safe. The login is going to fail anyway, and true can only cause
	// an upgrade.
	if !NeedsRehash("not a hash", DefaultParams) {
		t.Error("an unparseable hash should be flagged for rehash")
	}
}

func TestDummyHashIsVerifiableAndNeverMatchesRealInput(t *testing.T) {
	// The login handler verifies against this when the email is unknown, so
	// it must cost the same as a real verification — meaning it must be a
	// well-formed hash at the REAL parameters, not a placeholder string.
	p, _, _, err := Decode(DummyHash)
	if err != nil {
		t.Fatalf("DummyHash does not decode: %v", err)
	}
	if p != DefaultParams {
		t.Errorf("DummyHash uses %+v; it must use DefaultParams %+v or it will not equalise timing",
			p, DefaultParams)
	}
	if err := Verify("some user's actual password", DummyHash); !errors.Is(err, ErrMismatch) {
		t.Errorf("verifying arbitrary input against DummyHash = %v, want ErrMismatch", err)
	}
}

func TestHashRejectsDegenerateParams(t *testing.T) {
	// Returning an error rather than panicking. argon2.IDKey panics on a zero
	// memory or iteration count, and a panic in the signup path is an outage.
	for _, tt := range []struct {
		name string
		p    Params
	}{
		{"zero salt length", Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 0, KeyLength: 32}},
		{"zero key length", Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 0}},
		{"zero memory", Params{Memory: 0, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}},
		{"zero iterations", Params{Memory: 8, Iterations: 0, Parallelism: 1, SaltLength: 16, KeyLength: 32}},
		{"zero parallelism", Params{Memory: 8, Iterations: 1, Parallelism: 0, SaltLength: 16, KeyLength: 32}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Hash("x", tt.p); err == nil {
				t.Error("Hash accepted a degenerate parameter set; want an error")
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
