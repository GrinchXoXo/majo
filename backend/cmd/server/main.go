// Middleman backend entrypoint.
//
// This is the composition root (PRD 0 FINAL §19): the only place where
// infrastructure implementations are instantiated and wired into domain
// services and HTTP handlers. Domain packages never construct their own
// infrastructure dependencies.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/infrastructure/postgres"
	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/config"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/logging"
	"github.com/middleman/backend/internal/marketplace"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/transaction"
)

func main() {
	logger := logging.New("middleman-backend")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading configuration", "error", err)
		os.Exit(1)
	}

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
		requestRepo, convRepo, aiProvider, eventWriter, auditWriter, whatsappProvider, logger,
	)

	routingService := routing.NewService(
		vendorRepo, supplyRepo, routingRepo, requestRepo, aiProvider, eventWriter, auditWriter, whatsappProvider, logger,
	)

	listingService := supply.NewListingService(supplyRepo, listingRepo, eventWriter, auditWriter, logger)
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
		requestRepo, convRepo, offerService, offerRepo, offerRepo, routingRepo, txRepo, whatsappProvider, eventWriter, logger,
	)
	routingService.SetResponseListener(offerDelivery)
	contextResolver := marketplace.NewContextResolver(offerRepo, offerRepo)
	buyerInbound := marketplace.NewBuyerInbound(
		contextResolver, requestService, requestRepo, txService, offerRepo, routingRepo, whatsappProvider, eventWriter, jobRunner, logger,
	)

	// --- HTTP ---

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpserver.HealthHandler)
	mux.HandleFunc("GET /ready", httpserver.ReadyHandler(pool, aiProvider))

	webhookHandler := httpserver.NewWhatsAppWebhookHandler(
		whatsappProvider, convRepo, buyerRepo, vendorRepo, eventWriter, buyerInbound, routingService, jobRunner,
		cfg.WhatsAppVerifyToken, cfg.AllowUnsignedWebhooks, logger,
	)
	mux.Handle("/webhooks/whatsapp", webhookHandler)

	requestHandlers := httpserver.NewRequestHandlers(requestRepo, requestService, eventWriter)
	mux.HandleFunc("GET /api/requests", requestHandlers.List)
	mux.HandleFunc("GET /api/requests/{id}", requestHandlers.Get)
	mux.HandleFunc("PATCH /api/requests/{id}", requestHandlers.Patch)
	mux.HandleFunc("POST /api/requests/{id}/confirm", requestHandlers.Confirm)
	mux.HandleFunc("POST /api/requests/{id}/clarify", requestHandlers.Clarify)
	mux.HandleFunc("GET /api/requests/{id}/events", requestHandlers.Events)

	vendorHandlers := httpserver.NewVendorHandlers(vendorRepo, supplyRepo, eventWriter)
	mux.HandleFunc("POST /api/vendors", vendorHandlers.Create)
	mux.HandleFunc("GET /api/vendors", vendorHandlers.List)
	mux.HandleFunc("GET /api/vendors/{id}", vendorHandlers.Get)
	mux.HandleFunc("PATCH /api/vendors/{id}", vendorHandlers.Patch)
	mux.HandleFunc("GET /api/vendors/{id}/supply", vendorHandlers.Supply)

	routingHandlers := httpserver.NewRoutingHandlers(routingRepo, routingService)
	mux.HandleFunc("GET /api/requests/{id}/matches", routingHandlers.Matches)
	mux.HandleFunc("POST /api/requests/{id}/route", routingHandlers.Route)
	mux.HandleFunc("GET /api/requests/{id}/vendor-requests", routingHandlers.VendorRequestsForBuyerRequest)
	mux.HandleFunc("GET /api/vendor-responses/unmatched", routingHandlers.Unmatched)
	mux.HandleFunc("GET /api/vendor-responses/{id}", routingHandlers.GetVendorResponse)
	mux.HandleFunc("PATCH /api/vendor-responses/{id}", routingHandlers.PatchVendorResponse)
	mux.HandleFunc("POST /api/vendor-responses/{id}/associate", routingHandlers.AssociateResponse)
	mux.HandleFunc("GET /api/vendors/{id}/vendor-requests", routingHandlers.VendorRequestsForVendor)

	supplyListingHandlers := httpserver.NewSupplyListingHandlers(listingRepo, listingService)
	mux.HandleFunc("POST /api/supply-listings", supplyListingHandlers.Publish)
	mux.HandleFunc("GET /api/supply-listings", supplyListingHandlers.Search)
	mux.HandleFunc("GET /api/supply-listings/{id}", supplyListingHandlers.Get)
	mux.HandleFunc("PATCH /api/supply-listings/{id}", supplyListingHandlers.Patch)
	mux.HandleFunc("POST /api/supply-listings/{id}/activate", supplyListingHandlers.Activate)
	mux.HandleFunc("GET /api/vendors/{id}/supply-listings", supplyListingHandlers.ListByVendor)

	offerHandlers := httpserver.NewOfferHandlers(offerRepo, requestRepo, txService, txRepo, offerDelivery)
	mux.HandleFunc("GET /api/requests/{id}/offers", offerHandlers.List) // read-only since Phase 3.1
	mux.HandleFunc("GET /api/requests/{id}/offers/preview", offerHandlers.BuyerMessagePreview)
	mux.HandleFunc("POST /api/requests/{id}/offers/deliver", offerHandlers.Deliver)
	mux.HandleFunc("POST /api/requests/{id}/select", offerHandlers.Select)
	mux.HandleFunc("GET /api/requests/{id}/selection", offerHandlers.Selection)

	transactionHandlers := httpserver.NewTransactionHandlers(txRepo)
	mux.HandleFunc("GET /api/transactions/{id}", transactionHandlers.Get)

	handler := httpserver.RequestID(httpserver.CORS(mux))

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

	logger.Info("server shut down cleanly")
}
