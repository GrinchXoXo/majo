package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/workflow"
)

type trustAll struct{}

func (trustAll) PublishAs(context.Context, uuid.UUID, *time.Time, *uuid.UUID, event.ActorType) (*supply.Listing, error) {
	return nil, nil
}
func (trustAll) VendorTrust(context.Context, uuid.UUID) (workflow.VendorTrust, error) {
	return workflow.VendorTrust{Decided: 20, ApprovedUnchanged: 20}, nil
}

// 01a audit: a trusted vendor's draft listing for a restricted item is never
// auto-published; an ordinary one is queued for auto-publishing.
func TestIntegration_AutoPublishRefusesRestrictedItems(t *testing.T) {
	env := newHardeningEnv(t, "2348070000081", "SENT", time.Hour, func(c *workflow.Config) { c.Flags.AutoPublishTrustedVendorLists = true })
	env.engine.SetListingPublisher(trustAll{})
	ctx := context.Background()
	str := func(s string) *string { return &s }

	bad := &supply.Listing{ID: uuid.New(), VendorID: env.s.vendor, Product: str("Codeine syrup"), Category: str("health")}
	env.engine.DraftReady(ctx, bad)
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND outcome = 'REJECTED' AND reason = 'restricted_item'`, bad.ID); n != 1 {
		t.Fatalf("restricted draft refused with a recorded decision, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = $1 AND payload->>'supply_listing_id' = $2`, workflow.JobListingPublish, bad.ID.String()); n != 0 {
		t.Fatalf("no auto-publish job for a restricted item, got %d", n)
	}

	ok := &supply.Listing{ID: uuid.New(), VendorID: env.s.vendor, Product: str("iphone 13"), Category: str("electronics")}
	env.engine.DraftReady(ctx, ok)
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = $1 AND payload->>'supply_listing_id' = $2`, workflow.JobListingPublish, ok.ID.String()); n != 1 {
		t.Fatalf("ordinary draft queued, got %d", n)
	}
}
