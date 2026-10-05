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
