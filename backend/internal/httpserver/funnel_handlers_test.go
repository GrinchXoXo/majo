package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/middleman/backend/internal/funnel"
)

type fakeFunnel struct{ supplyErr error }

func (f fakeFunnel) Funnel(ctx context.Context, since, until time.Time) ([]funnel.Stage, error) {
	return []funnel.Stage{{Key: "buyer_request_created", Count: 3}}, nil
}

func (f fakeFunnel) Supply(ctx context.Context, since, until time.Time, fresh time.Duration) ([]funnel.SupplyMetric, error) {
	if f.supplyErr != nil {
		return nil, f.supplyErr
	}
	of := int64(4)
	return []funnel.SupplyMetric{{Key: "valid_supply_rate", Kind: "rate", Value: 3, Of: &of}}, nil
}

func TestFunnelHandler_SupplyRowsAndFailureIsolation(t *testing.T) {
	for _, tc := range []struct {
		err       error
		wantRows  int
		wantError bool
	}{{nil, 1, false}, {errors.New("db"), 0, true}} {
		rec := httptest.NewRecorder()
		NewFunnelHandlers(fakeFunnel{supplyErr: tc.err}).Get(rec, httptest.NewRequest("GET", "/api/metrics/funnel", nil))
		var rep funnel.Report
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rep) != nil {
			t.Fatalf("status %d", rec.Code)
		}
		if len(rep.Stages) != 1 || len(rep.Supply) != tc.wantRows || (rep.SupplyError != "") != tc.wantError {
			t.Fatalf("supply err=%v: stages=%d supply=%d supply_error=%q", tc.err, len(rep.Stages), len(rep.Supply), rep.SupplyError)
		}
	}
}
