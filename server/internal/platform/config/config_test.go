package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear everything this package reads, so a test never inherits state from
	// the developer's real shell — the GEMINI_API_KEY sitting in this
	// operator's environment is a reminder of how easily that happens.
	for _, k := range []string{
		"VYBE_ENV", "VYBE_LOG_LEVEL", "VYBE_HTTP_ADDR", "VYBE_DB_DSN",
		"VYBE_REDIS_ADDR", "VYBE_JWT_PRIVATE_KEY_B64", "VYBE_ACCESS_TOKEN_TTL",
		"VYBE_REFRESH_TOKEN_TTL", "VYBE_WS_TICKET_TTL", "TMDB_API_KEY",
		"TMDB_BASE_URL", "VYBE_RESYNC_DELTA_THRESHOLD",
	} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

const validDSN = "postgres://vybe:hunter2@localhost:5432/vybe?sslmode=disable"

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, map[string]string{"VYBE_DB_DSN": validDSN})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Env != EnvLocal {
		t.Fatalf("Env = %q, want local", cfg.Env)
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Fatalf("AccessTokenTTL = %v, want 15m (ADR-011)", cfg.AccessTokenTTL)
	}
	if cfg.WSTicketTTL != 60*time.Second {
		t.Fatalf("WSTicketTTL = %v, want 60s (ADR-011)", cfg.WSTicketTTL)
	}
	// ADR-003: the threshold is configuration precisely so it can be tuned.
	if cfg.ResyncDeltaThreshold != 200 {
		t.Fatalf("ResyncDeltaThreshold = %d, want 200", cfg.ResyncDeltaThreshold)
	}
}

func TestLoad_RequiresDSN(t *testing.T) {
	setEnv(t, nil)
	_, err := Load()
	if err == nil {
		t.Fatal("Load() must fail without VYBE_DB_DSN")
	}
	if !strings.Contains(err.Error(), "VYBE_DB_DSN") {
		t.Fatalf("error should name the missing variable, got: %v", err)
	}
}

// An unrecognised environment must be refused, not defaulted. Defaulting to
// "local" would silently disable the production safety checks below.
func TestLoad_RejectsUnknownEnv(t *testing.T) {
	setEnv(t, map[string]string{"VYBE_DB_DSN": validDSN, "VYBE_ENV": "prod"})
	_, err := Load()
	if err == nil {
		t.Fatal(`VYBE_ENV="prod" is not a valid value and must be refused`)
	}
}

// ADR-011: an access token is not revocable before it expires, so a long TTL
// is a security regression rather than a tuning choice.
func TestLoad_RefusesLongAccessTokenTTL(t *testing.T) {
	setEnv(t, map[string]string{
		"VYBE_DB_DSN":           validDSN,
		"VYBE_ACCESS_TOKEN_TTL": "24h",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("a 24h access token TTL must be refused")
	}
	if !strings.Contains(err.Error(), "revocable") {
		t.Fatalf("error should explain WHY, got: %v", err)
	}
}

// Generating a key at boot in a deployed environment would invalidate every
// session on restart and give each instance a different key.
func TestLoad_RequiresSigningKeyOutsideLocal(t *testing.T) {
	for _, env := range []string{"staging", "production"} {
		t.Run(env, func(t *testing.T) {
			setEnv(t, map[string]string{"VYBE_DB_DSN": validDSN, "VYBE_ENV": env})
			_, err := Load()
			if err == nil {
				t.Fatalf("%s must require VYBE_JWT_PRIVATE_KEY_B64", env)
			}
			if !strings.Contains(err.Error(), "invalidates all sessions") {
				t.Fatalf("error should explain the consequence, got: %v", err)
			}
		})
	}

	t.Run("local may generate one", func(t *testing.T) {
		setEnv(t, map[string]string{"VYBE_DB_DSN": validDSN, "VYBE_ENV": "local"})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("local should not require a key: %v", err)
		}
		if !cfg.HasEphemeralSigningKey() {
			t.Fatal("HasEphemeralSigningKey() should be true when no key is set")
		}
	})
}

func TestLoad_ValidatesSigningKeyLength(t *testing.T) {
	t.Run("wrong length is refused", func(t *testing.T) {
		setEnv(t, map[string]string{
			"VYBE_DB_DSN":              validDSN,
			"VYBE_JWT_PRIVATE_KEY_B64": base64.StdEncoding.EncodeToString([]byte("too short")),
		})
		if _, err := Load(); err == nil {
			t.Fatal("a 9-byte key must be refused; Ed25519 needs 32 or 64")
		}
	})

	t.Run("not base64 is refused", func(t *testing.T) {
		setEnv(t, map[string]string{
			"VYBE_DB_DSN":              validDSN,
			"VYBE_JWT_PRIVATE_KEY_B64": "!!! not base64 !!!",
		})
		if _, err := Load(); err == nil {
			t.Fatal("invalid base64 must be refused")
		}
	})

	t.Run("32-byte seed is accepted", func(t *testing.T) {
		setEnv(t, map[string]string{
			"VYBE_DB_DSN":              validDSN,
			"VYBE_JWT_PRIVATE_KEY_B64": base64.StdEncoding.EncodeToString(make([]byte, 32)),
		})
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HasEphemeralSigningKey() {
			t.Fatal("a key was supplied; HasEphemeralSigningKey() should be false")
		}
	})
}

// ADR-012: an absent provider key degrades refresh, it does not stop the
// process. The catalogue still serves local content.
func TestLoad_MissingTMDBKeyIsNotFatal(t *testing.T) {
	setEnv(t, map[string]string{"VYBE_DB_DSN": validDSN})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a missing TMDB key must not fail startup: %v", err)
	}
	if cfg.ProviderRefreshEnabled() {
		t.Fatal("ProviderRefreshEnabled() should be false with no key")
	}
}

// §12.6 forbids logging secrets. String() exists so the config CAN be logged
// at startup; this test is what keeps that safe as fields are added.
func TestString_RedactsEverySecret(t *testing.T) {
	const (
		dbPassword = "hunter2-should-never-appear"
		tmdbKey    = "tmdb-secret-should-never-appear"
	)
	keyBytes := make([]byte, 32)
	for i := range keyBytes {
		keyBytes[i] = 0xAB
	}

	setEnv(t, map[string]string{
		"VYBE_DB_DSN":              "postgres://vybe:" + dbPassword + "@db:5432/vybe",
		"TMDB_API_KEY":             tmdbKey,
		"VYBE_JWT_PRIVATE_KEY_B64": base64.StdEncoding.EncodeToString(keyBytes),
	})

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	out := cfg.String()

	for _, secret := range []string{
		dbPassword,
		tmdbKey,
		base64.StdEncoding.EncodeToString(keyBytes),
		string(keyBytes),
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("String() leaked a secret (§12.6):\n%s", out)
		}
	}

	// It must still be useful for diagnosis, or people will log the struct
	// directly instead and defeat the whole point.
	for _, want := range []string{"env=", "db=postgres://vybe:***@db:5432", "jwt_key=set", "tmdb_key=set"} {
		if !strings.Contains(out, want) {
			t.Fatalf("String() should contain %q for diagnosis, got:\n%s", want, out)
		}
	}
}

func TestRedactDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5432/db":  "postgres://u:***@h:5432/db",
		"postgres://u@h:5432/db":    "postgres://u:***@h:5432/db",
		"":                          "unset",
		"not-a-url":                 "set",
		"host=localhost password=p": "set",
	}
	for in, want := range cases {
		if got := redactDSN(in); got != want {
			t.Fatalf("redactDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnv_IsProduction(t *testing.T) {
	if !EnvProduction.IsProduction() {
		t.Fatal("production must report IsProduction")
	}
	for _, e := range []Env{EnvLocal, EnvDev, EnvStaging} {
		if e.IsProduction() {
			t.Fatalf("%q must not report IsProduction", e)
		}
	}
}

func TestGetenvDuration_RejectsGarbage(t *testing.T) {
	setEnv(t, map[string]string{
		"VYBE_DB_DSN":           validDSN,
		"VYBE_ACCESS_TOKEN_TTL": "fifteen minutes",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("an unparseable duration must be refused, not silently defaulted")
	}
	if !strings.Contains(err.Error(), "15m") {
		t.Fatalf("error should show the expected format, got: %v", err)
	}
}
