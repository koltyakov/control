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
)

func TestRegistrationWizardCopiesAndRetriesWithoutCreatingAnotherInvitation(t *testing.T) {
	created, copied := 0, 0
	m := view{ctx: context.Background(), width: 80, height: 24, options: Options{
		Invite: func(_ context.Context, name, platform string) (enrollment.Link, error) {
			created++
			if name != "render-01" || platform != "windows" {
				t.Fatalf("wrong registration: %s %s", name, platform)
			}
			return enrollment.Link{Command: "installation-command", Invitation: enrollment.Invitation{ExpiresAt: time.Now().Add(15 * time.Minute)}}, nil
		},
		Copy: func(_ context.Context, text string) error {
			copied++
			if text != "installation-command" {
				t.Fatal("wrong command copied")
			}
			if copied == 1 {
				return errors.New("clipboard unavailable")
			}
			return nil
		},
	}}
	apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.Update(msg); m = next.(view); return cmd }
	key := func(text string) tea.Cmd { return apply(tea.KeyPressMsg{Code: rune(text[0]), Text: text}) }
	key("a")
	apply(tea.PasteMsg{Content: "render-01"})
	apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	key("2")
	cmd := apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next := apply(tea.KeyPressMsg{Code: tea.KeyEnter}); next != nil {
		t.Fatal("double enter issued a second request")
	}
	cmd = apply(cmd())
	apply(cmd())
	if m.wizard == nil || m.wizard.step != "done" || m.wizard.err == "" {
		t.Fatal("clipboard failure lost invitation")
	}
	cmd = key("c")
	apply(cmd())
	if created != 1 || copied != 2 || m.wizard.err != "" {
		t.Fatal("copy retry recreated invitation or did not recover")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 35, Height: 12}, {Width: 4, Height: 4}} {
		apply(size)
		for _, line := range strings.Split(m.View().Content, "\n") {
			if ansi.StringWidth(line) > size.Width {
				t.Fatal("wizard exceeds terminal width")
			}
		}
	}
	apply(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.wizard != nil {
		t.Fatal("wizard did not close")
	}
	apply(invitationResult{id: 1, link: enrollment.Link{Command: "late"}})
	if m.wizard != nil {
		t.Fatal("late result reopened wizard")
	}
}

func TestRegistrationIsUnavailableWithoutFleetManagement(t *testing.T) {
	m := view{width: 80, height: 24}
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if cmd != nil || next.(view).wizard != nil {
		t.Fatal("read-only dashboard opened registration")
	}
}
