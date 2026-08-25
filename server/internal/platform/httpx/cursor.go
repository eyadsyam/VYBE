package httpx

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FR-59: all list endpoints use opaque cursor pagination; offset pagination
// must not be used.
//
// The reason is correctness, not fashion. Every list in VYBE is ordered by
// recency and mutates while a user reads it — chat, room lists, the activity
// feed. With OFFSET, a row inserted above the window shifts everything down and
// page 2 silently repeats an item from page 1, while a deletion silently skips
// one. Keyset pagination anchors to a row the client has already seen, so
// concurrent writes cannot corrupt the sequence.

// Cursor is a keyset position: the sort key plus a tiebreaker.
//
// CreatedAt alone is not unique — UUIDv7 ids are generated at the same
// microsecond under load (ADR-010) — so ID breaks ties and guarantees a total
// order. Without it a page boundary landing inside a group of equal timestamps
// drops or repeats rows.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

const cursorSep = "\x1f" // ASCII unit separator: cannot occur in a UUID or RFC 3339 timestamp

// Encode renders the cursor as an opaque token.
//
// "Opaque" is a contract, not encryption: base64url of the keyset tuple. The
// client must echo it back unmodified and must not construct one. It is not
// signed because it carries nothing secret — a forged cursor can only select a
// different page of data the caller is already authorised to read, and
// authorisation is enforced by the query, never by the cursor.
func (c Cursor) Encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSep + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by Encode.
func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrCursorInvalid.WithCause(err)
	}
	parts := strings.SplitN(string(raw), cursorSep, 2)
	if len(parts) != 2 || parts[1] == "" {
		return Cursor{}, ErrCursorInvalid
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return Cursor{}, ErrCursorInvalid.WithCause(err)
	}
	return Cursor{CreatedAt: ts.UTC(), ID: parts[1]}, nil
}

// PageParams is a validated page request.
type PageParams struct {
	Limit  int
	Cursor *Cursor // nil on the first page
}

// PageRequest is the query-string contract for every list endpoint.
const (
	QueryCursor = "cursor"
	QueryLimit  = "limit"
)

// ParsePageParams reads and validates ?cursor= and ?limit=.
//
// It also actively rejects ?offset= and ?page=. FR-59 says offset pagination
// must not be used, and a requirement that is only documented gets violated by
// the next client written against a half-remembered convention. Failing loudly
// with an explanation turns that into a five-second fix instead of a silently
// ignored parameter and a paginated list that appears to work.
func ParsePageParams(r *http.Request, defaultLimit, maxLimit int) (PageParams, error) {
	q := r.URL.Query()

	for _, banned := range []string{"offset", "page", "skip"} {
		if q.Has(banned) {
			return PageParams{}, ErrOffsetPagination.WithDetail(
				"The %q parameter is not supported. This API uses opaque cursor pagination: pass ?%s= with the value from the previous page.",
				banned, QueryCursor)
		}
	}

	params := PageParams{Limit: defaultLimit}

	if raw := q.Get(QueryLimit); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return PageParams{}, ErrLimitInvalid.WithDetail("limit %q is not a positive integer.", raw)
		}
		if n > maxLimit {
			// Clamping silently would make a client believe it received
			// everything it asked for. Refusing states the ceiling instead.
			return PageParams{}, ErrLimitInvalid.WithDetail("limit %d exceeds the maximum of %d.", n, maxLimit)
		}
		params.Limit = n
	}

	if raw := q.Get(QueryCursor); raw != "" {
		c, err := DecodeCursor(raw)
		if err != nil {
			return PageParams{}, err
		}
		params.Cursor = &c
	}

	return params, nil
}

// Page is the list envelope every paginated endpoint returns.
//
// NextCursor is empty on the last page. There is deliberately no total count:
// producing one costs a second full scan on every request, and no V1 screen
// displays it.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// NewPage builds the envelope from one extra row.
//
// The caller queries limit+1 rows. If the extra row came back there is another
// page; it is dropped from the payload and its predecessor becomes the cursor.
// This detects "is there more" without a COUNT query, which is the whole point.
func NewPage[T any](rows []T, limit int, key func(T) Cursor) Page[T] {
	if limit <= 0 || len(rows) <= limit {
		return Page[T]{Items: emptyIfNil(rows)}
	}
	items := rows[:limit]
	return Page[T]{
		Items:      items,
		NextCursor: key(items[len(items)-1]).Encode(),
	}
}

// emptyIfNil guarantees `"items": []` rather than `"items": null`.
//
// A null here is a real client bug generator: Dart's List<T> decode throws on
// null where it accepts an empty list, so an empty page would crash the app
// rather than render the §3.2 empty state.
func emptyIfNil[T any](rows []T) []T {
	if rows == nil {
		return []T{}
	}
	return rows
}
