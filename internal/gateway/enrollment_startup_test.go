package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
)

func TestInvitationStartupContext(t *testing.T) {
	g, server := installationFixture(t)
	admin := testAdmin{URL: server.URL, Key: superKey}
	for _, mode := range []string{"user", "system"} {
		var link enrollment.Link
		q := enrollment.Request{Name: "worker-" + mode, OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: mode}
		if err := admin.JSON(context.Background(), "POST", "/v1/fleet/installations", q, &link); err != nil {
			t.Fatal(err)
		}
		if link.ServiceMode != mode || strings.Contains(link.Command, "sudo bash") != (mode == "system") {
			t.Fatalf("wrong startup context: %+v", link)
		}
		response, err := http.Get(link.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || !strings.Contains(string(body), "service_mode='"+mode+"'") {
			t.Fatalf("script lost startup context: %s %v", body, err)
		}
		g.mu.Lock()
		persisted := map[string]installation{}
		err = readRecords(g.db, "invitations", "hash", persisted)
		g.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, invitation := range persisted {
			if invitation.ID == link.ID && invitation.ServiceMode == mode {
				found = true
			}
		}
		if !found {
			t.Fatal("startup context was not persisted")
		}
	}
	q := enrollment.Request{Name: "invalid", OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "unexpected"}
	if err := admin.JSON(context.Background(), "POST", "/v1/fleet/installations", q, nil); err == nil {
		t.Fatal("invalid startup context accepted")
	}
}
