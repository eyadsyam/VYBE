package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The spec-drift guard.
//
// api/openapi.yaml is hand-written, which is deliberate — a generated spec
// documents whatever the code happens to do, including its accidents, whereas
// a hand-written one states what the code is supposed to do. The cost of that
// choice is that the two can drift apart silently, and a client generated from
// a stale spec fails in ways nobody can reproduce against the running server.
//
// This test is what pays that cost down: it walks the REAL router and asserts
// that every route it finds is documented, and that every documented path
// exists. It does not parse YAML — a full parser would be a dependency for one
// assertion — it matches path strings, which is exactly the thing that drifts.

// specPath resolves api/openapi.yaml relative to this package.
func specPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "api", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("cannot find the spec at %s: %v", p, err)
	}
	return p
}

// documentedPaths extracts the top-level keys of the `paths:` block.
//
// They are the only lines in the file that start with exactly two spaces and a
// slash, which is a property of the document's shape rather than a guess about
// YAML in general.
func documentedPaths(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(specPath(t))
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}

	out := map[string]bool{}
	inPaths := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "paths:" {
			inPaths = true
			continue
		}
		if inPaths && trimmed == "components:" {
			break
		}
		if !inPaths {
			continue
		}
		if m := regexp.MustCompile(`^  (/\S*):\s*$`).FindStringSubmatch(trimmed); m != nil {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("found no documented paths; the extraction is broken, not the spec")
	}
	return out
}

// routerPaths walks the mounted router and returns its patterns, normalised to
// the spec's `{param}` style.
func routerPaths(t *testing.T) map[string]bool {
	t.Helper()
	e := newE2E(t)
	_ = e // the harness builds and mounts the real module graph

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mods := e.buildModulesForTest(t, logger)
	router := newRouter(nil, stubPool{}, stubRedis{}, logger, mods, nil)

	mux, ok := router.(*chi.Mux)
	if !ok {
		t.Fatalf("the router is %T, not a *chi.Mux; this test needs chi.Walk", router)
	}

	out := map[string]bool{}
	err := chi.Walk(mux, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/*")
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		// chi renders wildcards as /* and params as {name}, which already
		// matches the spec's style.
		if route == "" || strings.Contains(route, "*") {
			return nil
		}
		out[route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	return out
}

func TestEveryRouteIsDocumented(t *testing.T) {
	documented := documentedPaths(t)
	actual := routerPaths(t)

	for route := range actual {
		if !documented[route] {
			t.Errorf("route %q exists but is not in api/openapi.yaml; "+
				"a client generated from the spec will not know it is there", route)
		}
	}
}

func TestEveryDocumentedPathExists(t *testing.T) {
	documented := documentedPaths(t)
	actual := routerPaths(t)

	for route := range documented {
		if !actual[route] {
			t.Errorf("api/openapi.yaml documents %q but no such route is mounted; "+
				"a client generated from the spec will call it and get a 404", route)
		}
	}
}
