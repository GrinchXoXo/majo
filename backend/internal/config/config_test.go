package config

import (
	"os"
	"testing"
)

func TestOperatorTokenRequiredOutsideDevelopment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WHATSAPP_APP_SECRET", "s")

	t.Setenv("APP_ENV", "production")
	t.Setenv("OPERATOR_API_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("production without OPERATOR_API_TOKEN must fail to start")
	}
	t.Setenv("OPERATOR_API_TOKEN", "short")
	if _, err := Load(); err == nil {
		t.Fatal("a short operator token must be refused")
	}
	t.Setenv("OPERATOR_API_TOKEN", "a-long-enough-operator-token")
	cfg, err := Load()
	if err != nil || cfg.AllowUnauthenticatedOperator {
		t.Fatalf("production with a token: %v %+v", err, cfg)
	}

	t.Setenv("APP_ENV", "development")
	t.Setenv("OPERATOR_API_TOKEN", "")
	cfg, err = Load()
	if err != nil || !cfg.AllowUnauthenticatedOperator {
		t.Fatalf("explicit development without a token stays open (with a startup warning): %v", err)
	}
}

// Fail closed: APP_ENV defaults to development, but that default must not
// open curation; only an explicit APP_ENV=development does.
func TestOperatorTokenRequiredWhenAppEnvUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OPERATOR_API_TOKEN", "")
	t.Setenv("APP_ENV", "")
	os.Unsetenv("APP_ENV")
	if _, err := Load(); err == nil {
		t.Fatal("no APP_ENV and no OPERATOR_API_TOKEN must refuse to start")
	}
	t.Setenv("OPERATOR_API_TOKEN", "a-long-enough-operator-token")
	cfg, err := Load()
	if err != nil || cfg.AllowUnauthenticatedOperator || cfg.AppEnv != "development" {
		t.Fatalf("with a token the defaults still apply: %v", err)
	}
}
