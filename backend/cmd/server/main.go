// Majo backend entrypoint.
//
// This is the composition root (PRD 0 FINAL §19): the only place where
// infrastructure implementations are instantiated and wired into domain
// services and HTTP handlers. Domain packages never construct their own
// infrastructure dependencies.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/infrastructure/postgres"
	"github.com/middleman/backend/infrastructure/storage"
	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/config"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/deal"
	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/distribution"
	"github.com/middleman/backend/internal/errortrack"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/logging"
	"github.com/middleman/backend/internal/marketplace"
	"github.com/middleman/backend/internal/media"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/payments"
	"github.com/middleman/backend/internal/presentation"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/tone"
	"github.com/middleman/backend/internal/transaction"
	"github.com/middleman/backend/internal/workflow"
)

func main() {
	// `server healthcheck` is the container HEALTHCHECK (the distroless image
	// has no shell or curl): exit 0 when this host's /health answers 200.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	logger := logging.New("middleman-backend")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading configuration", "error", err)
		os.Exit(1)
	}

	// Reuse-audit R1 pilot: ERROR records and handler panics go to a
	// Sentry-protocol error tracker when SENTRY_DSN is set (scrubbed).
	flushErrors, err := errortrack.Init(errortrack.Config{DSN: cfg.SentryDSN, Environment: cfg.SentryEnvironment,
		Release: cfg.SentryRelease, ServerName: "majo-backend"})
	if err != nil {
		logger.Error("error tracking disabled: invalid SENTRY_DSN", "error", err)
	}
	defer flushErrors()
	logger = slog.New(errortrack.SlogHandler(logger.Handler()))
	logger.Info("error tracking", "enabled", errortrack.Enabled())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- Infrastructure ---

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("connecting to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	migrationsDir := "migrations"
	if err := postgres.RunMigrations(ctx, pool, migrationsDir); err != nil {
		logger.Error("running migrations", "error", err)
		os.Exit(1)
	}
	logger.Info("migrations applied successfully")

	aiProvider := ai.NewHTTPProvider(cfg.AIServiceURL)

	whatsappProvider := whatsapp.NewCloudAPIProvider(
		cfg.WhatsAppAPIURL,
		cfg.WhatsAppAccessToken,
		cfg.WhatsAppPhoneNumberID,
		cfg.WhatsAppVerifyToken,
		cfg.WhatsAppAppSecret,
	)
	if cfg.AllowUnsignedWebhooks {
		logger.Warn("WHATSAPP_APP_SECRET is not set: webhook signature verification is DISABLED (development only). " +
			"Any caller that can reach /webhooks/whatsapp can impersonate buyers and vendors.")
	}

	convRepo := postgres.NewConversationRepository(pool)
	// §23A: every outbound message goes through the recorder, which persists
	// it with its WhatsApp message ID and business purpose so delivery/read
	// receipts can be measured. Services keep their SendText interface.
	outbound := conversation.NewRecordingSender(whatsappProvider, convRepo, "whatsapp", logger)
	// Pilot batch item 7: buyer messages may be rephrased in a Nigerian
	// register; facts never change (internal/tone). Off unless TONE_LAYER is set.
	if cfg.ToneLayer != tone.Off {
		outbound.SetToner(tone.New(tone.Config{Mode: cfg.ToneLayer, Timeout: cfg.ToneTimeout}, aiProvider, logger))
	}
	logger.Info("tone layer", "mode", string(cfg.ToneLayer), "timeout", cfg.ToneTimeout.String())
	buyerRepo := postgres.NewBuyerRepository(pool)
	requestRepo := postgres.NewRequestRepository(pool)
	vendorRepo := postgres.NewVendorRepository(pool)
	supplyRepo := postgres.NewSupplyRepository(pool)
	routingRepo := postgres.NewRoutingRepository(pool)
	eventWriter := postgres.NewEventWriter(pool)
	auditWriter := postgres.NewAuditWriter(pool)
	listingRepo := postgres.NewSupplyListingRepository(pool)
	offerRepo := postgres.NewOfferRepository(pool)
	txRepo := postgres.NewTransactionRepository(pool)

	// --- Application services ---

	requestService := request.NewService(
		requestRepo, convRepo, aiProvider, eventWriter, auditWriter, outbound, logger,
	)

	routingService := routing.NewService(
		vendorRepo, supplyRepo, routingRepo, requestRepo, aiProvider, eventWriter, auditWriter, outbound, logger,
	)

	// Phase 4A.5: the ONE canonical supply-eligibility policy (lifecycle,
	// freshness, geographic expansion). Invalid configuration stops startup.
	const day = 24 * time.Hour
	supplyPolicy, err := supply.NewEligibilityPolicy(supply.PolicyConfig{
		MaxLifecycle:    time.Duration(cfg.SupplyMaxLifecycleDays) * day,
		FreshnessWindow: cfg.SupplyFreshnessWindow,
		GeoLocalAfter:   time.Duration(cfg.SupplyGeoLocalDays) * day,
		GeoNearbyAfter:  time.Duration(cfg.SupplyGeoNearbyDays) * day,
		GeoStateAfter:   time.Duration(cfg.SupplyGeoStateDays) * day,
		GeoRegionAfter:  time.Duration(cfg.SupplyGeoRegionDays) * day,
	}, nil)
	if err != nil {
		logger.Error("invalid supply lifecycle configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("supply eligibility policy", "max_lifecycle_days", cfg.SupplyMaxLifecycleDays,
		"freshness_window", cfg.SupplyFreshnessWindow.String(), "geo_days", fmt.Sprintf("local=%d nearby=%d state=%d region=%d",
			cfg.SupplyGeoLocalDays, cfg.SupplyGeoNearbyDays, cfg.SupplyGeoStateDays, cfg.SupplyGeoRegionDays))

	listingService := supply.NewListingService(supplyRepo, listingRepo, vendorRepo, listingRepo, eventWriter, auditWriter, logger)
	listingService.SetEligibilityPolicy(supplyPolicy)
	routingService.SetListingEvidence(listingRepo, supplyPolicy) // Phase 4A: matching reads eligible listings
	// Phase 4B: listing-origin demand asks the listing's vendor to re-confirm
	// it; an AVAILABLE reply is fresh verification of that listing.
	routingService.SetListingOrigin(listingRepo, listingService)
	offerService := offer.NewService(offerRepo, routingRepo, requestRepo, eventWriter, auditWriter, logger)
	txService := transaction.NewService(txRepo, offerRepo, requestRepo, eventWriter, auditWriter, logger)

	// Bounded worker pool for background extraction (PRD 1 §12, PRD 0.2 §19).
	// 4 workers / 256-deep queue is deliberately generous for pilot-scale
	// traffic; revisit only if evidence shows it's insufficient.
	jobRunner := jobs.NewRunner(logger, 4, 256)

	// --- Phase 3.1: buyer-side marketplace loop ---
	// OfferDelivery reacts to vendor replies (via routing's ResponseListener)
	// and delivers offers to the buyer; BuyerInbound resolves each buyer
	// message to a new request or an offer selection.
	offerDelivery := marketplace.NewOfferDelivery(
		requestRepo, convRepo, offerService, offerRepo, offerRepo, routingRepo, txRepo, outbound, eventWriter, logger,
	)
	routingService.SetResponseListener(offerDelivery)
	contextResolver := marketplace.NewContextResolver(offerRepo, offerRepo)
	buyerInbound := marketplace.NewBuyerInbound(
		contextResolver, requestService, requestRepo, txService, offerRepo, routingRepo, outbound, eventWriter, jobRunner, logger,
	)

	// --- Phase 4A.1: durable media ingestion ---
	// Vendor media -> MEDIA_DOWNLOAD job (Postgres) -> in-process worker ->
	// Graph download -> validate -> SHA-256 -> storage.Provider -> MediaAsset.
	mediaStore, err := storage.NewLocalProvider(cfg.MediaStorageDir)
	if err != nil {
		logger.Error("initialising media storage", "error", err)
		os.Exit(1)
	}
	jobStore := postgres.NewJobRepository(pool)
	mediaIngestor := media.NewIngestor(
		jobStore, postgres.NewMediaAssetRepository(pool), mediaStore,
		map[string]media.Downloader{"whatsapp": whatsappProvider}, eventWriter,
		media.Limits{MaxImageBytes: cfg.MediaMaxImageSize, MaxVideoBytes: cfg.MediaMaxVideoSize},
		cfg.JobsMaxAttempts, logger,
	)
	durableWorker := jobs.NewWorker(jobStore, jobs.WorkerConfig{
		PollInterval: cfg.JobsPollInterval, Lease: cfg.JobsLease, Concurrency: cfg.JobsConcurrency,
		Retry: jobs.RetryPolicy{BaseDelay: cfg.JobsBackoffBase, MaxDelay: cfg.JobsBackoffMax},
	}, logger)
	durableWorker.Register(media.JobTypeDownload, mediaIngestor.Handle, mediaIngestor.OnFailure)
	durableWorker.RegisterFailureEvent(media.JobTypeDownload, mediaIngestor.FailureEvent)

	// --- Phase 4C correction: autonomous runtime ---
	// Majo is autonomous by default: the workflow engine confirms clear
	// requests, asks the buyer what it needs, routes by policy, resolves
	// vendor replies from context, asks vendors when unclear and recovers
	// failed deliveries. Operators are the control plane (overrides and the
	// exception queue), never a normal step.
	autonomyRepo := postgres.NewAutonomyRepository(pool)
	workflowRepo := postgres.NewWorkflowRepository(pool)
	engine := workflow.New(workflow.Config{Flags: cfg.AutonomyFlags, Params: cfg.AutonomyParams, JobMaxAttempts: cfg.JobsMaxAttempts}, workflow.Deps{
		Store: autonomyRepo, Context: workflowRepo, Requests: requestService, ReqRepo: requestRepo, Router: routingService,
		Routes: routingRepo, Listings: listingRepo, Vendors: vendorRepo, Extractor: aiProvider, Messenger: outbound, Convs: convRepo,
		Policy: supplyPolicy, Jobs: jobStore, Events: eventWriter, Logger: logger, Async: jobRunner.Enqueue,
		// Phase 4C hardening: vendor-request deadlines and recovery, queued
		// buyer notices, operator holds, provider-reported failures.
		VendorRequests: routingRepo, Recovery: workflowRepo, Notices: autonomyRepo, Holds: autonomyRepo, OfferReverter: offerRepo,
		Pauses: autonomyRepo, // pilot batch item 4: global automation pause
	})
	requestService.SetListener(engine)
	routingService.SetAutonomy(&routing.VendorAutonomy{Flags: cfg.AutonomyFlags, Params: cfg.AutonomyParams, Store: autonomyRepo,
		Repo: routingRepo, Quoted: convRepo, Paused: engine.AutomationPaused})
	routingService.SetSendFailureListener(engine)
	routingService.SetLifecycleListener(engine)
	// Pilot batch item 3: vendor requests outside the 24h window go as the
	// approved template (VENDOR_REQUEST_TEMPLATE_NAME; empty = free text only).
	routingService.SetVendorTemplate(routing.VendorTemplate{Name: cfg.VendorRequestTemplateName, Language: cfg.VendorRequestTemplateLanguage},
		outbound, convRepo)
	if cfg.VendorRequestTemplateName == "" {
		logger.Warn("no vendor-request template configured: vendors who have not written to Majo in 24h cannot be reached (set VENDOR_REQUEST_TEMPLATE_NAME once Meta approves it)")
	}
	offerDelivery.SetFailureListener(engine)
	offerDelivery.SetAutomationHooks(engine)
	engine.SetOfferDeliverer(func(ctx context.Context, id uuid.UUID) error {
		_, err := offerDelivery.DeliverPendingOffers(ctx, id)
		return err
	})
	buyerInbound.SetAutonomy(engine)
	engine.SetBuyerPaths(buyerInbound)
	listingService.SetDraftListener(engine)
	engine.SetListingPublisher(listingPublisher{listingService, workflowRepo})
	engine.Register(durableWorker)

	// Pilot batch item 5: introduce buyer and vendor at the buyer's choice,
	// then follow up ("did you buy / sell?"), ratings and the free-deal counter.
	dealRepo := postgres.NewDealRepository(pool)
	dealService := deal.NewService(deal.Config{Enabled: cfg.DealFollowUp, FollowUpDelay: cfg.DealFollowUpDelay, AnswerTTL: cfg.DealAnswerTTL,
		FreeDeals: cfg.FreeDeals, ShareBuyerContact: cfg.IntroShareBuyerPhone}, deal.Deps{
		Repo: dealRepo, Offers: offerRepo, Requests: requestRepo, Vendors: vendorRepo, Conversations: convRepo, References: dealRepo,
		Messenger: outbound, Store: autonomyRepo, Jobs: jobStore, Events: eventWriter, Logger: logger, Holds: engine,
		VendorBusy: func(ctx context.Context, vendorID uuid.UUID) bool {
			active, err := routingRepo.ListActiveByVendor(ctx, vendorID)
			return err == nil && len(active) > 0
		},
	})
	dealService.Register(durableWorker)
	txService.SetListener(dealService)
	buyerInbound.SetIntroducer(dealService)
	engine.SetDeals(dealService)
	routingService.SetDeals(dealService)
	logger.Info("autonomous runtime", "flags", cfg.AutonomyFlags.Map(), "route_max_vendors", cfg.AutonomyParams.RouteMaxVendors,
		"route_min_score", cfg.AutonomyParams.RouteMinScore, "vendor_request_ttl", cfg.AutonomyParams.VendorRequestTTL.String())

	// --- Phase 4A.2: vendor media + captions -> grouped SupplySignal ---
	// routing classifies vendor content; the intake groups it (durable
	// SUPPLY_GROUPING jobs) once media downloads are final.
	supplyIntake := supply.NewIntake(postgres.NewSupplyGroupingRepository(pool), aiProvider, jobStore, vendorRepo,
		cfg.SupplyGroupingWindow, cfg.JobsMaxAttempts, logger)
	routingService.SetSupplyIntake(supplyIntake)
	mediaIngestor.SetSettleListener(supplyIntake)
	durableWorker.Register(supply.JobTypeGroup, supplyIntake.Handle, nil)

	// --- Phase 4A.3: SupplySignal -> DRAFT SupplyListing ---
	// Every new (or newly media-complete) signal is evaluated by a durable
	// SUPPLY_LISTING_DRAFT job. Eligible ones become DRAFT listings; nothing
	// is published automatically (Activate stays an operator action).
	listingService.SetDraftJobs(jobStore, cfg.JobsMaxAttempts)
	supplyIntake.SetSignalListener(listingService)
	durableWorker.Register(supply.JobTypeListingDraft, listingService.HandleDraftJob, nil)
	// Close the "message saved, job not yet enqueued" gap after a crash,
	// and pick up vendor media persisted before durable ingestion existed.
	if n, err := mediaIngestor.Backfill(ctx, time.Now().Add(-cfg.MediaBackfillWindow), 5000); err != nil {
		logger.Error("media backfill failed (will retry on next start)", "error", err)
	} else if n > 0 {
		logger.Info("media backfill enqueued downloads", "count", n)
	}
	engine.Recover(ctx)         // requests left mid-flow by a restart continue
	engine.RecoverTimeouts(ctx) // vendor-request deadlines missing or passed while down
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		durableWorker.Run(ctx)
	}()

	// --- HTTP ---

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpserver.HealthHandler)
	mux.HandleFunc("GET /ready", httpserver.ReadyHandler(pool, aiProvider))

	// --- Phase 4B: Discovery & Distribution (staff-only foundation) ---
	// Discovery is pure over the canonical policy; Distribution owns
	// consent, caps, dedupe, windows, batches, presentation and sending.
	discoveryRepo := postgres.NewDiscoveryRepository(pool)
	discoveryService := discovery.NewService(discovery.NewEngine(supplyPolicy), discoveryRepo)
	weights, err := discovery.ParseWeights(cfg.DiscoveryRankingWeights, discovery.DefaultWeights())
	if err != nil {
		logger.Error("invalid discovery ranking weights", "error", err)
		os.Exit(1)
	}
	window, err := distribution.ParseWindow(cfg.DiscoveryDeliveryWindow, cfg.DiscoveryTimezone)
	if err != nil {
		logger.Error("invalid discovery delivery window", "error", err)
		os.Exit(1)
	}
	internal := map[string]bool{}
	for _, r := range cfg.DiscoveryInternalRecipients {
		internal[r] = true
	}
	var arms []presentation.Arm
	for _, a := range cfg.DiscoveryArms {
		arms = append(arms, presentation.Arm(a))
	}
	distCfg := distribution.Config{
		Mode: distribution.Mode(cfg.DiscoveryMode), InternalRecipients: internal, Arms: arms,
		MaxDeliveriesPerBuyer: cfg.DiscoveryMaxDeliveriesPerBuyer, CapPeriod: cfg.DiscoveryCapPeriod,
		ExposureWindow: cfg.DiscoveryExposureWindow, ListingsPerDelivery: cfg.DiscoveryListingsPerDelivery,
		Window: window, InteractionTTL: cfg.DiscoveryInteractionTTL, NegativeWindow: cfg.DiscoveryNegativeWindow,
		Template:       presentation.TemplateConfig{Name: cfg.DiscoveryTemplateName, Language: cfg.DiscoveryTemplateLanguage},
		ConsentWording: cfg.DiscoveryConsentWording, EnquiryConfirmation: cfg.DiscoveryEnquiryConfirmation,
		Ranking: discovery.RankingParams{Weights: weights}, OutboundMediaTTL: cfg.DiscoveryOutboundMediaTTL,
	}
	if err := distCfg.Validate(); err != nil {
		logger.Error("invalid discovery configuration", "error", err)
		os.Exit(1)
	}
	mediaPublisher := distribution.NewMediaPublisher(postgres.NewMediaAssetRepository(pool), mediaStore, whatsappProvider, cfg.DiscoveryOutboundMediaTTL)
	distributionService := distribution.NewService(distCfg, discoveryRepo, discoveryService, outbound, mediaPublisher, requestService, eventWriter, logger)
	buyerInbound.SetDiscovery(distributionService)
	engine.SetEnquirer(distributionService)
	if os.Getenv("DISCOVERY_ENQUIRY_CONFIRMATION") != "" {
		logger.Warn("DISCOVERY_ENQUIRY_CONFIRMATION is deprecated and ignored: discovery enquiries are confirmed by the autonomous workflow (AUTO_CONFIRM_CLEAR_REQUESTS)")
	}
	logger.Info("discovery distribution", "mode", distCfg.Mode, "internal_recipients", len(internal), "arms", cfg.DiscoveryArms,
		"window", window.Raw, "template_configured", distCfg.Template.Name != "", "ranking_version",
		distCfg.Ranking.Version(supplyPolicy.Config()))

	webhookHandler := httpserver.NewWhatsAppWebhookHandler(
		whatsappProvider, convRepo, convRepo, buyerRepo, vendorRepo, eventWriter, buyerInbound, routingService, jobRunner,
		cfg.WhatsAppVerifyToken, cfg.AllowUnsignedWebhooks, logger,
	)
	webhookHandler.SetMediaIngestion(mediaIngestor)
	webhookHandler.SetOutboundFailureListener(engine) // provider-reported failures are classified and recovered
	mux.Handle("/webhooks/whatsapp", webhookHandler)

	// Phase 4C correction: every operator override of the autonomous loop
	// (correct, confirm, clarify, route, associate, deliver, select) is an
	// authenticated control-plane action.
	// Reuse-audit R1: every /api route is behind the guard (reads need a
	// VIEWER or OPERATOR credential, changes an OPERATOR one). Named operators
	// come from the operators table; the shared token is break-glass.
	operatorAuth := httpserver.NewOperatorAuth(cfg.OperatorAPIToken)
	operatorRepo := postgres.NewOperatorRepository(pool)
	activeOperators, err := operatorRepo.CountActive(ctx)
	if err != nil {
		logger.Error("count operators", "error", err)
		os.Exit(1)
	}
	operatorAuth.SetStore(operatorRepo, activeOperators, logger)
	if operatorAuth.Open() {
		logger.Warn("no OPERATOR_API_TOKEN and no named operators: the operator API is UNAUTHENTICATED. Development only.")
	}
	logger.Info("operator access", "named_operators", activeOperators, "shared_token", cfg.OperatorAPIToken != "", "cors_origins", cfg.CORSAllowedOrigins)
	requestHandlers := httpserver.NewRequestHandlers(requestRepo, requestService, eventWriter)
	if cfg.AutonomyFlags.AutoClarifyRequests {
		requestHandlers.SetClarifier(engine) // operator questions continue the request
	}
	mux.HandleFunc("GET /api/requests", operatorAuth.RequireViewer(requestHandlers.List))
	mux.HandleFunc("GET /api/requests/{id}", operatorAuth.RequireViewer(requestHandlers.Get))
	mux.HandleFunc("PATCH /api/requests/{id}", operatorAuth.Require(requestHandlers.Patch))
	mux.HandleFunc("POST /api/requests/{id}/confirm", operatorAuth.Require(requestHandlers.Confirm))
	mux.HandleFunc("POST /api/requests/{id}/clarify", operatorAuth.Require(requestHandlers.Clarify))
	mux.HandleFunc("GET /api/requests/{id}/events", operatorAuth.RequireViewer(requestHandlers.Events))

	vendorHandlers := httpserver.NewVendorHandlers(vendorRepo, supplyRepo, eventWriter)
	// Phase 4C correction: vendors receive buyers' requests automatically, so
	// creating or activating one is an authenticated control-plane action.
	mux.HandleFunc("POST /api/vendors", operatorAuth.Require(vendorHandlers.Create))
	mux.HandleFunc("GET /api/vendors", operatorAuth.RequireViewer(vendorHandlers.List))
	mux.HandleFunc("GET /api/vendors/{id}", operatorAuth.RequireViewer(vendorHandlers.Get))
	mux.HandleFunc("PATCH /api/vendors/{id}", operatorAuth.Require(vendorHandlers.Patch))
	mux.HandleFunc("GET /api/vendors/{id}/supply", operatorAuth.RequireViewer(vendorHandlers.Supply))

	routingHandlers := httpserver.NewRoutingHandlers(routingRepo, routingService)
	mux.HandleFunc("GET /api/requests/{id}/matches", operatorAuth.RequireViewer(routingHandlers.Matches))
	mux.HandleFunc("POST /api/requests/{id}/route", operatorAuth.Require(routingHandlers.Route))
	mux.HandleFunc("GET /api/requests/{id}/vendor-requests", operatorAuth.RequireViewer(routingHandlers.VendorRequestsForBuyerRequest))
	mux.HandleFunc("GET /api/vendor-responses/unmatched", operatorAuth.RequireViewer(routingHandlers.Unmatched))
	mux.HandleFunc("GET /api/vendor-responses/{id}", operatorAuth.RequireViewer(routingHandlers.GetVendorResponse))
	mux.HandleFunc("PATCH /api/vendor-responses/{id}", operatorAuth.Require(routingHandlers.PatchVendorResponse))
	mux.HandleFunc("POST /api/vendor-responses/{id}/associate", operatorAuth.Require(routingHandlers.AssociateResponse))
	mux.HandleFunc("GET /api/vendors/{id}/vendor-requests", operatorAuth.RequireViewer(routingHandlers.VendorRequestsForVendor))

	// Phase 4A Step 4: listing writes and media previews are operator-only.
	supplyListingHandlers := httpserver.NewSupplyListingHandlers(listingRepo, listingService, listingRepo, mediaStore, operatorAuth, logger)
	mux.HandleFunc("POST /api/supply-listings", operatorAuth.Require(supplyListingHandlers.CreateFromSignal))
	// Search can list drafts and rejected listings: operator-only.
	mux.HandleFunc("GET /api/supply-listings", operatorAuth.RequireViewer(supplyListingHandlers.Search))
	// The detail carries the vendor's raw messages (evidence): operator-only.
	mux.HandleFunc("GET /api/supply-listings/{id}", operatorAuth.RequireViewer(supplyListingHandlers.Get))
	mux.HandleFunc("PATCH /api/supply-listings/{id}", operatorAuth.Require(supplyListingHandlers.Patch))
	mux.HandleFunc("POST /api/supply-listings/{id}/publish", operatorAuth.Require(supplyListingHandlers.PublishListing))
	// Legacy alias kept for existing callers: the same publication gate.
	mux.HandleFunc("POST /api/supply-listings/{id}/activate", operatorAuth.Require(supplyListingHandlers.PublishListing))
	mux.HandleFunc("POST /api/supply-listings/{id}/reject", operatorAuth.Require(supplyListingHandlers.Reject))
	mux.HandleFunc("POST /api/supply-listings/{id}/verify", operatorAuth.Require(supplyListingHandlers.Verify))
	mux.HandleFunc("GET /api/supply-listings/{id}/media/{asset}", operatorAuth.RequireViewer(supplyListingHandlers.Media))
	mux.HandleFunc("GET /api/vendors/{id}/supply-listings", operatorAuth.RequireViewer(supplyListingHandlers.ListByVendor))

	offerHandlers := httpserver.NewOfferHandlers(offerRepo, requestRepo, txService, txRepo, offerDelivery)
	mux.HandleFunc("GET /api/requests/{id}/offers", operatorAuth.RequireViewer(offerHandlers.List)) // read-only since Phase 3.1
	mux.HandleFunc("GET /api/requests/{id}/offers/preview", operatorAuth.RequireViewer(offerHandlers.BuyerMessagePreview))
	mux.HandleFunc("POST /api/requests/{id}/offers/deliver", operatorAuth.Require(offerHandlers.Deliver))
	mux.HandleFunc("POST /api/requests/{id}/select", operatorAuth.Require(offerHandlers.Select))
	mux.HandleFunc("GET /api/requests/{id}/selection", operatorAuth.RequireViewer(offerHandlers.Selection))

	// §23A: read-only core funnel, counted from system-of-record tables.
	funnelHandlers := httpserver.NewFunnelHandlers(postgres.NewFunnelRepository(pool))
	funnelHandlers.SetListingFreshness(supplyPolicy.Config().FreshnessWindow) // §K.2 stale rate uses the canonical freshness [PARAM]
	mux.HandleFunc("GET /api/metrics/funnel", operatorAuth.RequireViewer(funnelHandlers.Get))

	// Phase 4B: every discovery route is operator-only; there is no consumer endpoint.
	discoveryHandlers := httpserver.NewDiscoveryHandlers(distributionService, discoveryService, logger)
	mux.HandleFunc("GET /api/discovery/settings", operatorAuth.RequireViewer(discoveryHandlers.Settings))
	mux.HandleFunc("GET /api/discovery/candidates", operatorAuth.RequireViewer(discoveryHandlers.Candidates))
	mux.HandleFunc("GET /api/discovery/preferences", operatorAuth.RequireViewer(discoveryHandlers.Preferences))
	mux.HandleFunc("POST /api/discovery/preferences", operatorAuth.Require(discoveryHandlers.SetPreference))
	mux.HandleFunc("POST /api/discovery/consent/invite", operatorAuth.Require(discoveryHandlers.Invite))
	mux.HandleFunc("POST /api/discovery/preview", operatorAuth.Require(discoveryHandlers.Preview))
	mux.HandleFunc("POST /api/discovery/batches", operatorAuth.Require(discoveryHandlers.CreateBatch))
	mux.HandleFunc("GET /api/discovery/batches", operatorAuth.RequireViewer(discoveryHandlers.Batches))
	mux.HandleFunc("GET /api/discovery/batches/{id}", operatorAuth.RequireViewer(discoveryHandlers.Batch))
	mux.HandleFunc("POST /api/discovery/batches/{id}/send", operatorAuth.Require(discoveryHandlers.Send))
	mux.HandleFunc("GET /api/discovery/deliveries/{id}", operatorAuth.RequireViewer(discoveryHandlers.Delivery))
	mux.HandleFunc("GET /api/discovery/funnel", operatorAuth.RequireViewer(discoveryHandlers.Funnel))

	autonomyHandlers := httpserver.NewAutonomyHandlers(cfg.AutonomyFlags, cfg.AutonomyParams, autonomyRepo, autonomyRepo, eventWriter, logger)
	autonomyHandlers.SetReadvancer(engine)
	autonomyHandlers.SetControl(engine, messageTrail{workflowRepo, autonomyRepo})
	autonomyHandlers.SetGlobalControl(engine, autonomyRepo)
	mux.HandleFunc("GET /api/autonomy/settings", operatorAuth.RequireViewer(autonomyHandlers.Settings))
	mux.HandleFunc("GET /api/autonomy/metrics", operatorAuth.RequireViewer(autonomyHandlers.Metrics))
	mux.HandleFunc("GET /api/autonomy/exceptions", operatorAuth.RequireViewer(autonomyHandlers.Exceptions))
	mux.HandleFunc("POST /api/autonomy/exceptions/{id}/resolve", operatorAuth.Require(autonomyHandlers.ResolveException))
	mux.HandleFunc("GET /api/requests/{id}/autonomy", operatorAuth.RequireViewer(autonomyHandlers.Request))
	mux.HandleFunc("POST /api/requests/{id}/pause", operatorAuth.Require(autonomyHandlers.Pause))
	mux.HandleFunc("POST /api/requests/{id}/resume", operatorAuth.Require(autonomyHandlers.Resume))
	// Pilot batch item 4: pause / resume ALL automation (incident switch).
	mux.HandleFunc("GET /api/autonomy/pause", operatorAuth.RequireViewer(autonomyHandlers.GlobalPauseStatus))
	// Pilot batch item 5: deals (read-only).
	// Pilot batch item 8: monetization scaffold, everything off. Only the
	// "none" payment provider exists; the ledger is written to by no one yet.
	if _, err := payments.New(cfg.Monetization.Provider); err != nil {
		logger.Error("payments", "error", err)
		os.Exit(1)
	}
	ledgerRepo := postgres.NewLedgerRepository(pool)
	logger.Info("monetization", "stage", string(cfg.Monetization.Stage), "provider", string(cfg.Monetization.Provider),
		"lines_on", len(cfg.Monetization.Enabled))
	monetizationHandlers := httpserver.NewMonetizationHandlers(cfg.Monetization, ledgerRepo, logger)
	mux.HandleFunc("GET /api/monetization", operatorAuth.RequireViewer(monetizationHandlers.Get))

	dealHandlers := httpserver.NewDealHandlers(dealService, logger)
	mux.HandleFunc("GET /api/deals/summary", operatorAuth.RequireViewer(dealHandlers.Summary))
	mux.HandleFunc("GET /api/deals", operatorAuth.RequireViewer(dealHandlers.List))
	mux.HandleFunc("POST /api/autonomy/pause", operatorAuth.Require(autonomyHandlers.PauseAll))
	mux.HandleFunc("POST /api/autonomy/resume", operatorAuth.Require(autonomyHandlers.ResumeAll))

	transactionHandlers := httpserver.NewTransactionHandlers(txRepo)
	mux.HandleFunc("GET /api/transactions/{id}", operatorAuth.RequireViewer(transactionHandlers.Get))

	// Reuse-audit R1: control-plane read views (event timeline, job queue).
	controlPlaneRepo := postgres.NewControlPlaneRepository(pool)
	requestHandlers.SetPager(struct {
		*postgres.RequestRepository
		*postgres.ControlPlaneRepository
	}{requestRepo, controlPlaneRepo})
	controlPlaneHandlers := httpserver.NewControlPlaneHandlers(controlPlaneRepo)
	mux.HandleFunc("GET /api/events", operatorAuth.RequireViewer(controlPlaneHandlers.Events))
	mux.HandleFunc("GET /api/events/types", operatorAuth.RequireViewer(controlPlaneHandlers.EventTypes))
	mux.HandleFunc("GET /api/jobs", operatorAuth.RequireViewer(controlPlaneHandlers.Jobs))
	mux.HandleFunc("GET /api/jobs/summary", operatorAuth.RequireViewer(controlPlaneHandlers.JobSummary))

	handler := httpserver.RequestID(httpserver.CORS(cfg.CORSAllowedOrigins)(errortrack.Recoverer(logger)(mux)))

	srv := &http.Server{
		Addr:         ":" + cfg.AppPort,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// --- Run ---

	go func() {
		logger.Info("server starting", "port", cfg.AppPort, "env", cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	// In-flight durable jobs finish their bookkeeping; anything cut short
	// stays RUNNING and is reclaimed after JOBS_LEASE by the next process.
	select {
	case <-workerDone:
	case <-shutdownCtx.Done():
		logger.Warn("durable worker did not stop before the shutdown deadline")
	}

	logger.Info("server shut down cleanly")
}

// listingPublisher adapts the curation service and the trust query for
// trusted-vendor auto-publishing.
type listingPublisher struct {
	*supply.ListingService
	trust *postgres.WorkflowRepository
}

func (p listingPublisher) VendorTrust(ctx context.Context, vendorID uuid.UUID) (workflow.VendorTrust, error) {
	return p.trust.VendorTrust(ctx, vendorID)
}

// messageTrail joins the request's message trail (workflow repository) with
// its holds and queued notices (autonomy repository) for the operator view.
type messageTrail struct {
	*postgres.WorkflowRepository
	autonomy *postgres.AutonomyRepository
}

func (t messageTrail) Holds(ctx context.Context, id uuid.UUID) ([]*autonomy.Hold, error) {
	return t.autonomy.Holds(ctx, id)
}

func (t messageTrail) NoticesForRequest(ctx context.Context, id uuid.UUID) ([]*autonomy.Notice, error) {
	return t.autonomy.NoticesForRequest(ctx, id)
}

func healthcheck() int {
	port := os.Getenv("APP_PORT")
	if port == "" {
		port = "8080"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
