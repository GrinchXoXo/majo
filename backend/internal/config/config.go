// Package config centralizes environment configuration for the Middleman backend.
// Phase 0 rule: all env vars are loaded once, at startup, in one place.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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

	// OperatorAPIToken (Phase 4A Step 4) is the shared secret that protects
	// operator curation actions (listing edit/publish/reject and listing
	// media previews): callers must send "Authorization: Bearer <token>".
	// Required outside development. There is no per-operator identity.
	OperatorAPIToken string
	// AllowUnauthenticatedOperator is true ONLY when APP_ENV=development
	// and no OperatorAPIToken is configured (local work).
	AllowUnauthenticatedOperator bool

	// --- Phase 4A.1: durable media ingestion ---
	// MediaStorageDir is the root of the local storage.Provider. Must be a
	// persistent volume in any deployed environment.
	MediaStorageDir   string
	MediaMaxImageSize int64 // bytes
	MediaMaxVideoSize int64 // bytes
	// MediaBackfillWindow: at startup, vendor media received within this
	// window that has neither an asset nor a job is enqueued. WhatsApp
	// media IDs stay downloadable for about 7 days.
	MediaBackfillWindow time.Duration

	JobsPollInterval time.Duration
	JobsConcurrency  int
	JobsMaxAttempts  int
	JobsBackoffBase  time.Duration
	JobsBackoffMax   time.Duration
	// JobsLease: a RUNNING job not finished within this time is assumed
	// abandoned (worker crashed) and becomes claimable again.
	JobsLease time.Duration

	// SupplyGroupingWindow (Phase 4A.2) [PARAM]: consecutive vendor supply
	// messages sent within this time of a submission's first message can
	// form one SupplySignal.
	SupplyGroupingWindow time.Duration

	// --- Phase 4A.5: supply lifecycle, freshness, geographic expansion ---
	// All [PARAM]; validated together by supply.PolicyConfig.Validate at
	// startup. Days are whole days counted from publication.
	//
	// SupplyFreshnessWindow: verification older than this = STALE (not
	// expired). SUPPLY_FRESHNESS_WINDOW, default 72h.
	SupplyFreshnessWindow time.Duration
	// SupplyMaxLifecycleDays: hard lifecycle ceiling. Default 60.
	SupplyMaxLifecycleDays int
	// Geographic expansion stage starts (days since publication):
	// local 0, nearby 20, state 30, region 45.
	SupplyGeoLocalDays  int
	SupplyGeoNearbyDays int
	SupplyGeoStateDays  int
	SupplyGeoRegionDays int
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
		MediaStorageDir:       getEnv("MEDIA_STORAGE_DIR", "data/media"),
		OperatorAPIToken:      os.Getenv("OPERATOR_API_TOKEN"),
	}

	var err error
	ints := []struct {
		key  string
		def  int64
		dest *int64
	}{
		{"MEDIA_MAX_IMAGE_BYTES", 5 << 20, &cfg.MediaMaxImageSize},
		{"MEDIA_MAX_VIDEO_BYTES", 16 << 20, &cfg.MediaMaxVideoSize},
	}
	for _, i := range ints {
		if *i.dest, err = getInt(i.key, i.def); err != nil {
			return nil, err
		}
	}
	var concurrency, attempts int64
	if concurrency, err = getInt("JOBS_CONCURRENCY", 2); err != nil {
		return nil, err
	}
	if attempts, err = getInt("JOBS_MAX_ATTEMPTS", 8); err != nil {
		return nil, err
	}
	cfg.JobsConcurrency, cfg.JobsMaxAttempts = int(concurrency), int(attempts)
	durs := []struct {
		key  string
		def  time.Duration
		dest *time.Duration
	}{
		{"MEDIA_BACKFILL_WINDOW", 7 * 24 * time.Hour, &cfg.MediaBackfillWindow},
		{"JOBS_POLL_INTERVAL", 2 * time.Second, &cfg.JobsPollInterval},
		{"JOBS_BACKOFF_BASE", 30 * time.Second, &cfg.JobsBackoffBase},
		{"JOBS_BACKOFF_MAX", 30 * time.Minute, &cfg.JobsBackoffMax},
		{"JOBS_LEASE", 5 * time.Minute, &cfg.JobsLease},
		{"SUPPLY_GROUPING_WINDOW", 120 * time.Second, &cfg.SupplyGroupingWindow},
		{"SUPPLY_FRESHNESS_WINDOW", 72 * time.Hour, &cfg.SupplyFreshnessWindow},
	}
	for _, d := range durs {
		if *d.dest, err = getDuration(d.key, d.def); err != nil {
			return nil, err
		}
	}

	days := []struct {
		key  string
		def  int
		dest *int
	}{
		{"SUPPLY_MAX_LIFECYCLE_DAYS", 60, &cfg.SupplyMaxLifecycleDays},
		{"SUPPLY_GEO_LOCAL_DAYS", 0, &cfg.SupplyGeoLocalDays},
		{"SUPPLY_GEO_NEARBY_DAYS", 20, &cfg.SupplyGeoNearbyDays},
		{"SUPPLY_GEO_STATE_DAYS", 30, &cfg.SupplyGeoStateDays},
		{"SUPPLY_GEO_REGION_DAYS", 45, &cfg.SupplyGeoRegionDays},
	}
	for _, d := range days {
		if *d.dest, err = getDays(d.key, d.def); err != nil {
			return nil, err
		}
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

	if cfg.OperatorAPIToken == "" {
		// Fail closed: running curation without a token needs APP_ENV to be
		// SET to development explicitly; the APP_ENV default does not count,
		// so a deployment that forgets both settings refuses to start.
		if env, set := os.LookupEnv("APP_ENV"); !set || env != "development" {
			return nil, fmt.Errorf("OPERATOR_API_TOKEN is required unless APP_ENV=development is set explicitly (APP_ENV=%q); listing publication must not be publicly callable", env)
		}
		cfg.AllowUnauthenticatedOperator = true
	} else if len(cfg.OperatorAPIToken) < 16 {
		return nil, fmt.Errorf("OPERATOR_API_TOKEN must be at least 16 characters")
	}

	return cfg, nil
}

func getInt(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// getDays parses a whole number of days (0 allowed).
func getDays(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 3650 {
		return 0, fmt.Errorf("%s must be a whole number of days between 0 and 3650, got %q", key, v)
	}
	return n, nil
}

func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 30s or 5m, got %q", key, v)
	}
	return d, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
