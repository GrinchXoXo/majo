package conversation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type fakeIDSender struct {
	id  string
	err error
}

func (f fakeIDSender) SendTextWithID(ctx context.Context, to, body string) (string, error) {
	return f.id, f.err
}

type memRepo struct {
	convID uuid.UUID
	saved  []*Message
}

func (m *memRepo) FindOrCreateByExternalID(ctx context.Context, ch, ext string) (*Conversation, error) {
	return &Conversation{ID: m.convID, Channel: ch, ExternalID: ext}, nil
}
func (m *memRepo) FindByID(ctx context.Context, id uuid.UUID) (*Conversation, error) { return nil, nil }
func (m *memRepo) SaveMessage(ctx context.Context, msg *Message) (*Message, error) {
	m.saved = append(m.saved, msg)
	return msg, nil
}
func (m *memRepo) MessageExists(ctx context.Context, id uuid.UUID, ext string) (bool, error) {
	return false, nil
}
func (m *memRepo) LinkBuyer(ctx context.Context, c, b uuid.UUID) error { return nil }

func TestRecordingSender_PersistsOutboundWithIDAndPurpose(t *testing.T) {
	repo := &memRepo{convID: uuid.New()}
	rs := NewRecordingSender(fakeIDSender{id: "wamid.ABC"}, repo, "whatsapp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	vrID := uuid.New()

	ctx := WithOutboundPurpose(context.Background(), OutboundPurpose{
		Kind: "vendor_request", EntityType: "vendor_request", EntityID: &vrID,
		Extra: map[string]any{"reference": "MM-123456"},
	})
	if err := rs.SendText(ctx, "2348000000001", "Majo Request #MM-123456"); err != nil {
		t.Fatal(err)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("expected 1 persisted outbound message, got %d", len(repo.saved))
	}
	m := repo.saved[0]
	if m.Direction != DirectionOutbound || m.ExternalMessageID != "wamid.ABC" || m.ConversationID != repo.convID {
		t.Fatalf("unexpected message %+v", m)
	}
	if m.Metadata["kind"] != "vendor_request" || m.Metadata["entity_type"] != "vendor_request" ||
		m.Metadata["entity_id"] != vrID.String() || m.Metadata["reference"] != "MM-123456" {
		t.Fatalf("purpose not recorded: %+v", m.Metadata)
	}
}

func TestRecordingSender_FailedSendIsNotRecordedAndErrorPropagates(t *testing.T) {
	repo := &memRepo{convID: uuid.New()}
	rs := NewRecordingSender(fakeIDSender{err: errors.New("131047")}, repo, "whatsapp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := rs.SendText(context.Background(), "x", "y"); err == nil {
		t.Fatal("send error must propagate unchanged")
	}
	if len(repo.saved) != 0 {
		t.Fatal("a message that was never sent must not be recorded as sent")
	}
}
