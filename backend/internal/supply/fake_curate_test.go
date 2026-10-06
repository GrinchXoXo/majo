package supply

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Curate mirrors the Postgres implementation's semantics (decide on the
// current row; apply change + audit + event together; nothing on error)
// without the row lock, which the Postgres integration tests cover.
func (f *fakeListingRepo) Curate(ctx context.Context, id uuid.UUID, decide CurationDecision) (*Listing, error) {
	stored, ok := f.listings[id]
	if !ok {
		return nil, ErrListingNotFound
	}
	if f.beforeDecide != nil {
		hook := f.beforeDecide
		f.beforeDecide = nil
		hook(stored)
	}
	snapshot := *stored
	ch, err := decide(&snapshot)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		cp := *stored
		return &cp, nil
	}
	fl := ch.Fields
	set := func(dst **string, v *string) {
		if v != nil {
			c := *v
			*dst = &c
		}
	}
	set(&stored.Category, fl.Category)
	set(&stored.Product, fl.Product)
	if fl.Product != nil {
		n := NormalizeSearch(*fl.Product)
		stored.NormalizedProduct = &n
	}
	set(&stored.Variant, fl.Variant)
	set(&stored.Condition, fl.Condition)
	set(&stored.Location, fl.Location)
	set(&stored.Description, fl.Description)
	if fl.Price != nil {
		p := *fl.Price
		stored.Price = &p
	}
	if fl.Quantity != nil {
		q := *fl.Quantity
		stored.Quantity = &q
	}
	if fl.Currency != nil {
		stored.Currency = *fl.Currency
	}
	if fl.MediaReferences != nil {
		stored.MediaReferences = append([]MediaReference(nil), (*fl.MediaReferences)...)
	}
	now := time.Now().UTC()
	if ch.Publish {
		stored.Status = ListingActive
		if stored.PublishedAt == nil {
			stored.PublishedAt = &now
		}
	}
	if ch.Reject {
		stored.Status = ListingArchived
		stored.RejectedAt = &now
		stored.RejectionReason = ch.RejectReason
	}
	stored.UpdatedAt = now
	if ch.Audit != nil {
		f.curationAudits = append(f.curationAudits, ch.Audit)
	}
	if ch.Event != nil {
		f.curationEvents = append(f.curationEvents, ch.Event)
	}
	cp := *stored
	return &cp, nil
}
