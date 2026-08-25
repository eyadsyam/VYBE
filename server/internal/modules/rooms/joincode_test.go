package rooms

import (
	"errors"
	"strings"
	"testing"
)

func TestNewJoinCodeShape(t *testing.T) {
	for range 500 {
		code, err := NewJoinCode()
		if err != nil {
			t.Fatalf("NewJoinCode: %v", err)
		}
		if len(code) != JoinCodeLength {
			t.Fatalf("code %q has length %d, want %d (FR-12)", code, len(code), JoinCodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(crockfordAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside the Crockford alphabet", code, r)
			}
		}
	}
}

func TestGeneratedCodesNeverContainTheExcludedLetters(t *testing.T) {
	// FR-12 excludes I, L, O and U. I and L are misread as 1 and O as 0 — and
	// these codes are read aloud over a call and typed by somebody else, so
	// transcription is the dominant failure mode. U is excluded so the encoding
	// cannot spell an obscenity.
	for range 2000 {
		code, err := NewJoinCode()
		if err != nil {
			t.Fatalf("NewJoinCode: %v", err)
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("generated code %q contains an excluded letter", code)
		}
	}
}

func TestGeneratedCodesAreNotObviouslyPredictable(t *testing.T) {
	// A predictable code would let somebody enumerate live rooms. FR-14 means
	// possession is not access, so this is defence in depth — but guessable
	// identifiers are how depth gets used up.
	seen := map[string]bool{}
	const runs = 2000
	for range runs {
		code, err := NewJoinCode()
		if err != nil {
			t.Fatalf("NewJoinCode: %v", err)
		}
		seen[code] = true
	}
	// 32^6 is about 1.07e9, so 2000 draws should essentially never collide.
	if len(seen) < runs-1 {
		t.Errorf("only %d distinct codes from %d draws; the generator looks biased", len(seen), runs)
	}

	// Every position should vary across the sample. A generator stuck on one
	// character in some position would still pass a pure uniqueness check.
	for pos := range JoinCodeLength {
		chars := map[byte]bool{}
		for code := range seen {
			chars[code[pos]] = true
		}
		if len(chars) < 10 {
			t.Errorf("position %d only ever took %d distinct values", pos, len(chars))
		}
	}
}

func TestParseJoinCodeAcceptsWhatUsersActuallyType(t *testing.T) {
	// Crockford's decoding rules exist for exactly this moment: somebody is
	// typing what they heard on a call.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical", "K7X2QP", "K7X2QP"},
		{"lowercase", "k7x2qp", "K7X2QP"},
		{"mixed case", "K7x2Qp", "K7X2QP"},
		{"surrounding whitespace", "  K7X2QP  ", "K7X2QP"},
		{"hyphenated for grouping", "K7X-2QP", "K7X2QP"},
		{"spaced for grouping", "K7X 2QP", "K7X2QP"},
		{"capital I read as one", "I7X2QP", "17X2QP"},
		{"lowercase l read as one", "l7X2QP", "17X2QP"},
		{"capital O read as zero", "O7X2QP", "07X2QP"},
		{"lowercase o read as zero", "o7X2QP", "07X2QP"},
		// I->1, O->0, l->1: three different substitutions in one code.
		{"several substitutions at once", "IOl-2QP", "1012QP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJoinCode(tt.in)
			if err != nil {
				t.Fatalf("ParseJoinCode(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseJoinCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseJoinCodeRejectsU(t *testing.T) {
	// U is excluded from the alphabet, and unlike I/L/O there is no digit it is
	// plausibly a misreading of. Silently mapping it would resolve to a
	// DIFFERENT room, which is worse than refusing.
	for _, in := range []string{"U7X2QP", "u7X2QP", "K7X2QU"} {
		if got, err := ParseJoinCode(in); !errors.Is(err, ErrInvalidJoinCode) {
			t.Errorf("ParseJoinCode(%q) = %q, %v; want ErrInvalidJoinCode", in, got, err)
		}
	}
}

func TestParseJoinCodeRejectsMalformed(t *testing.T) {
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"too short", "K7X2Q"},
		{"too long", "K7X2QPX"},
		{"punctuation", "K7X2Q!"},
		{"non-ascii", "K7X2Qم"},
		{"only separators", "------"},
		{"emoji", "K7X2Q🎬"},
		{"whitespace only", "      "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ParseJoinCode(tt.in); err == nil {
				t.Errorf("ParseJoinCode(%q) = %q; want an error", tt.in, got)
			}
		})
	}
}

func TestParseIsIdempotentOnGeneratedCodes(t *testing.T) {
	// Anything the generator produces must survive a parse unchanged, or a
	// user reading back a correct code would be told it is invalid.
	for range 500 {
		code, err := NewJoinCode()
		if err != nil {
			t.Fatalf("NewJoinCode: %v", err)
		}
		got, err := ParseJoinCode(code)
		if err != nil {
			t.Fatalf("ParseJoinCode(%q) rejected a generated code: %v", code, err)
		}
		if got != code {
			t.Fatalf("ParseJoinCode(%q) = %q; generated codes must be canonical", code, got)
		}
	}
}

func TestShareURLIsAUniversalLink(t *testing.T) {
	// FR-13 / §1.9 L4: any app can claim a custom scheme, whereas a Universal
	// Link is bound to a domain an attacker does not control.
	got := ShareURL("K7X2QP")
	if got != "https://vybe.app/r/K7X2QP" {
		t.Errorf("ShareURL = %q, want the https form", got)
	}
	if !strings.HasPrefix(got, "https://") {
		t.Error("the share URL must not be a custom scheme")
	}
}
