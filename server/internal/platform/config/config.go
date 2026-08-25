// Package config loads and validates process configuration.
//
// Two rules shape this file:
//
//   - **Fail at boot, not at first use.** A missing DSN should stop the
//     process immediately with a clear message, not surface as a confusing
//     nil-pointer during someone's first room.
//   - **Never log a secret** (§12.6). String() exists precisely so that
//     dumping the config at startup — which is genuinely useful — cannot leak
//     a signing key.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env is the deployment environment. Several safety behaviours key off it, so
// it is a type rather than a free string.
type Env string

const (
	EnvLocal      Env = "local"
	EnvDev        Env = "dev"
	EnvStaging    Env = "staging"
	EnvProduction Env = "production"
)

func (e Env) IsProduction() bool { return e == EnvProduction }

// Valid reports whether e is a recognised environment. An unrecognised value
// is refused rather than defaulted, because defaulting an unknown environment
// to "local" would quietly disable production safety checks.
func (e Env) Valid() bool {
	switch e {
	case EnvLocal, EnvDev, EnvStaging, EnvProduction:
		return true
	}
	return false
}

// Config is the fully validated process configuration.
type Config struct {
	Env      Env
	LogLevel string
	HTTPAddr string

	DatabaseDSN string
	RedisAddr   string

	// JWTPrivateKey is the raw Ed25519 seed (ADR-011). Never logged, never
	// serialised, and deliberately not a string field so it does not land in
	// a %v of the struct.
	JWTPrivateKey []byte

	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	WSTicketTTL     time.Duration

	// TMDBAPIKey may be empty. ADR-012: the catalogue serves local content
	// regardless; only provider *refresh* needs the key. An empty key
	// degrades a feature, it does not stop the process.
	TMDBAPIKey  string
	TMDBBaseURL string

	// ResyncDeltaThreshold — ADR-003 requires this be configuration, not a
	// constant, because 200 is a hypothesis until the delta/snapshot ratio has
	// been measured in the field.
	ResyncDeltaThreshold int
}

// Load reads the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		Env:                  Env(getenv("VYBE_ENV", string(EnvLocal))),
		LogLevel:             getenv("VYBE_LOG_LEVEL", "info"),
		HTTPAddr:             getenv("VYBE_HTTP_ADDR", ":8080"),
		DatabaseDSN:          os.Getenv("VYBE_DB_DSN"),
		RedisAddr:            getenv("VYBE_REDIS_ADDR", "localhost:6379"),
		TMDBAPIKey:           os.Getenv("TMDB_API_KEY"),
		TMDBBaseURL:          getenv("TMDB_BASE_URL", "https://api.themoviedb.org/3"),
		ResyncDeltaThreshold: getenvInt("VYBE_RESYNC_DELTA_THRESHOLD", 200),
	}

	var errs []error

	if !cfg.Env.Valid() {
		errs = append(errs, fmt.Errorf(
			"VYBE_ENV=%q is not one of local|dev|staging|production", cfg.Env))
	}
	if cfg.DatabaseDSN == "" {
		errs = append(errs, errors.New("VYBE_DB_DSN is required (see .env.example)"))
	}

	var err error
	if cfg.AccessTokenTTL, err = getenvDuration("VYBE_ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		errs = append(errs, err)
	}
	if cfg.RefreshTokenTTL, err = getenvDuration("VYBE_REFRESH_TOKEN_TTL", 1440*time.Hour); err != nil {
		errs = append(errs, err)
	}
	if cfg.WSTicketTTL, err = getenvDuration("VYBE_WS_TICKET_TTL", 60*time.Second); err != nil {
		errs = append(errs, err)
	}

	// ADR-011 sets the access-token TTL at 15 minutes precisely because the
	// token is not revocable before it expires. A long TTL silently widens
	// that window, so it is refused rather than warned about.
	if cfg.AccessTokenTTL > time.Hour {
		errs = append(errs, fmt.Errorf(
			"VYBE_ACCESS_TOKEN_TTL=%s exceeds one hour; access tokens are not "+
				"revocable before expiry (ADR-011), so a long TTL is a security "+
				"regression, not a convenience", cfg.AccessTokenTTL))
	}

	if cfg.ResyncDeltaThreshold < 1 {
		errs = append(errs, fmt.Errorf(
			"VYBE_RESYNC_DELTA_THRESHOLD=%d must be at least 1", cfg.ResyncDeltaThreshold))
	}

	// --- signing key ---------------------------------------------------
	if raw := os.Getenv("VYBE_JWT_PRIVATE_KEY_B64"); raw != "" {
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if decodeErr != nil {
			errs = append(errs, fmt.Errorf("VYBE_JWT_PRIVATE_KEY_B64 is not valid base64: %w", decodeErr))
		} else if len(key) != 32 && len(key) != 64 {
			errs = append(errs, fmt.Errorf(
				"VYBE_JWT_PRIVATE_KEY_B64 decodes to %d bytes; an Ed25519 key is "+
					"32 (seed) or 64 (full private key)", len(key)))
		} else {
			cfg.JWTPrivateKey = key
		}
	} else if cfg.Env.IsProduction() || cfg.Env == EnvStaging {
		// Generating an ephemeral key in a deployed environment would mean
		// every restart invalidates every session, and every instance in a
		// horizontally-scaled deployment would sign with a different key.
		errs = append(errs, errors.New(
			"VYBE_JWT_PRIVATE_KEY_B64 is required outside local/dev: an ephemeral "+
				"key invalidates all sessions on restart and breaks multi-instance deployments"))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// HasEphemeralSigningKey reports whether a key must be generated at boot.
// True only in local/dev, and the caller is expected to log loudly.
func (c *Config) HasEphemeralSigningKey() bool { return len(c.JWTPrivateKey) == 0 }

// ProviderRefreshEnabled reports whether the catalogue can refresh from the
// metadata provider. False is a degraded feature, not an error (ADR-012).
func (c *Config) ProviderRefreshEnabled() bool { return c.TMDBAPIKey != "" }

// String renders the config for a startup log with every secret redacted.
//
// This method is the reason it is safe to log the configuration at all, and
// §12.6 is why it exists. Adding a secret field without extending this method
// is a defect; the test in config_test.go asserts that no known secret value
// appears in the output.
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "env=%s ", c.Env)
	fmt.Fprintf(&b, "log_level=%s ", c.LogLevel)
	fmt.Fprintf(&b, "http_addr=%s ", c.HTTPAddr)
	fmt.Fprintf(&b, "db=%s ", redactDSN(c.DatabaseDSN))
	fmt.Fprintf(&b, "redis=%s ", c.RedisAddr)
	fmt.Fprintf(&b, "access_ttl=%s ", c.AccessTokenTTL)
	fmt.Fprintf(&b, "refresh_ttl=%s ", c.RefreshTokenTTL)
	fmt.Fprintf(&b, "ws_ticket_ttl=%s ", c.WSTicketTTL)
	fmt.Fprintf(&b, "resync_delta_threshold=%d ", c.ResyncDeltaThreshold)
	fmt.Fprintf(&b, "jwt_key=%s ", presence(len(c.JWTPrivateKey) > 0))
	fmt.Fprintf(&b, "tmdb_key=%s", presence(c.TMDBAPIKey != ""))
	return b.String()
}

func presence(ok bool) string {
	if ok {
		return "set"
	}
	return "absent"
}

// redactDSN strips the password from a Postgres URL while keeping the parts
// that make a connection problem diagnosable.
func redactDSN(dsn string) string {
	if dsn == "" {
		return "unset"
	}
	// postgres://user:password@host:port/db?params
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return "set"
	}
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return "set"
	}
	creds := dsn[scheme+3 : at]
	user := creds
	if colon := strings.Index(creds, ":"); colon >= 0 {
		user = creds[:colon]
	}
	return dsn[:scheme+3] + user + ":***" + dsn[at:]
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenvDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration (want e.g. 15m, 24h): %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q must be positive", key, v)
	}
	return d, nil
}
