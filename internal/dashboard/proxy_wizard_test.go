package dashboard

import (
	"context"
	"errors"
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
		next, cmd := m.Update(msg)
		m = next.(view)
		return cmd
	}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	if cmd := apply(enter); cmd == nil || m.wizard.step != "loading-connections" {
		t.Fatalf("context did not check available connections: %s", m.wizard.step)
	} else {
		apply(cmd())
	}
	text := ansi.Strip(m.registrationView().Content)
	if !strings.Contains(text, "Direct") || !strings.Contains(text, "Through worker") {
		t.Fatal("connection choices missing")
	}
	apply(tea.KeyPressMsg{Code: '2', Text: "2"})
	cmd := apply(enter)
	if cmd != nil || m.wizard.step != "worker" {
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
			ProxyWorkers: func(context.Context) ([]model.ProxyWorker, error) {
				return []model.ProxyWorker{{ID: "bridge", Online: true, Endpoints: []model.ProxyEndpointStatus{{URL: "http://10.0.0.5:3128", State: model.ProxyReady}}}}, nil
			},
		},
	}
	next, load := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(view)
	if load == nil {
		t.Fatal("available connections were not checked")
	}
	next, _ = m.Update(load())
	m = next.(view)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next.(view).wizard.step != "creating" || cmd == nil {
		t.Fatal("direct connection did not create")
	}
	cmd()
	if got != (model.GatewayProxy{}) {
		t.Fatalf("direct invitation carried %+v", got)
	}
}

func TestRegistrationSkipsConnectionChoiceWithoutUsableProxy(t *testing.T) {
	for _, state := range []string{"none", "offline", "stale", "disabled"} {
		t.Run(state, func(t *testing.T) {
			var workers []model.ProxyWorker
			if state != "none" {
				workers = []model.ProxyWorker{{ID: "bridge", Online: state != "offline", Disabled: state == "disabled", Endpoints: []model.ProxyEndpointStatus{{URL: "http://10.0.0.5:3128", State: model.ProxyReady}}}}
				if state == "stale" {
					workers[0].Endpoints[0].State = model.ProxyStale
				}
			}
			created := 0
			m := view{ctx: context.Background(), wizard: &registration{id: 1, name: "child", step: "context"}, options: Options{
				ProxyWorkers: func(context.Context) ([]model.ProxyWorker, error) { return workers, nil },
				Invite: func(_ context.Context, _, _, _ string, proxy model.GatewayProxy) (enrollment.Link, error) {
					created++
					if proxy != (model.GatewayProxy{}) {
						t.Fatal("direct invitation carried a proxy")
					}
					return enrollment.Link{}, nil
				},
			}}
			next, load := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(view)
			if load == nil || created != 0 {
				t.Fatal("invitation was not deferred until discovery completed")
			}
			next, create := m.Update(load())
			if next.(view).wizard.step != "creating" || create == nil {
				t.Fatal("unavailable connection choices were shown")
			}
			create()
			if created != 1 {
				t.Fatal("direct invitation was not created exactly once")
			}
		})
	}
}

func TestRegistrationDoesNotCreateAfterProxyDiscoveryFailure(t *testing.T) {
	m := view{ctx: context.Background(), wizard: &registration{id: 1, step: "loading-connections"}, options: Options{
		Invite: func(context.Context, string, string, string, model.GatewayProxy) (enrollment.Link, error) {
			t.Fatal("discovery failure created an invitation")
			return enrollment.Link{}, nil
		},
	}}
	next, cmd := m.Update(proxyWorkersResult{id: 1, err: errors.New("unavailable")})
	if cmd != nil || next.(view).wizard.step != "context" || next.(view).wizard.err == "" {
		t.Fatal("discovery failure was hidden")
	}
}
