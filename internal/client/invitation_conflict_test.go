package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
)

func TestPendingInvitationConflictRequiresExactGatewayRejection(t *testing.T) {
	for _, tc := range []struct {
		status  int
		text    string
		pending bool
	}{
		{409, enrollment.ErrPendingInvitation.Error(), true},
		{500, enrollment.ErrPendingInvitation.Error(), false},
		{409, "name has an existing enrollment", false},
		{409, "name is already registered", false},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, tc.text, tc.status)
		}))
		err := (Admin{URL: s.URL, Key: "key"}).JSON(context.Background(), "PATCH", "/v1/fleet/nodes/id", map[string]string{"name": "reserved"}, nil)
		s.Close()
		if errors.Is(err, enrollment.ErrPendingInvitation) != tc.pending {
			t.Fatal("misclassified gateway rejection", tc, err)
		}
	}
}
