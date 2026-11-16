package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

func TestRegistrationSelectsProxyWorkerAndExplicitAddress(t *testing.T) {
	office := strings.Repeat("a", 64)
	workers := []model.ProxyWorker{
		{ID: office, Name: "office-bridge", Online: true, Endpoints: []model.ProxyEndpointStatus{
			{URL: "http://10.0.0.5:3128", Interface: "office", State: model.ProxyReady, CheckedAt: time.Now()},
			{URL: "http://[fd00::5]:3128", Interface: "vpn", State: model.ProxyReady, CheckedAt: time.Now()},
		}},
		{ID: strings.Repeat("b", 64), Name: "stale-bridge", Online: true, Endpoints: []model.ProxyEndpointStatus{{URL: "http://10.1.0.5:3128", State: model.ProxyStale}}},
	}
	var got model.GatewayProxy
	created := 0
	m := view{ctx: context.Background(), width: 100, height: 40,
		wizard: &registration{id: 1, name: "render-01", step: "context"},
		options: Options{
			Invite: func(_ context.Context, name, os, mode string, proxy model.GatewayProxy) (enrollment.Link, error) {
				created++
				got = proxy
				return enrollment.Link{Invitation: enrollment.Invitation{GatewayProxy: &proxy}, Command: "curl"}, nil
			},
			ProxyWorkers: func(context.Context) ([]model.ProxyWorker, error) { return workers, nil },
		},
	}
	apply := func(msg tea.Msg) tea.Cmd {
		next, cmd := m.updateRegistration(msg)
		m = next.(view)
		return cmd
	}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	if cmd := apply(enter); cmd != nil || m.wizard.step != "connection" {
		t.Fatalf("context did not lead to connection selection: %s", m.wizard.step)
	}
	text := ansi.Strip(m.registrationView().Content)
	if !strings.Contains(text, "Direct") || !strings.Contains(text, "Through worker") {
		t.Fatal("connection choices missing")
	}
	apply(tea.KeyPressMsg{Code: '2', Text: "2"})
	cmd := apply(enter)
	if cmd == nil || m.wizard.step != "loading-workers" {
		t.Fatal("worker list was not loaded")
	}
	apply(cmd())
	if m.wizard.step != "worker" {
		t.Fatalf("step %s", m.wizard.step)
	}
	text = ansi.Strip(m.registrationView().Content)
	if !strings.Contains(text, "office-bridge") || !strings.Contains(text, "10.0.0.5:3128") || !strings.Contains(text, "stale-bridge — not ready") || !strings.Contains(text, "must be able to reach") {
		t.Fatalf("worker view: %s", text)
	}
	// Two advertised addresses: selection must be explicit.
	if cmd = apply(enter); cmd != nil || m.wizard.step != "address" {
		t.Fatal("ambiguous worker skipped address selection")
	}
	apply(tea.KeyPressMsg{Code: tea.KeyDown})
	cmd = apply(enter)
	if cmd == nil {
		t.Fatal("address selection did not create the invitation")
	}
	apply(cmd())
	if created != 1 || got.Worker != office || got.URL != "http://[fd00::5]:3128" {
		t.Fatalf("selection %+v", got)
	}
}

func TestRegistrationDirectConnectionSendsNoProxy(t *testing.T) {
	var got model.GatewayProxy
	m := view{ctx: context.Background(), width: 80, height: 24,
		wizard: &registration{id: 1, name: "render-01", step: "context"},
		options: Options{
			Invite: func(_ context.Context, _, _, _ string, proxy model.GatewayProxy) (enrollment.Link, error) {
				got = proxy
				return enrollment.Link{}, nil
			},
			ProxyWorkers: func(context.Context) ([]model.ProxyWorker, error) { t.Fatal("direct loaded workers"); return nil, nil },
		},
	}
	next, _ := m.updateRegistration(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(view)
	next, cmd := m.updateRegistration(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next.(view).wizard.step != "creating" || cmd == nil {
		t.Fatal("direct connection did not create")
	}
	cmd()
	if got != (model.GatewayProxy{}) {
		t.Fatalf("direct invitation carried %+v", got)
	}
}
