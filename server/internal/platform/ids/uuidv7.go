// Package ids mints the identifiers used for every persisted entity.
//
// ADR-010 chose UUIDv7 for index write locality on the hot append-only tables
// (room_events above all), while keeping IDs non-enumerable.
//
// We implement RFC 9562 §5.7 here rather than calling a library's NewV7 so
// that the monotonicity guarantee is ours, explicit, and directly tested. That
// guarantee is not decorative: room_events ordering leans on it, and two IDs
// minted in the same millisecond must still sort in creation order.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// UUID is a 128-bit identifier in its canonical binary form.
type UUID [16]byte

// Nil is the zero UUID. It is never a valid entity ID.
var Nil UUID

// generator holds the state needed to keep IDs monotonic inside a millisecond.
//
// RFC 9562 calls the 12 bits after the version nibble "rand_a" and explicitly
// permits using them as a sub-millisecond counter (method 2). We do exactly
// that: within one millisecond the counter increments, so ordering is total
// rather than merely probable.
type generator struct {
	mu      sync.Mutex
	lastMS  int64
	counter uint16 // 12 bits used; 4096 IDs per millisecond before rollover
	now     func() time.Time
	randRead func([]byte) (int, error)
}

var defaultGen = &generator{
	now:      time.Now,
	randRead: rand.Read,
}

// New returns a new UUIDv7.
//
// It panics if the system CSPRNG fails. That is deliberate: an ID source that
// has silently stopped being random is not a condition to paper over with a
// fallback, and every call site would otherwise have to handle an error that
// can only occur when the machine is already broken.
func New() UUID {
	u, err := defaultGen.next()
	if err != nil {
		panic("ids: CSPRNG unavailable: " + err.Error())
	}
	return u
}

func (g *generator) next() (UUID, error) {
	var u UUID

	// Fill the whole value with randomness first, then overwrite the
	// structured prefix. This guarantees rand_b is random even if we later
	// change the layout above it.
	if _, err := g.randRead(u[:]); err != nil {
		return Nil, err
	}

	g.mu.Lock()
	ms := g.now().UnixMilli()
	switch {
	case ms > g.lastMS:
		g.lastMS = ms
		// Start the counter somewhere random in the lower half of its range.
		// Starting at zero would leak "this was the first ID of the
		// millisecond", and starting high would shorten the runway before
		// rollover.
		g.counter = binary.BigEndian.Uint16(u[6:8]) & 0x07FF
	case ms == g.lastMS:
		g.counter++
		if g.counter > 0x0FFF {
			// More than 4096 IDs in one millisecond. Rather than break
			// monotonicity, borrow from the next millisecond and reset. The
			// timestamp is then at most a few ms ahead of the wall clock,
			// which is harmless and self-correcting.
			g.lastMS++
			ms = g.lastMS
			g.counter = 0
		}
	default:
		// Clock moved backwards (NTP step, VM migration). Never emit a
		// smaller timestamp than one we have already issued — that would
		// silently break the ordering room_events depends on. Hold the last
		// millisecond and keep counting.
		ms = g.lastMS
		g.counter++
		if g.counter > 0x0FFF {
			g.lastMS++
			ms = g.lastMS
			g.counter = 0
		}
	}
	counter := g.counter
	g.mu.Unlock()

	// Bytes 0..5: 48-bit big-endian Unix timestamp in milliseconds.
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)

	// Byte 6: high nibble is version 7; low nibble is the top 4 bits of the
	// 12-bit counter.
	u[6] = 0x70 | byte((counter>>8)&0x0F)
	// Byte 7: remaining 8 bits of the counter.
	u[7] = byte(counter)

	// Byte 8: top two bits are the RFC 4122 variant (0b10); the rest stays
	// random.
	u[8] = (u[8] & 0x3F) | 0x80

	return u, nil
}

// Timestamp returns the creation time encoded in the ID.
//
// Note the privacy consequence, recorded in ADR-010: a UUIDv7 leaks when it
// was created. That is fine for our entities and wrong for a security token,
// which is why password reset tokens are random instead.
func (u UUID) Timestamp() time.Time {
	ms := int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 |
		int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
	return time.UnixMilli(ms).UTC()
}

// Version returns the UUID version nibble. Always 7 for IDs we mint.
func (u UUID) Version() int { return int(u[6] >> 4) }

// IsNil reports whether u is the zero UUID.
func (u UUID) IsNil() bool { return u == Nil }

// String renders the canonical 8-4-4-4-12 lowercase hex form.
func (u UUID) String() string {
	var buf [36]byte
	hex := "0123456789abcdef"
	i := 0
	for b := 0; b < 16; b++ {
		if b == 4 || b == 6 || b == 8 || b == 10 {
			buf[i] = '-'
			i++
		}
		buf[i] = hex[u[b]>>4]
		buf[i+1] = hex[u[b]&0x0F]
		i += 2
	}
	return string(buf[:])
}

// Parse reads a canonical UUID string. It accepts only the 36-character
// hyphenated form — being liberal here (braces, urn: prefixes, no hyphens)
// would mean two spellings of the same ID could reach the database and defeat
// a unique constraint.
func Parse(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 {
		return Nil, fmt.Errorf("ids: want 36 characters, got %d", len(s))
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return Nil, fmt.Errorf("ids: malformed UUID %q", s)
	}
	src := []byte(s)
	pos := 0
	for _, span := range [...]struct{ start, n int }{{0, 8}, {9, 4}, {14, 4}, {19, 4}, {24, 12}} {
		for i := 0; i < span.n; i += 2 {
			hi, ok1 := unhex(src[span.start+i])
			lo, ok2 := unhex(src[span.start+i+1])
			if !ok1 || !ok2 {
				return Nil, fmt.Errorf("ids: non-hex character in %q", s)
			}
			u[pos] = hi<<4 | lo
			pos++
		}
	}
	return u, nil
}

// MustParse is Parse for constants and test fixtures, where a malformed
// literal is a programming error rather than bad input.
func MustParse(s string) UUID {
	u, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// --- database/sql and JSON plumbing -----------------------------------------

func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

func (u *UUID) UnmarshalText(b []byte) error {
	parsed, err := Parse(string(b))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// Value implements driver.Valuer. We hand pgx the 16-byte array so the column
// stays native `uuid` rather than becoming a 36-byte string (ADR-010).
func (u UUID) Value() (any, error) { return u[:], nil }

// Scan implements sql.Scanner for both the binary and text wire forms.
func (u *UUID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*u = Nil
		return nil
	case [16]byte:
		*u = v
		return nil
	case []byte:
		if len(v) == 16 {
			copy(u[:], v)
			return nil
		}
		return u.UnmarshalText(v)
	case string:
		return u.UnmarshalText([]byte(v))
	}
	return fmt.Errorf("ids: cannot scan %T into UUID", src)
}
