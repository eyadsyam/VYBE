// Package rooms holds the room lifecycle domain: the state machine (FR-15),
// join codes (FR-12), capacity (FR-16), and host succession (FR-17, FR-18).
//
// Everything here is a pure function of its inputs. Persistence lives in the
// repository; this package is what the repository's decisions are *made of*,
// which is why it can be exhaustively tested with no database — a live
// Postgres is still an open blocker (BLOCKER-02) and the domain rules should
// never have been waiting on it.
package rooms

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
)

// JoinCodeLength is FR-12's six characters.
const JoinCodeLength = 6

// crockfordAlphabet is Crockford base32 (ADR-010): the digits and uppercase
// letters, minus I, L, O and U.
//
// The exclusions are not arbitrary. I and L are misread as 1, O as 0, and U is
// dropped so the encoding cannot accidentally spell an obscenity. This code is
// meant to be read aloud over a voice call and typed by the other person —
// which is exactly how VYBE rooms are shared — so transcription errors are the
// dominant failure mode, not collisions.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrInvalidJoinCode is returned by ParseJoinCode.
var ErrInvalidJoinCode = errors.New("rooms: join code is not valid")

// NewJoinCode generates a random code (FR-12).
//
// Uniqueness among non-ended rooms is a database constraint, not something
// this function can promise; the caller retries on conflict. The alphabet
// gives 32^6 ≈ 1.07 billion codes, so at any plausible number of live rooms a
// collision is rare enough that retrying is cheaper than coordinating.
//
// crypto/rand rather than math/rand: a predictable code would let somebody
// enumerate live rooms. FR-14 means possession of a code is not by itself
// access to a private room, so this is defence in depth rather than the only
// control — but guessable identifiers are how the depth gets used up.
func NewJoinCode() (string, error) {
	var sb strings.Builder
	sb.Grow(JoinCodeLength)

	max := big.NewInt(int64(len(crockfordAlphabet)))
	for range JoinCodeLength {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		sb.WriteByte(crockfordAlphabet[n.Int64()])
	}
	return sb.String(), nil
}

// ParseJoinCode normalises a user-entered code (FR-12).
//
// Crockford's decoding rules exist precisely for this moment: the user is
// typing something they heard, so accept what they plausibly meant.
//
//   - lowercase is uppercased
//   - I and L become 1, O becomes 0 — the substitutions people actually make
//   - hyphens and spaces are ignored, because users group characters
//
// This is forgiving on input and strict on output: it returns the canonical
// code or an error, never a half-cleaned string.
func ParseJoinCode(raw string) (string, error) {
	var sb strings.Builder
	sb.Grow(JoinCodeLength)

	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch r {
		case '-', ' ', '\t':
			continue // grouping, not content
		case 'I', 'L':
			sb.WriteByte('1')
		case 'O':
			sb.WriteByte('0')
		case 'U':
			// Deliberately NOT mapped. U is excluded from the alphabet, and
			// unlike I/L/O there is no digit it is plausibly a misreading of.
			// Silently turning it into something would invent a different room.
			return "", ErrInvalidJoinCode
		default:
			if !strings.ContainsRune(crockfordAlphabet, r) {
				return "", ErrInvalidJoinCode
			}
			sb.WriteRune(r)
		}
	}

	code := sb.String()
	if len(code) != JoinCodeLength {
		return "", ErrInvalidJoinCode
	}
	return code, nil
}

// ShareURL is FR-13's Universal/App Link.
//
// A https:// link, not a custom scheme. §1.9's L4 concern is deep-link
// hijacking: any app can claim `vybe://`, whereas a Universal Link is bound to
// a domain the attacker does not control. The custom scheme exists only as a
// fallback, and FR-14 is what actually closes the hole — the server authorises
// on resolve, so a hijacked link yields an attacker a code and no access.
func ShareURL(code string) string {
	return "https://vybe.app/r/" + code
}
