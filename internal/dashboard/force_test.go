package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

func TestForceRenameRequiresExplicitConfirmation(t *testing.T) {
	for _, requestErr := range []error{enrollment.ErrPendingInvitation, errors.New("request timed out")} {
		m := view{ctx: context.Background(), width: 90, height: 24, busy: true,
			manager: &machineManager{id: 1, phase: "pending", action: "rename", name: "reserved", nodes: []model.NodeActivitySnapshot{{ID: "id", Name: "original"}}}}
		calls := 0
		m.options.ForceRename = func(_ context.Context, id, name string) error {
			calls++
			if id != "id" || name != "reserved" {
				t.Fatal("force changed target", id, name)
			}
			return errors.New("uncertain force result")
		}
		apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.updateManager(msg); m = next.(view); return cmd }
		key := func(code rune, text string) tea.Cmd { return apply(tea.KeyPressMsg{Code: code, Text: text}) }
		apply(machineActionResult{1, fmt.Errorf("gateway rejected rename: %w", requestErr)})
		if calls != 0 {
			t.Fatal("force retried automatically")
		}
		if !errors.Is(requestErr, enrollment.ErrPendingInvitation) {
			if m.manager.phase != "error" || strings.Contains(m.managerView().Content, "force rename") || key('f', "f") != nil {
				t.Fatal("uncertain error offered force")
			}
			continue
		}
		if m.manager.phase != "conflict" || !strings.Contains(m.managerView().Content, "f force rename") {
			t.Fatal("missing force action")
		}
		key('f', "f")
		if calls != 0 || m.manager.phase != "force-confirm" || !strings.Contains(m.managerView().Content, "URL will stop working immediately") {
			t.Fatal("force skipped confirmation")
		}
		key(tea.KeyEscape, "")
		if calls != 0 || m.manager.force || m.manager.phase != "actions" {
			t.Fatal("cancel submitted force")
		}
		m.manager.phase = "conflict"
		key('f', "f")
		cmd := key(tea.KeyEnter, "")
		if cmd == nil || key(tea.KeyEnter, "") != nil {
			t.Fatal("force submitted twice")
		}
		apply(cmd())
		if calls != 1 || m.manager.phase != "error" || key('f', "f") != nil {
			t.Fatal("uncertain force was replayed")
		}
	}
}

func TestForceAddRequiresExplicitConfirmation(t *testing.T) {
	for _, requestErr := range []error{enrollment.ErrPendingInvitation, errors.New("request timed out")} {
		m := view{ctx: context.Background(), width: 90, height: 24,
			wizard: &registration{id: 1, step: "creating", name: "reserved", platform: 2, context: 1}}
		calls := 0
		m.options.ForceInvite = func(_ context.Context, name, platform, mode string) (enrollment.Link, error) {
			calls++
			if name != "reserved" || platform != "linux" || mode != "system" {
				t.Fatal("force changed invitation settings", name, platform, mode)
			}
			return enrollment.Link{Command: "replacement"}, nil
		}
		apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.updateRegistration(msg); m = next.(view); return cmd }
		key := func(code rune, text string) tea.Cmd { return apply(tea.KeyPressMsg{Code: code, Text: text}) }
		apply(invitationResult{id: 1, err: requestErr})
		if !errors.Is(requestErr, enrollment.ErrPendingInvitation) {
			if m.wizard.step != "failed" || key('f', "f") != nil || calls != 0 {
				t.Fatal("uncertain invitation request offered force")
			}
			continue
		}
		if m.wizard.step != "conflict" || !strings.Contains(m.registrationView().Content, "f force add") {
			t.Fatal("missing force add")
		}
		key('f', "f")
		if calls != 0 || m.wizard.step != "force-confirm" || !strings.Contains(m.registrationView().Content, "URL will stop working immediately") {
			t.Fatal("force skipped confirmation")
		}
		key(tea.KeyEscape, "")
		if calls != 0 || m.wizard.force || m.wizard.step != "context" {
			t.Fatal("cancel submitted force add")
		}
		m.wizard.step = "conflict"
		key('f', "f")
		cmd := key(tea.KeyEnter, "")
		if cmd == nil || key(tea.KeyEnter, "") != nil {
			t.Fatal("force add submitted twice")
		}
		apply(cmd())
		if calls != 1 || m.wizard.step != "copying" || m.wizard.link.Command != "replacement" {
			t.Fatal("replacement invitation not copied")
		}
	}
}
