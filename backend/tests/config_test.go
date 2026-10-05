package tests

import (
	"os"
	"testing"

	"github.com/middleman/backend/internal/config"
)

// TestConfigLoadRequiresDatabaseURL verifies Phase 0's "fail fast on missing
// required config" behavior rather than starting in a half-configured state.
func TestConfigLoadRequiresDatabaseURL(t *testing.T) {
	os.Unsetenv("DATABASE_URL")

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error when DATABASE_URL is unset, got nil")
	}
}

func TestConfigLoadSucceedsWithDatabaseURL(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/middleman")
	defer os.Unsetenv("DATABASE_URL")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.AppPort != "8080" {
		t.Fatalf("expected default port 8080, got %s", cfg.AppPort)
	}
}

// Phase 3.1 §3A-1: outside development, the backend must refuse to start
// without the Meta App Secret, because without it webhook signatures
// cannot be verified and anyone could impersonate a buyer or vendor.
func TestConfigLoadRequiresAppSecretOutsideDevelopment(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/middleman")
	os.Setenv("APP_ENV", "production")
	os.Unsetenv("WHATSAPP_APP_SECRET")
	defer os.Unsetenv("DATABASE_URL")
	defer os.Unsetenv("APP_ENV")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when WHATSAPP_APP_SECRET is unset in production")
	}

	os.Setenv("WHATSAPP_APP_SECRET", "s3cret")
	defer os.Unsetenv("WHATSAPP_APP_SECRET")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected success with secret set, got %v", err)
	}
	if cfg.AllowUnsignedWebhooks {
		t.Fatal("unsigned webhooks must never be allowed when a secret is configured")
	}
}

func TestConfigDevelopmentWithoutSecretAllowsUnsignedExplicitly(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/middleman")
	os.Unsetenv("APP_ENV")
	os.Unsetenv("WHATSAPP_APP_SECRET")
	defer os.Unsetenv("DATABASE_URL")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AllowUnsignedWebhooks {
		t.Fatal("expected development without secret to be flagged as AllowUnsignedWebhooks")
	}
}
