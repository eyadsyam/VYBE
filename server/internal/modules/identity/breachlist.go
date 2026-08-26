package identity

import (
	_ "embed"
	"strings"
)

// The embedded breach list.
//
// Embedded rather than read from disk so the binary has no runtime file
// dependency: PasswordPolicy fails CLOSED without a breach set, which means a
// missing file would turn registration into a 503 on a machine that is
// otherwise perfectly healthy. A container that starts is a container that can
// register users.
//
// See breachlist.txt for why the list is short and what it deliberately is
// not.

//go:embed breachlist.txt
var embeddedBreachList string

// EmbeddedBreachSet returns the compiled-in breach set.
//
// The error is returned rather than panicked on because the caller — main —
// is better placed to decide, and because an empty list would silently pass
// every password while looking configured. LoadStaticBreachSet already refuses
// that case.
func EmbeddedBreachSet() (*StaticBreachSet, error) {
	return LoadStaticBreachSet(strings.NewReader(embeddedBreachList))
}
