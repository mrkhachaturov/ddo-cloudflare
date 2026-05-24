package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearCFEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CLOUDFLARE_API_TOKEN",
		"CLOUDFLARE_API_TOKEN_FILE",
		"CLOUDFLARE_ZONES",
		"CLOUDFLARE_DEFAULT_TTL",
		"CLOUDFLARE_PROXIED_DEFAULT",
		"WEBHOOK_LISTEN",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoad_HappyPath(t *testing.T) {
	clearCFEnv(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "secret-token")
	t.Setenv("CLOUDFLARE_ZONES", "example.com, internal.example.com.")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIToken != "secret-token" {
		t.Fatalf("bad token: %+v", c)
	}
	if len(c.Zones) != 2 || c.Zones[0] != "example.com" || c.Zones[1] != "internal.example.com" {
		t.Fatalf("zones not normalised: %+v", c.Zones)
	}
	if c.DefaultTTL != 1 {
		t.Fatalf("default TTL should be 1 (Cloudflare 'automatic'), got %d", c.DefaultTTL)
	}
	if c.Listen != ":9090" {
		t.Fatalf("listen default wrong: %q", c.Listen)
	}
	if c.ProxiedDefault {
		t.Fatalf("proxied default should be false: %+v", c)
	}
}

func TestLoad_RejectsMissingToken(t *testing.T) {
	clearCFEnv(t)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CLOUDFLARE_API_TOKEN") {
		t.Fatalf("expected CLOUDFLARE_API_TOKEN error, got %v", err)
	}
}

func TestLoad_TokenFileResolvedFromDisk(t *testing.T) {
	clearCFEnv(t)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN_FILE", tf)
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIToken != "file-token" {
		t.Fatalf("token not picked up from file: %+v", c)
	}
}

func TestLoad_RejectsBothTokenAndTokenFile(t *testing.T) {
	clearCFEnv(t)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-token")
	t.Setenv("CLOUDFLARE_API_TOKEN_FILE", tf)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected mutex error, got %v", err)
	}
}

func TestLoad_RejectsEmptyTokenFile(t *testing.T) {
	clearCFEnv(t)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN_FILE", tf)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-file error, got %v", err)
	}
}

func TestLoad_ProxiedDefaultRespectsBool(t *testing.T) {
	clearCFEnv(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_PROXIED_DEFAULT", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ProxiedDefault {
		t.Fatalf("proxied default should be true: %+v", c)
	}
}

func TestLoad_RejectsInvalidProxiedDefault(t *testing.T) {
	clearCFEnv(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_PROXIED_DEFAULT", "yes")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "boolean") {
		t.Fatalf("expected boolean error, got %v", err)
	}
}

func TestLoad_RejectsInvalidDefaultTTL(t *testing.T) {
	clearCFEnv(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_DEFAULT_TTL", "-1")
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error on negative TTL")
	}
}

func TestLoad_EmptyZonesMeansNoFilter(t *testing.T) {
	clearCFEnv(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Zones != nil {
		t.Fatalf("zones should be nil when CLOUDFLARE_ZONES unset, got %+v", c.Zones)
	}
}
