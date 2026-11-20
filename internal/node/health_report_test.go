package node

import (
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestHealthReportSuppressesOnlyRecentIdenticalReports(t *testing.T) {
	var r healthReport
	now := time.Unix(1000, 0)
	data := []byte(`{"activeCount":0}`)
	if r.unchanged(data, 1, now) {
		t.Fatal("first report was suppressed")
	}
	r.sent(data, 1, now)
	if !r.unchanged([]byte(`{"activeCount":0}`), 1, now.Add(model.NodeHealthInterval)) {
		t.Fatal("identical report was resent before the unchanged interval")
	}
	if r.unchanged([]byte(`{"activeCount":1}`), 1, now.Add(model.NodeHealthInterval)) {
		t.Fatal("changed health was suppressed")
	}
	if r.unchanged(data, 2, now.Add(model.NodeHealthInterval)) {
		t.Fatal("a new gateway connection did not receive health")
	}
	// Ticks drift; the periodic resend must not slip a whole tick late.
	if r.unchanged(data, 1, now.Add(model.UnchangedTelemetryInterval-model.NodeHealthInterval/2)) {
		t.Fatal("unchanged health was not resent at its interval")
	}
}
