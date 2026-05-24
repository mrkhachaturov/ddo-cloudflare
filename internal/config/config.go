package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the parsed environment for the sidecar. Fields are populated by
// Load() from CLOUDFLARE_* / WEBHOOK_* env vars. The sidecar is intentionally
// ownership-agnostic — it has no env var for any owner-identity concept.
// The caller (operator) stamps Labels["owner"] on every Endpoint and the
// sidecar persists/round-trips that value via the Cloudflare `comment`
// field on each DNS record.
type Config struct {
	// Listen is the HTTP bind address. Default ":9090".
	Listen string

	// APIToken is the Cloudflare API token used to authenticate against the
	// Cloudflare REST API. Required. Resolved from either CLOUDFLARE_API_TOKEN
	// or CLOUDFLARE_API_TOKEN_FILE (mutually exclusive).
	//
	// The token must have Zone:Read + Zone DNS:Edit on the zones it manages.
	APIToken string

	// Zones is the optional zone allow-list. Empty = "use every zone the
	// token can see" (resolved at boot from the Cloudflare API). When set,
	// only the named zones are managed; inbound FQDNs must end in one of
	// them.
	Zones []string

	// DefaultTTL is applied when an inbound Endpoint omits recordTTL
	// (RecordTTL <= 0). The Cloudflare convention: a TTL of 1 means
	// "automatic" — the platform picks the value. Anything else must be
	// >= 60 (Free/Pro) or >= 30 (Enterprise) per Cloudflare validation.
	DefaultTTL int

	// ProxiedDefault is the Proxied value applied to A/CNAME records when
	// the inbound Endpoint has no
	// "external-dns.alpha.kubernetes.io/cloudflare-proxied"
	// providerSpecific entry. Matches external-dns' --cloudflare-proxied
	// flag semantics.
	ProxiedDefault bool
}

// Load populates a Config from the process environment. Returns an error if
// any required field is missing or any value fails validation.
func Load() (Config, error) {
	token, err := resolveSecret("CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKEN_FILE")
	if err != nil {
		return Config{}, err
	}
	if token == "" {
		return Config{}, errors.New("one of CLOUDFLARE_API_TOKEN or CLOUDFLARE_API_TOKEN_FILE is required")
	}

	defaultTTL, err := parsePositiveInt("CLOUDFLARE_DEFAULT_TTL", 1)
	if err != nil {
		return Config{}, err
	}

	proxiedDefault, err := parseBoolStrict("CLOUDFLARE_PROXIED_DEFAULT", false)
	if err != nil {
		return Config{}, err
	}

	c := Config{
		Listen:         envOr("WEBHOOK_LISTEN", ":9090"),
		APIToken:       token,
		Zones:          parseZones(os.Getenv("CLOUDFLARE_ZONES")),
		DefaultTTL:     defaultTTL,
		ProxiedDefault: proxiedDefault,
	}
	return c, nil
}

// resolveSecret picks exactly one source per credential. envKey XOR fileKey.
func resolveSecret(envKey, fileKey string) (string, error) {
	env := os.Getenv(envKey)
	file := os.Getenv(fileKey)
	switch {
	case env != "" && file != "":
		return "", fmt.Errorf("set exactly one of %s or %s", envKey, fileKey)
	case env != "":
		return env, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s: read: %w", fileKey, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("%s: file %q is empty", fileKey, file)
		}
		return v, nil
	}
	return "", nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// parseBoolStrict accepts only the set strconv.ParseBool recognises ("1",
// "t", "T", "TRUE", "true", "True", "0", "f", "F", "FALSE", "false",
// "False"). Empty falls back to the provided default; anything else is
// rejected.
func parseBoolStrict(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: must be a boolean (true/false), got %q", key, v)
	}
	return b, nil
}

func parsePositiveInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// parseZones splits a comma-separated list, trims and lower-cases each
// entry, strips a trailing dot, and drops blanks. Empty input returns nil
// (no filter — use all token-visible zones).
func parseZones(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		z := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p)), ".")
		if z == "" {
			continue
		}
		out = append(out, z)
	}
	return out
}
