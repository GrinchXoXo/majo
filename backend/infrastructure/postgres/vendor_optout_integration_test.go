package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func textPayloadFrom(from, wamid, text string) string {
	return fmt.Sprintf(`{"entry":[{"changes":[{"value":{"contacts":[{"wa_id":%q,"profile":{"name":"Vendor"}}],"messages":[{"from":%q,"id":%q,"timestamp":"1700000000","type":"text","text":{"body":%q}}]}}]}]}`,
		from, from, wamid, text)
}

// Independent review: STOP goes through the real webhook for an ACTIVE
// vendor, and START from the (now INACTIVE) vendor reaches the vendor
// keyword handling instead of creating a buyer.
func TestIntegration_VendorStopStartThroughWebhook(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348123400077"
	id := s.vendor("Stop Start Gadgets", wa)

	s.post(textPayloadFrom(wa, "wamid.stop.1", "Stop please"))
	deadline := time.Now().Add(5 * time.Second)
	for s.count(`SELECT count(*) FROM vendors WHERE id = $1 AND status = 'INACTIVE'`, id) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("STOP deactivates the vendor")
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.post(textPayloadFrom(wa, "wamid.start.1", "START"))
	if n := s.count(`SELECT count(*) FROM events WHERE event_type = 'VENDOR_OPT_IN_REQUESTED' AND entity_id = $1`, id); n != 1 {
		t.Fatalf("START from an inactive vendor is recorded for the owner, got %d", n)
	}
	if n := s.count(`SELECT count(*) FROM buyers WHERE external_user_id = $1`, wa); n != 0 {
		t.Fatalf("an inactive vendor's START never makes them a buyer, got %d", n)
	}
	if n := s.count(`SELECT count(*) FROM vendors WHERE id = $1 AND status = 'INACTIVE'`, id); n != 1 {
		t.Fatal("START does not re-activate by itself")
	}
}

// Migration 0020 rewrites old hand-typed vendor numbers to the wa_id form.
func TestIntegration_VendorNumberMigration(t *testing.T) {
	db := testDB(t, "")
	// Apply migrations up to 0019, insert old-style rows, then apply 0020.
	if err := RunMigrationsUpTo(t, db, "0019_ledger.sql"); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"A", "0803 123 4567"}, {"B", "+234 (0) 805 000 0001"}, {"C", "8091112222"}, {"D", "2348031234567x"}, {"E", "2348099990000"}, {"F", "08099990000"}} {
		mustExec(t, db, `INSERT INTO vendors (business_name, whatsapp_external_id) VALUES ($1, $2)`, row[0], row[1])
	}
	if err := RunMigrationsUpTo(t, db, "0020_vendor_wa_id_normalize.sql"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "2348031234567", "B": "2348050000001", "C": "2348091112222", "D": "2348031234567x", "E": "2348099990000", "F": "08099990000"}
	for name, wa := range want {
		var got string
		if err := db.QueryRow(`SELECT whatsapp_external_id FROM vendors WHERE business_name = $1`, name).Scan(&got); err != nil || got != wa {
			t.Errorf("%s: %q (want %q) %v", name, got, wa, err)
		}
	}
}

// RunMigrationsUpTo applies the repository's migrations up to and
// including last (a filename), through a temporary directory.
func RunMigrationsUpTo(t *testing.T, db *sql.DB, last string) error {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(migrationsPath())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") || e.Name() > last {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migrationsPath(), e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	return RunMigrations(context.Background(), db, dir)
}
