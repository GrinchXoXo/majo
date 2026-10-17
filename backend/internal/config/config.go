// Package config centralizes environment configuration for the Majo backend.
// Phase 0 rule: all env vars are loaded once, at startup, in one place.
package config

import (
	"fmt"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/monetization"
	"github.com/middleman/backend/internal/payments"
	"github.com/middleman/backend/internal/tone"
	"os"
	"strconv"
	"strings"
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
	// CORSAllowedOrigins (reuse-audit R1) are the browser origins allowed to
	// call the API: CORS_ALLOWED_ORIGINS (comma-separated), default
	// DASHBOARD_URL. Any other website gets no CORS grant.
	CORSAllowedOrigins []string

	// OperatorAPIToken (Phase 4A Step 4) is the shared secret for the operator
	// control plane: callers send "Authorization: Bearer <token>". Required
	// outside development. Since reuse-audit R1 it is the bootstrap /
	// break-glass credential; day-to-day operators use named tokens
	// (cmd/operatorctl), which record who acted and carry a role.
	OperatorAPIToken string

	// Error tracking (reuse-audit R1 pilot). Off unless SENTRY_DSN is set;
	// any Sentry-protocol endpoint works (GlitchTip self-hosted, Sentry).
	SentryDSN         string
	SentryEnvironment string // default APP_ENV
	SentryRelease     string // e.g. majo-backend@<git sha>; default majo-backend@unknown

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

	// --- Phase 4B: Discovery & Distribution (all [PARAM]; validated by
	// distribution.Config.Validate at startup) ---
	//
	// DiscoveryMode is the feature gate: OFF (default) or INTERNAL (staff
	// test recipients only). CONTROLLED_PILOT and ON are Phase 4C+ and are
	// refused at startup.
	DiscoveryMode string
	// DiscoveryInternalRecipients: the only channel addresses (wa_ids) that
	// may receive discovery while INTERNAL. Comma-separated.
	DiscoveryInternalRecipients []string
	DiscoveryArms               []string
	// Frequency caps and dedupe.
	DiscoveryMaxDeliveriesPerBuyer int
	DiscoveryCapPeriod             time.Duration
	DiscoveryExposureWindow        time.Duration
	DiscoveryListingsPerDelivery   int
	// DiscoveryDeliveryWindow "HH:MM-HH:MM" in DiscoveryTimezone ("" = any time).
	DiscoveryDeliveryWindow string
	DiscoveryTimezone       string
	// DiscoveryInteractionTTL: taps on a delivery older than this are expired.
	DiscoveryInteractionTTL time.Duration
	// DiscoveryNegativeWindow: how long "not interested" suppresses a category.
	DiscoveryNegativeWindow time.Duration
	// Approved template for buyers outside the 24h window ("" = none).
	DiscoveryTemplateName     string
	DiscoveryTemplateLanguage string
	// Pilot batch item 3: approved utility template for vendor requests
	// outside the 24-hour window. Empty = free text only.
	VendorRequestTemplateName     string
	VendorRequestTemplateLanguage string
	// Pilot batch item 5: deal follow-up, ratings and the free-deal counter.
	DealFollowUp         bool
	DealFollowUpDelay    time.Duration
	DealAnswerTTL        time.Duration
	FreeDeals            int
	IntroShareBuyerPhone bool
	// Pilot batch item 7: tone layer for buyer messages (TONE_LAYER
	// off|shadow|on, default off) and the per-message rewrite timeout.
	ToneLayer   tone.Mode
	ToneTimeout time.Duration
	// Pilot batch item 8: monetization scaffold. MONETIZATION_STAGE (A),
	// PAYMENTS_PROVIDER (none) and FEATURE_<LINE> flags, all off; a line
	// switched on before its stage and provider exist stops startup.
	Monetization monetization.Settings
	// DiscoveryConsentWording is the current opt-in wording version.
	DiscoveryConsentWording string
	// DiscoveryEnquiryConfirmation: how "Ask about it" requests become
	// CONFIRMED (Lock §O-1): "operator" (default) or "auto".
	DiscoveryEnquiryConfirmation string
	// DiscoveryRankingWeights overrides discovery weights ("key=value,...").
	DiscoveryRankingWeights string
	// DiscoveryOutboundMediaTTL: how long an uploaded outbound media ID is
	// reused before re-uploading the stored asset.
	DiscoveryOutboundMediaTTL time.Duration

	// --- Phase 4C correction: autonomous runtime (all [PARAM]) ---
	// Majo is autonomous by default; each flag can switch one autonomous
	// path off (the path still exists). See internal/autonomy.
	AutonomyFlags  autonomy.Flags
	AutonomyParams autonomy.Params
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
	cfg.SentryDSN = os.Getenv("SENTRY_DSN")
	cfg.SentryEnvironment = getEnv("SENTRY_ENVIRONMENT", cfg.AppEnv)
	cfg.SentryRelease = os.Getenv("SENTRY_RELEASE")
	cfg.CORSAllowedOrigins = splitList(getEnv("CORS_ALLOWED_ORIGINS", cfg.DashboardURL))

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
	// Legacy name (Phase 4A.4): honoured when the new one is not set, so an
	// existing deployment does not silently fall back to the default.
	if os.Getenv("SUPPLY_FRESHNESS_WINDOW") == "" && os.Getenv("LISTING_FRESHNESS_WINDOW") != "" {
		if cfg.SupplyFreshnessWindow, err = getDuration("LISTING_FRESHNESS_WINDOW", 72*time.Hour); err != nil {
			return nil, err
		}
	}

	cfg.DiscoveryMode = getEnv("DISCOVERY_MODE", "OFF")
	cfg.DiscoveryInternalRecipients = splitList(os.Getenv("DISCOVERY_INTERNAL_RECIPIENTS"))
	cfg.DiscoveryArms = splitList(getEnv("DISCOVERY_ARMS", "ARM_A,ARM_B"))
	cfg.DiscoveryDeliveryWindow = os.Getenv("DISCOVERY_DELIVERY_WINDOW")
	cfg.DiscoveryTimezone = getEnv("DISCOVERY_TIMEZONE", "Africa/Lagos")
	cfg.DiscoveryTemplateName = os.Getenv("DISCOVERY_TEMPLATE_NAME")
	cfg.DiscoveryTemplateLanguage = getEnv("DISCOVERY_TEMPLATE_LANGUAGE", "en")
	cfg.VendorRequestTemplateName = strings.TrimSpace(os.Getenv("VENDOR_REQUEST_TEMPLATE_NAME"))
	cfg.VendorRequestTemplateLanguage = getEnv("VENDOR_REQUEST_TEMPLATE_LANGUAGE", "en")
	cfg.DiscoveryConsentWording = getEnv("DISCOVERY_CONSENT_WORDING", "v1")
	cfg.DiscoveryEnquiryConfirmation = getEnv("DISCOVERY_ENQUIRY_CONFIRMATION", "operator")
	cfg.DiscoveryRankingWeights = os.Getenv("DISCOVERY_RANKING_WEIGHTS")
	discoveryDurs := []struct {
		key  string
		def  time.Duration
		dest *time.Duration
	}{
		{"DISCOVERY_CAP_PERIOD", 72 * time.Hour, &cfg.DiscoveryCapPeriod},
		{"DISCOVERY_EXPOSURE_WINDOW", 14 * 24 * time.Hour, &cfg.DiscoveryExposureWindow},
		{"DISCOVERY_INTERACTION_TTL", 7 * 24 * time.Hour, &cfg.DiscoveryInteractionTTL},
		{"DISCOVERY_NEGATIVE_WINDOW", 30 * 24 * time.Hour, &cfg.DiscoveryNegativeWindow},
		{"DISCOVERY_OUTBOUND_MEDIA_TTL", 25 * 24 * time.Hour, &cfg.DiscoveryOutboundMediaTTL},
	}
	for _, d := range discoveryDurs {
		if *d.dest, err = getDuration(d.key, d.def); err != nil {
			return nil, err
		}
	}
	var perBuyer, perDelivery int64
	if perBuyer, err = getInt("DISCOVERY_MAX_DELIVERIES_PER_BUYER", 1); err != nil {
		return nil, err
	}
	if perDelivery, err = getInt("DISCOVERY_LISTINGS_PER_DELIVERY", 3); err != nil {
		return nil, err
	}
	cfg.DiscoveryMaxDeliveriesPerBuyer, cfg.DiscoveryListingsPerDelivery = int(perBuyer), int(perDelivery)

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

	if err := loadAutonomy(cfg); err != nil {
		return nil, err
	}
	if err := loadDeals(cfg); err != nil {
		return nil, err
	}
	if cfg.ToneLayer, err = tone.ParseMode(os.Getenv("TONE_LAYER")); err != nil {
		return nil, err
	}
	if cfg.ToneTimeout, err = getDuration("TONE_TIMEOUT", 5*time.Second); err != nil {
		return nil, err
	}
	if err := loadMonetization(cfg); err != nil {
		return nil, err
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

// splitList parses a comma-separated list, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadAutonomy reads the autonomous-runtime flags and parameters.
func loadAutonomy(cfg *Config) error {
	f := autonomy.AllOn()
	flags := []struct {
		key  string
		dest *bool
	}{
		{"AUTO_CONFIRM_CLEAR_REQUESTS", &f.AutoConfirmClearRequests},
		{"AUTO_ROUTE_DISCOVERY_REQUESTS", &f.AutoRouteDiscoveryRequests},
		{"AUTO_ROUTE_MATCHED_REQUESTS", &f.AutoRouteMatchedRequests},
		{"AUTO_RESOLVE_VENDOR_CONTEXT", &f.AutoResolveVendorContext},
		{"AUTO_CLARIFY_REQUESTS", &f.AutoClarifyRequests},
		{"AUTO_CLARIFY_VENDOR_RESPONSES", &f.AutoClarifyVendorResponses},
		{"AUTO_RETRY_DELIVERY", &f.AutoRetryDelivery},
		{"AUTO_PUBLISH_TRUSTED_VENDOR_LISTINGS", &f.AutoPublishTrustedVendorLists},
		{"AUTO_RECOVER_STALLED_REQUESTS", &f.AutoRecoverStalledRequests},
	}
	for _, fl := range flags {
		v, err := getBool(fl.key, *fl.dest)
		if err != nil {
			return err
		}
		*fl.dest = v
	}
	if f.AutoClarifyVendorResponses && !f.AutoResolveVendorContext {
		return fmt.Errorf("AUTO_CLARIFY_VENDOR_RESPONSES=true requires AUTO_RESOLVE_VENDOR_CONTEXT=true (the vendor's answer is read through the context resolver)")
	}
	p := autonomy.DefaultParams()
	if v := os.Getenv("AUTO_CONFIRM_MIN_CONFIDENCE"); v != "" {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("AUTO_CONFIRM_MIN_CONFIDENCE must be a number between 0 and 1, got %q", v)
		}
		p.ConfirmMinConfidence = x
	}
	ints := []struct {
		key  string
		dest *int
	}{
		{"AUTO_CLARIFY_MAX_QUESTIONS", &p.MaxClarifications},
		{"AUTO_ROUTE_MIN_SCORE", &p.RouteMinScore},
		{"AUTO_ROUTE_MAX_VENDORS", &p.RouteMaxVendors},
		{"AUTO_ROUTE_VENDOR_MAX_OPEN", &p.RouteVendorMaxOpen},
		{"AUTO_VENDOR_CLARIFY_MAX_ROUNDS", &p.VendorClarifyMaxRounds},
		{"AUTO_RETRY_MAX_ATTEMPTS", &p.RetryMaxAttempts},
		{"AUTO_RECOVERY_MAX_ROUNDS", &p.RecoveryMaxRounds},
		{"AUTO_ROUTE_MAX_VENDORS_PER_REQUEST", &p.MaxVendorsPerRequest},
	}
	for _, i := range ints {
		n, err := getInt(i.key, int64(*i.dest))
		if err != nil {
			return err
		}
		*i.dest = int(n)
	}
	durs := []struct {
		key  string
		dest *time.Duration
	}{
		{"AUTO_CLARIFICATION_TTL", &p.ClarificationTTL},
		{"VENDOR_REQUEST_TTL", &p.VendorRequestTTL},
		{"AUTO_CONTEXT_WINDOW", &p.ContextWindow},
		{"AUTO_PENDING_NOTICE_TTL", &p.PendingNoticeTTL},
		{"AUTO_RETRY_FIRST_DELAY", &p.RetryFirstDelayOverride},
		{"AUTO_LATE_REPLY_WINDOW", &p.LateReplyWindow},
	}
	for _, d := range durs {
		v, err := getDuration(d.key, *d.dest)
		if err != nil {
			return err
		}
		*d.dest = v
	}
	p.DefaultLocation = strings.TrimSpace(os.Getenv("PILOT_DEFAULT_LOCATION"))
	if v := os.Getenv("AUTO_BROADEN_EXCLUDED_CATEGORIES"); v != "" {
		for _, c := range splitList(v) {
			p.BroadenExcludedCategories = append(p.BroadenExcludedCategories, strings.ToLower(c))
		}
	}
	// AUTO_RESTRICTED_ITEM_TERMS replaces the default list; "off" disables it.
	if v := strings.TrimSpace(os.Getenv("AUTO_RESTRICTED_ITEM_TERMS")); v != "" {
		p.RestrictedTerms = nil
		if !strings.EqualFold(v, "off") {
			for _, t := range splitList(v) {
				p.RestrictedTerms = append(p.RestrictedTerms, strings.ToLower(t))
			}
		}
	}
	if err := p.Validate(); err != nil {
		return err
	}
	cfg.AutonomyFlags, cfg.AutonomyParams = f, p
	return nil
}

func getBool(key string, fallback bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return fallback, nil
	case "true", "1", "on", "yes":
		return true, nil
	case "false", "0", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s must be true or false, got %q", key, os.Getenv(key))
}

// loadDeals reads the deal follow-up settings (pilot batch item 5).
func loadDeals(cfg *Config) error {
	var err error
	if cfg.DealFollowUp, err = getBool("DEAL_FOLLOWUP", true); err != nil {
		return err
	}
	if cfg.DealFollowUpDelay, err = getDuration("DEAL_FOLLOWUP_DELAY", 6*time.Hour); err != nil {
		return err
	}
	if cfg.DealAnswerTTL, err = getDuration("DEAL_ANSWER_TTL", 72*time.Hour); err != nil {
		return err
	}
	free, err := getInt("FREE_DEALS", 100)
	if err != nil {
		return err
	}
	if free < 0 || free > 1_000_000 {
		return fmt.Errorf("FREE_DEALS must be between 0 and 1000000")
	}
	cfg.FreeDeals = int(free)
	if cfg.IntroShareBuyerPhone, err = getBool("INTRO_SHARE_BUYER_CONTACT", false); err != nil {
		return err
	}
	if cfg.DealFollowUpDelay <= 0 || cfg.DealAnswerTTL <= 0 {
		return fmt.Errorf("DEAL_FOLLOWUP_DELAY and DEAL_ANSWER_TTL must be positive")
	}
	if cfg.DealFollowUpDelay >= 24*time.Hour {
		// Not an error, but the buyer's 24h window will usually have closed:
		// the question would not arrive without a template.
		fmt.Fprintln(os.Stderr, "warning: DEAL_FOLLOWUP_DELAY >= 24h: the follow-up will usually miss the buyer's WhatsApp window")
	}
	return nil
}

// loadMonetization reads the revenue-line flags (pilot batch item 8).
func loadMonetization(cfg *Config) error {
	m := monetization.Default()
	var err error
	if m.Stage, err = monetization.ParseStage(os.Getenv("MONETIZATION_STAGE")); err != nil {
		return err
	}
	if m.Provider, err = payments.ParseKind(os.Getenv("PAYMENTS_PROVIDER")); err != nil {
		return err
	}
	for _, l := range monetization.Lines {
		on, err := getBool(monetization.EnvName(l), false)
		if err != nil {
			return err
		}
		if on {
			m.Enabled[l] = true
		}
	}
	if err := m.Validate(); err != nil {
		return err
	}
	cfg.Monetization = m
	return nil
}
