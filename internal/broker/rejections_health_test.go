package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleHealth_ReportsDegradedRejectionLedger(t *testing.T) {
	b := &Broker{}
	rec := httptest.NewRecorder()
	b.HandleHealth(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"rejection_ledger_error":""`) {
		t.Fatalf("healthy body: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"queue_max_denials":0`) {
		t.Fatalf("queue_max_denials missing: %s", rec.Body)
	}
	b.MaxDenials = 2
	b.Rejections = degradedLedger("line 3 is not a ledger entry; fix or remove it and restart brokerd")
	rec = httptest.NewRecorder()
	b.HandleHealth(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"rejection_ledger_error":"line 3 is not a ledger entry`) {
		t.Fatalf("degraded body: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"queue_max_denials":2`) {
		t.Fatalf("queue_max_denials not reported: %s", rec.Body)
	}
}
