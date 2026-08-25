package httpx

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCursorRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		cur  Cursor
	}{
		{"typical", Cursor{CreatedAt: time.Date(2026, 8, 25, 7, 0, 29, 123456789, time.UTC), ID: "0192f0c1-8a3e-7c4d-9b2a-1f5e6d7c8b9a"}},
		{"zero time", Cursor{CreatedAt: time.Time{}.UTC(), ID: "x"}},
		{"sub-millisecond precision", Cursor{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC), ID: "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeCursor(tt.cur.Encode())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.CreatedAt.Equal(tt.cur.CreatedAt) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, tt.cur.CreatedAt)
			}
			if got.ID != tt.cur.ID {
				t.Errorf("ID = %q, want %q", got.ID, tt.cur.ID)
			}
		})
	}
}

func TestCursorPreservesNanosecondsForTieBreaking(t *testing.T) {
	// UUIDv7 ids are minted within the same millisecond under load (ADR-010).
	// If Encode truncated to seconds, two adjacent rows would produce the same
	// cursor and a page boundary would drop or repeat rows.
	a := Cursor{CreatedAt: time.Date(2026, 8, 25, 7, 0, 29, 100000000, time.UTC), ID: "a"}
	b := Cursor{CreatedAt: time.Date(2026, 8, 25, 7, 0, 29, 200000000, time.UTC), ID: "a"}
	if a.Encode() == b.Encode() {
		t.Fatal("cursors 100ms apart encoded identically; sub-second precision was lost")
	}
}

func TestCursorIsOpaqueAndRejectsTampering(t *testing.T) {
	tests := []struct{ name, token string }{
		{"not base64", "!!!!not-base64!!!!"},
		{"base64 but no separator", "aGVsbG8"}, // "hello"
		{"empty string handled by caller", ""},
		{"bad timestamp", encodeRaw("not-a-time\x1fid")},
		{"empty id after separator", encodeRaw("2026-08-25T00:00:00Z\x1f")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeCursor(tt.token); err == nil {
				t.Errorf("DecodeCursor(%q) succeeded; want an error", tt.token)
			}
		})
	}
}

// encodeRaw builds a token from arbitrary bytes, so the decoder can be tested
// against payloads Encode would never produce.
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestParsePageParamsRejectsOffsetPagination(t *testing.T) {
	// FR-59 says offset pagination MUST NOT be used. A rule that is only
	// documented is a rule the next client breaks silently.
	for _, banned := range []string{"offset", "page", "skip"} {
		t.Run(banned, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/rooms?"+banned+"=20", nil)
			_, err := ParsePageParams(r, 20, 100)
			if err == nil {
				t.Fatalf("?%s= was accepted; FR-59 requires refusal", banned)
			}
			p := AsProblem(err)
			if p.Code != "OFFSET_PAGINATION_UNSUPPORTED" {
				t.Errorf("code = %q, want OFFSET_PAGINATION_UNSUPPORTED", p.Code)
			}
			if p.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", p.Status)
			}
		})
	}
}

func TestParsePageParamsLimits(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantCode  string
	}{
		{"default when absent", "", 20, ""},
		{"explicit within range", "?limit=50", 50, ""},
		{"at the maximum", "?limit=100", 100, ""},
		{"above the maximum is refused", "?limit=101", 0, "LIMIT_INVALID"},
		{"zero is refused", "?limit=0", 0, "LIMIT_INVALID"},
		{"negative is refused", "?limit=-1", 0, "LIMIT_INVALID"},
		{"non-numeric is refused", "?limit=lots", 0, "LIMIT_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/rooms"+tt.query, nil)
			got, err := ParsePageParams(r, 20, 100)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("expected %s, got limit=%d", tt.wantCode, got.Limit)
				}
				if code := AsProblem(err).Code; code != tt.wantCode {
					t.Fatalf("code = %q, want %q", code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", got.Limit, tt.wantLimit)
			}
		})
	}
}

// A limit above the maximum is refused rather than clamped, because clamping
// tells the client it received everything it asked for when it did not.
func TestOverLimitIsRefusedNotClamped(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/rooms?limit=1000", nil)
	got, err := ParsePageParams(r, 20, 100)
	if err == nil {
		t.Fatalf("limit=1000 was silently clamped to %d instead of refused", got.Limit)
	}
}

func TestParsePageParamsCursor(t *testing.T) {
	cur := Cursor{CreatedAt: time.Date(2026, 8, 25, 7, 0, 0, 0, time.UTC), ID: "abc"}

	r := httptest.NewRequest(http.MethodGet, "/v1/rooms?cursor="+cur.Encode(), nil)
	got, err := ParsePageParams(r, 20, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Cursor == nil || got.Cursor.ID != "abc" {
		t.Fatalf("cursor = %+v, want ID abc", got.Cursor)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/rooms", nil)
	got, err = ParsePageParams(r, 20, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Cursor != nil {
		t.Errorf("first page should have a nil cursor, got %+v", got.Cursor)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/rooms?cursor=garbage!!", nil)
	if _, err := ParsePageParams(r, 20, 100); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}

type row struct {
	id string
	at time.Time
}

func rowKey(r row) Cursor { return Cursor{CreatedAt: r.at, ID: r.id} }

func TestNewPageDetectsMoreViaTheExtraRow(t *testing.T) {
	base := time.Date(2026, 8, 25, 7, 0, 0, 0, time.UTC)
	mk := func(n int) []row {
		out := make([]row, n)
		for i := range out {
			out[i] = row{id: string(rune('a' + i)), at: base.Add(time.Duration(i) * time.Second)}
		}
		return out
	}

	t.Run("exactly the limit means no next page", func(t *testing.T) {
		p := NewPage(mk(3), 3, rowKey)
		if len(p.Items) != 3 {
			t.Errorf("items = %d, want 3", len(p.Items))
		}
		if p.NextCursor != "" {
			t.Errorf("NextCursor = %q, want empty on the last page", p.NextCursor)
		}
	})

	t.Run("limit+1 yields a cursor and drops the probe row", func(t *testing.T) {
		p := NewPage(mk(4), 3, rowKey)
		if len(p.Items) != 3 {
			t.Fatalf("items = %d, want 3 — the extra probe row must not be returned", len(p.Items))
		}
		if p.NextCursor == "" {
			t.Fatal("NextCursor is empty despite a fourth row signalling more")
		}
		got, err := DecodeCursor(p.NextCursor)
		if err != nil {
			t.Fatalf("decode next cursor: %v", err)
		}
		if got.ID != "c" {
			t.Errorf("cursor anchors on %q, want the last RETURNED row %q", got.ID, "c")
		}
	})

	t.Run("under the limit", func(t *testing.T) {
		p := NewPage(mk(1), 3, rowKey)
		if len(p.Items) != 1 || p.NextCursor != "" {
			t.Errorf("got %d items, cursor %q", len(p.Items), p.NextCursor)
		}
	})
}

func TestNewPageNeverEmitsNullItems(t *testing.T) {
	// Dart's List<T> decode throws on null but accepts []. A null here would
	// crash the app on an empty list instead of rendering the §3.2 empty state.
	p := NewPage(nil, 20, rowKey)
	if p.Items == nil {
		t.Fatal("Items is nil; it must marshal as [] not null")
	}
	if len(p.Items) != 0 {
		t.Errorf("Items = %v, want empty", p.Items)
	}
}

func TestNewPageWithNonPositiveLimit(t *testing.T) {
	p := NewPage([]row{{id: "a"}}, 0, rowKey)
	if p.NextCursor != "" || len(p.Items) != 1 {
		t.Errorf("a non-positive limit should not paginate, got %+v", p)
	}
}
