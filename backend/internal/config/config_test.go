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

func TestSupplyLifecycleConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("APP_ENV", "development")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SupplyMaxLifecycleDays != 60 || cfg.SupplyGeoLocalDays != 0 || cfg.SupplyGeoNearbyDays != 20 ||
		cfg.SupplyGeoStateDays != 30 || cfg.SupplyGeoRegionDays != 45 || cfg.SupplyFreshnessWindow.Hours() != 72 {
		t.Fatalf("defaults: %+v", cfg)
	}
	t.Setenv("SUPPLY_GEO_LOCAL_DAYS", "0") // zero is allowed for day values
	t.Setenv("SUPPLY_MAX_LIFECYCLE_DAYS", "90")
	t.Setenv("SUPPLY_FRESHNESS_WINDOW", "48h")
	if cfg, err = Load(); err != nil || cfg.SupplyMaxLifecycleDays != 90 || cfg.SupplyFreshnessWindow.Hours() != 48 {
		t.Fatalf("overrides: %v %+v", err, cfg)
	}
	for _, bad := range []string{"-1", "x", "3651", "1.5"} {
		t.Setenv("SUPPLY_GEO_NEARBY_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("SUPPLY_GEO_NEARBY_DAYS=%q must be rejected", bad)
		}
	}
}
