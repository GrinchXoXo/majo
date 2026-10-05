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

	WhatsAppAPIURL         string
	WhatsAppAccessToken    string
	WhatsAppPhoneNumberID  string
	WhatsAppVerifyToken    string

	DashboardURL string
}

// Load reads configuration from environment variables.
// It does not read .env files directly; that is expected to be handled by
// Docker Compose / the process environment. Missing required variables
// produce a clear error rather than a silent zero-value.
func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:                getEnv("APP_ENV", "development"),
		AppPort:                getEnv("APP_PORT", "8080"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		AIServiceURL:           getEnv("AI_SERVICE_URL", "http://ai:8000"),
		WhatsAppAPIURL:         getEnv("WHATSAPP_API_URL", "https://graph.facebook.com/v19.0"),
		WhatsAppAccessToken:    os.Getenv("WHATSAPP_ACCESS_TOKEN"),
		WhatsAppPhoneNumberID:  os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppVerifyToken:    os.Getenv("WHATSAPP_VERIFY_TOKEN"),
		DashboardURL:           getEnv("DASHBOARD_URL", "http://localhost:3000"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
