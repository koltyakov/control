package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

func TestMachineManagementTargetsSelectedIdentityOnce(t *testing.T) {
	calls := 0
	m := view{ctx: context.Background(), width: 90, height: 24, busy: true, snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{ID: "first", Name: "first"}, {ID: "second", Name: "second"}}}, options: Options{Manage: func(_ context.Context, id, action string) error {
		calls++
		if id != "second" || action != "unregister" {
			t.Fatalf("wrong target %s %s", id, action)
		}
		return nil
	}}}
	apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.Update(msg); m = next.(view); return cmd }
	key := func(text string) tea.Cmd { return apply(tea.KeyPressMsg{Code: rune(text[0]), Text: text}) }
	key("m")
	key("j")
	apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	key("u")
	if m.manager.phase != "confirm" || calls != 0 {
		t.Fatal("unregister ran before selection confirmation")
	}
	if text := m.View().Content; !strings.Contains(text, "offline registration will be removed immediately") || !strings.Contains(text, "Lifecycle-capable nodes stop") {
		t.Fatal("offline confirmation does not explain removal", text)
	}
	for _, width := range []int{4, 30, 90} {
		m.width = width
		for _, line := range strings.Split(m.View().Content, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatal("manager overflow")
			}
		}
	}
	cmd := apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next := apply(tea.KeyPressMsg{Code: tea.KeyEnter}); next != nil {
		t.Fatal("duplicate request")
	}
	apply(cmd())
	if calls != 1 || m.manager.phase != "done" {
		t.Fatal("request not completed")
	}
	apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.manager != nil {
		t.Fatal("manager did not close")
	}
}

func TestNodeVersionAndDisabledStatus(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{{Name: "worker", Online: true, Status: "summary", Disabled: true, Software: buildinfo.Info{Version: "v1.2.3"}}}}
	text := render(snapshot, now, renderOptions{width: 140})
	if !strings.Contains(text, "v1.2.3") || !strings.Contains(text, "disabled") {
		t.Fatal(text)
	}
	snapshot.Nodes[0].ControlPending = true
	if text = render(snapshot, now, renderOptions{width: 140}); !strings.Contains(text, "disabling") {
		t.Fatal(text)
	}
}

func TestMachineRenameInputAndSingleSubmission(t *testing.T) {
	for _, requestErr := range []error{nil, errors.New("name already registered")} {
		m := view{ctx: context.Background(), width: 90, height: 24, busy: true,
			snapshot: model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{{ID: "stable-id", Name: "hostname.local"}}}}
		calls := 0
		m.options.Manage = func(context.Context, string, string) error { t.Fatal("used lifecycle action for rename"); return nil }
		m.options.Rename = func(_ context.Context, id, name string) error {
			calls++
			if id != "stable-id" || name != "render-01" {
				t.Fatalf("wrong rename target %q %q", id, name)
			}
			return requestErr
		}
		apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.Update(msg); m = next.(view); return cmd }
		key := func(code rune, text string) tea.Cmd { return apply(tea.KeyPressMsg{Code: code, Text: text}) }
		key('m', "m")
		key(tea.KeyEnter, "")
		if !strings.Contains(m.View().Content, "r rename machine") {
			t.Fatal("missing rename action")
		}
		key('r', "r")
		if m.manager.phase != "rename" || m.manager.name != "hostname.local" {
			t.Fatal("rename did not prefill existing name")
		}
		key(tea.KeyEscape, "")
		if m.manager.phase != "actions" || calls != 0 {
			t.Fatal("cancel submitted rename")
		}
		key('r', "r")
		apply(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
		if cmd := key(tea.KeyEnter, ""); cmd != nil || m.manager.message == "" {
			t.Fatal("empty alias accepted")
		}
		apply(tea.PasteMsg{Content: "render-02"})
		key(tea.KeyBackspace, "")
		key('1', "1")
		cmd := key(tea.KeyEnter, "")
		if cmd == nil || calls != 0 || key(tea.KeyEnter, "") != nil {
			t.Fatal("rename not submitted exactly once")
		}
		apply(cmd())
		if calls != 1 {
			t.Fatal("unexpected request count", calls)
		}
		wantPhase, wantText := "done", "Renamed hostname.local to render-01."
		if requestErr != nil {
			wantPhase, wantText = "error", requestErr.Error()
		}
		if m.manager.phase != wantPhase || !strings.Contains(m.View().Content, wantText) {
			t.Fatal("incorrect rename result", m.View().Content)
		}
	}
}
