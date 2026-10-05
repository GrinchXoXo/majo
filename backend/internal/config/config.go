// Package config centralizes environment configuration for the Middleman backend.
// Phase 0 rule: all env vars are loaded once, at startup, in one place.
package config

import (
	"fmt"
	"os"
)

// Config holds every environment-driven setting the application needs.
type Config struct {
	AppEnv  string
	AppPort string

	DatabaseURL string

	AIServiceURL string

	WhatsAppAPIURL        string
	WhatsAppAccessToken   string
	WhatsAppPhoneNumberID string
	WhatsAppVerifyToken   string
	// WhatsAppAppSecret is the Meta App Secret used to verify the
	// X-Hub-Signature-256 header on every webhook POST.
	WhatsAppAppSecret string
	// AllowUnsignedWebhooks is true ONLY when APP_ENV=development and no
	// App Secret is configured (local simulation without Meta). In every
	// other environment a missing secret is a startup error.
	AllowUnsignedWebhooks bool

	DashboardURL string
}

// Load reads configuration from environment variables.
// It does not read .env files directly; that is expected to be handled by
// Docker Compose / the process environment. Missing required variables
// produce a clear error rather than a silent zero-value.
func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:                getEnv("APP_ENV", "development"),
		AppPort:               getEnv("APP_PORT", "8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		AIServiceURL:          getEnv("AI_SERVICE_URL", "http://ai:8000"),
		WhatsAppAPIURL:        getEnv("WHATSAPP_API_URL", "https://graph.facebook.com/v19.0"),
		WhatsAppAccessToken:   os.Getenv("WHATSAPP_ACCESS_TOKEN"),
		WhatsAppPhoneNumberID: os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppVerifyToken:   os.Getenv("WHATSAPP_VERIFY_TOKEN"),
		WhatsAppAppSecret:     os.Getenv("WHATSAPP_APP_SECRET"),
		DashboardURL:          getEnv("DASHBOARD_URL", "http://localhost:3000"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	if cfg.WhatsAppAppSecret == "" {
		if cfg.AppEnv != "development" {
			return nil, fmt.Errorf("WHATSAPP_APP_SECRET is required when APP_ENV=%s (webhook signatures must be verified)", cfg.AppEnv)
		}
		cfg.AllowUnsignedWebhooks = true
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
