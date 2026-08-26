package identity_test

import (
	"os"
	"strings"
	"testing"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
)

func TestEmbeddedBreachSetLoads(t *testing.T) {
	// PasswordPolicy fails closed without a breach set, so a broken embed
	// turns registration into a 503 on an otherwise healthy machine.
	set, err := identity.EmbeddedBreachSet()
	if err != nil {
		t.Fatalf("EmbeddedBreachSet: %v", err)
	}
	if set.Size() < 50 {
		t.Errorf("the embedded breach set has %d entries; that is too few to be doing anything", set.Size())
	}
}

func TestEmbeddedBreachSetCatchesTheObviousLongPasswords(t *testing.T) {
	set, err := identity.EmbeddedBreachSet()
	if err != nil {
		t.Fatalf("EmbeddedBreachSet: %v", err)
	}
	policy := identity.PasswordPolicy{Breaches: set}

	// These all clear the length bar and are exactly what a user lands on
	// when told to "make it longer".
	for _, pw := range []string{
		"password1234", "123456789012", "qwertyuiop123", "iloveyou1234",
		"correct horse battery staple",
	} {
		if err := policy.Validate(pw); err == nil {
			t.Errorf("Validate(%q) accepted a known-breached password", pw)
		}
	}

	// And a genuinely unusual passphrase must pass, or the list is so broad it
	// is unusable.
	for _, pw := range []string{
		"ليلة أفلام مع الأصدقاء في القاهرة",
		"tangerine scaffold umbrella 7",
	} {
		if err := policy.Validate(pw); err != nil {
			t.Errorf("Validate(%q) = %v; an ordinary passphrase must be accepted", pw, err)
		}
	}
}

func TestEveryEmbeddedEntryClearsTheLengthBar(t *testing.T) {
	// An entry under MinPasswordLength is dead weight: the length check
	// rejects it first, so the breach check is never reached for it.
	for _, line := range strings.Split(embeddedListForTest(t), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < identity.MinPasswordLength {
			t.Errorf("%q is %d characters; entries under %d are unreachable dead weight",
				line, len(line), identity.MinPasswordLength)
		}
	}
}

// embeddedListForTest re-reads the source file, since the embedded string is
// unexported.
func embeddedListForTest(t *testing.T) string {
	t.Helper()
	data, err := readFile("breachlist.txt")
	if err != nil {
		t.Fatalf("reading breachlist.txt: %v", err)
	}
	return data
}

func readFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
