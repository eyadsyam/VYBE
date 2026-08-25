// Command probe makes real calls to a third-party API and records what it
// actually returned.
//
// This exists because of Master Prompt v2 §0.3 rule 1:
//
//	"Never invent third-party API behaviour. If you do not know how a provider
//	 responds, write a small integration probe, run it, record the real
//	 response shape in docs/INTEGRATIONS.md, then build against that."
//
// So: no adapter is written against a remembered or assumed TMDB schema. This
// tool runs, writes what it saw to tools/probe/out/, and only then does
// docs/INTEGRATIONS.md get response shapes in it.
//
//	go run ./cmd/probe tmdb
//
// Requires TMDB_API_KEY. Without it the tool exits non-zero and says so — it
// does not fall back to a fixture, because a fixture would be exactly the
// invented behaviour the rule prohibits.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const outDir = "tools/probe/out"

func main() {
	log.SetFlags(0)
	log.SetPrefix("probe: ")

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <tmdb>")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "tmdb":
		if err := probeTMDB(); err != nil {
			log.Fatalf("%v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown provider %q\n", os.Args[1])
		os.Exit(2)
	}
}

// call is one recorded request/response pair.
type call struct {
	Name       string            `json:"name"`
	Why        string            `json:"why"`
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Status     int               `json:"status"`
	LatencyMS  int64             `json:"latency_ms"`
	Headers    map[string]string `json:"interesting_headers"`
	BodyShape  any               `json:"body_shape"`
	BodySample json.RawMessage   `json:"body_sample,omitempty"`
	Error      string            `json:"error,omitempty"`
	ObservedAt string            `json:"observed_at"`
}

func probeTMDB() error {
	key := os.Getenv("TMDB_API_KEY")
	if key == "" {
		return fmt.Errorf(`TMDB_API_KEY is not set.

This probe deliberately has no offline mode. Master Prompt v2 §0.3 rule 1
forbids inventing provider behaviour, and a canned fixture would be exactly
that. Until this runs against the live API, docs/INTEGRATIONS.md records no
TMDB response shapes and the M0 exit criterion stays unmet (BLOCKER-01).

Get a free key: https://www.themoviedb.org/settings/api
Then: TMDB_API_KEY=... go run ./cmd/probe tmdb`)
	}

	baseURL := os.Getenv("TMDB_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.themoviedb.org/3"
	}

	client := &http.Client{Timeout: 20 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Each probe answers a question the catalog adapter (ADR-012) cannot be
	// written without. The Arabic ones are not optional: §1.4 makes Arabic a
	// launch requirement, and TMDB's MENA coverage is precisely the unknown
	// that risk R3 is about.
	probes := []struct {
		name  string
		why   string
		path  string
		query url.Values
	}{
		{
			name:  "search_movie_latin",
			why:   "Baseline: field names, pagination envelope, date and poster path formats.",
			path:  "/search/movie",
			query: url.Values{"query": {"Inception"}},
		},
		{
			name:  "search_movie_arabic_query",
			why:   "Does an Arabic-script query return anything at all? Determines whether provider search is usable for the MENA wedge or whether local search must carry it entirely.",
			path:  "/search/movie",
			query: url.Values{"query": {"الفيل الأزرق"}, "language": {"ar"}},
		},
		{
			name:  "search_tv_arabic_musalsal",
			why:   "Ramadan musalsalat are the launch wedge (§1.4). Establishes whether they exist in TMDB and how titles are transliterated.",
			path:  "/search/tv",
			query: url.Values{"query": {"مسلسل"}, "language": {"ar"}},
		},
		{
			name:  "movie_details_ar_locale",
			why:   "Which fields localise under language=ar and which stay English. Drives the content.title_ar / synopsis_ar mapping and the curated_fields mask.",
			path:  "/movie/27205",
			query: url.Values{"language": {"ar"}},
		},
		{
			name:  "movie_details_en_locale",
			why:   "Same record in English, to diff against the ar response and see exactly which fields are translated.",
			path:  "/movie/27205",
			query: url.Values{"language": {"en-US"}},
		},
		{
			name:  "watch_providers_eg",
			why:   "Where-to-watch offers for region EG (FR-9). Confirms whether deeplinks are supplied and what shape content_offers must hold.",
			path:  "/movie/27205/watch/providers",
			query: url.Values{},
		},
		{
			name:  "tv_season_episodes",
			why:   "Episode shape for the content_type discriminator and the parent_id/season/episode columns.",
			path:  "/tv/1399/season/1",
			query: url.Values{"language": {"en-US"}},
		},
		{
			name:  "credits_for_search_ranking",
			why:   "Cast/crew payload feeding the content_people table and §6.4's third ranking tier.",
			path:  "/movie/27205/credits",
			query: url.Values{},
		},
		{
			name:  "rate_limit_headers",
			why:   "What rate-limit headers does TMDB actually return today? Our own limiter must sit under their real ceiling, and their documented limits have changed before (risk R3).",
			path:  "/configuration",
			query: url.Values{},
		},
		{
			name:  "not_found_error_shape",
			why:   "Error envelope for a missing resource, so the adapter maps provider errors to our Failure hierarchy rather than guessing.",
			path:  "/movie/999999999",
			query: url.Values{},
		},
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	results := make([]call, 0, len(probes))
	for _, p := range probes {
		q := url.Values{}
		for k, v := range p.query {
			q[k] = v
		}
		q.Set("api_key", key)

		endpoint := baseURL + p.path + "?" + q.Encode()
		rec := call{
			Name:       p.name,
			Why:        p.why,
			Method:     http.MethodGet,
			URL:        redactKey(endpoint, key),
			ObservedAt: time.Now().UTC().Format(time.RFC3339),
			Headers:    map[string]string{},
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			rec.Error = err.Error()
			results = append(results, rec)
			continue
		}
		req.Header.Set("Accept", "application/json")

		start := time.Now()
		resp, err := client.Do(req)
		rec.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			rec.Error = err.Error()
			results = append(results, rec)
			log.Printf("%-30s ERROR %v", p.name, err)
			continue
		}

		rec.Status = resp.StatusCode
		for _, h := range []string{
			"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
			"Retry-After", "Cache-Control", "ETag", "Content-Type",
		} {
			if v := resp.Header.Get(h); v != "" {
				rec.Headers[h] = v
			}
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			rec.Error = err.Error()
			results = append(results, rec)
			continue
		}

		var parsed any
		if err := json.Unmarshal(body, &parsed); err != nil {
			rec.Error = "response was not JSON: " + err.Error()
			rec.BodySample = json.RawMessage(strconv.Quote(truncate(string(body), 500)))
		} else {
			// The shape is what we build against; the sample is evidence that
			// the shape was not invented.
			rec.BodyShape = describe(parsed, 0)
			rec.BodySample = json.RawMessage(mustCompactSample(body))
		}

		results = append(results, rec)
		log.Printf("%-30s %d  %dms", p.name, rec.Status, rec.LatencyMS)

		// Stay well under any plausible ceiling; this is a probe, not a scrape.
		time.Sleep(300 * time.Millisecond)
	}

	outPath := filepath.Join(outDir, "tmdb.json")
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"provider":    "tmdb",
		"base_url":    baseURL,
		"observed_at": time.Now().UTC().Format(time.RFC3339),
		"note": "Recorded by cmd/probe against the LIVE API. " +
			"docs/INTEGRATIONS.md must be derived from this file, never from memory.",
		"calls": results,
	}); err != nil {
		return err
	}

	log.Printf("wrote %s", outPath)
	log.Printf("next: transcribe the observed shapes into docs/INTEGRATIONS.md and clear BLOCKER-01")
	return nil
}

// describe reduces a decoded JSON value to a type skeleton.
//
// The shape is what the adapter is written against. Recording it separately
// from the sample means a reviewer can see the contract without reading a
// thousand lines of payload, and a future re-run can be diffed against it to
// detect a provider change (risk R3).
func describe(v any, depth int) any {
	if depth > 6 {
		return "…"
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = describe(t[k], depth+1)
		}
		return out
	case []any:
		if len(t) == 0 {
			return []any{"empty array — shape unknown from this response"}
		}
		return []any{describe(t[0], depth+1)}
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "number(integer)"
		}
		return "number(float)"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func mustCompactSample(body []byte) []byte {
	const max = 4000
	if len(body) <= max {
		return body
	}
	// Keep it valid JSON: a truncated object would not parse on re-read.
	return []byte(strconv.Quote(truncate(string(body), max) + " …[truncated]"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// redactKey removes the API key from anything written to disk. A probe output
// file is committed as evidence, and §12.6 forbids logging credentials.
func redactKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "REDACTED")
}
