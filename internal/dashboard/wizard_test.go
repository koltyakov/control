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

func TestRegistrationEmptyNameUsesTargetHostname(t *testing.T) {
	for _, name := range []string{"", "auto"} {
		m := view{ctx: context.Background(), wizard: &registration{id: 1, step: "name", name: name}, options: Options{
			Invite: func(_ context.Context, name, platform string) (enrollment.Link, error) {
				if name != "auto" || platform != "darwin" {
					t.Fatalf("wrong automatic naming: %q, %q", name, platform)
				}
				return enrollment.Link{}, nil
			},
		}}
		next, _ := m.updateRegistration(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = next.(view)
		if m.wizard.step != "platform" || m.wizard.name != "auto" {
			t.Fatal("blank or auto name did not advance to platform selection")
		}
		_, cmd := m.updateRegistration(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("auto naming did not create an invitation")
		}
		if result := cmd().(invitationResult); result.err != nil {
			t.Fatal(result.err)
		}
	}
}

func TestRegistrationClosesAfterRedeemedMachineComesOnline(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "automatic"}[auto], func(t *testing.T) {
			invitation := enrollment.Invitation{ID: "current", Name: "worker", AutoName: auto}
			if auto {
				invitation.Name = ""
			}
			checks, fetches := 0, 0
			online := false
			m := view{ctx: context.Background(), options: Options{Interval: time.Second, Timeout: time.Second,
				InvitationStatus: func(ctx context.Context, id string) (enrollment.Invitation, error) {
					checks++
					if id != invitation.ID {
						t.Fatalf("checked another invitation: %q", id)
					}
					if _, bounded := ctx.Deadline(); !bounded {
						t.Fatal("status request lacks a deadline")
					}
					return invitation, nil
				},
			}, wizard: &registration{id: 1, step: "copying", link: enrollment.Link{Invitation: invitation}},
				snapshot: model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{{ID: "existing", Name: "worker", Online: true}}},
				fetch: func(context.Context) (model.PoolActivitySnapshot, error) {
					fetches++
					return model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{ID: "new-identity", Name: "target-hostname", Online: online}}}, nil
				},
			}
			apply := func(msg tea.Msg) tea.Cmd { next, cmd := m.Update(msg); m = next.(view); return cmd }
			cmd := apply(clipboardResult{id: 1})
			if cmd == nil || !m.wizard.checking {
				t.Fatal("displaying the command did not start a status check")
			}
			if m.checkInvitation() != nil {
				t.Fatal("started an overlapping status request")
			}
			apply(cmd())
			if m.wizard == nil || m.wizard.checking {
				t.Fatal("existing same-name machine closed an unredeemed invitation")
			}
			invitation.RedeemedID, invitation.Name = "new-identity", "target-hostname"
			// The next ordinary dashboard refresh checks the pending invitation.
			batch := apply(result{snapshot: m.snapshot})
			commands, ok := batch().(tea.BatchMsg)
			if !ok || len(commands) != 2 {
				t.Fatal("dashboard refresh did not schedule an invitation check")
			}
			refresh := apply(commands[1]())
			if m.wizard == nil || m.wizard.redeemedID != "new-identity" || refresh == nil || !m.busy || checks != 2 {
				t.Fatal("redemption did not retain the dialog and request an online check")
			}
			batch = apply(refresh())
			if m.wizard == nil || fetches != 1 || m.busy || m.snapshot.ObservedAt.IsZero() {
				t.Fatal("offline machine closed the dialog")
			}
			commands = batch().(tea.BatchMsg)
			if apply(commands[1]()) != nil {
				t.Fatal("repeated redemption bypassed the normal polling interval")
			}
			online = true
			refresh = apply(tick{generation: m.generation})
			apply(refresh())
			if m.wizard != nil || fetches != 2 || m.busy || !m.snapshot.Nodes[0].Online {
				t.Fatal("online machine did not close the dialog with a refreshed list")
			}
			if m.checkInvitation() != nil {
				t.Fatal("invitation polling continued after closing")
			}
		})
	}
}

func TestRegistrationWaitsForFreshOnlineSnapshot(t *testing.T) {
	online := model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{{ID: "reinstalled", Online: true}}}
	m := view{ctx: context.Background(), options: Options{Interval: time.Second, Timeout: time.Second},
		wizard:   &registration{id: 1, step: "done", link: enrollment.Link{Invitation: enrollment.Invitation{ID: "current"}}},
		snapshot: online,
		fetch:    func(context.Context) (model.PoolActivitySnapshot, error) { return online, nil },
	}
	// Reinstallation uses the same identity. A request started before redemption
	// may still report the old process online, even if its result arrives later.
	oldPoll := m.poll()
	m.busy = true
	next, cmd := m.Update(invitationStatusResult{id: 1, invitation: enrollment.Invitation{ID: "current", RedeemedID: "reinstalled"}})
	m = next.(view)
	if m.wizard == nil || cmd != nil {
		t.Fatal("redemption used an older online snapshot or duplicated an in-flight poll")
	}
	next, _ = m.Update(oldPoll())
	m = next.(view)
	if m.wizard == nil {
		t.Fatal("snapshot requested before redemption closed the dialog")
	}
	for _, response := range []result{
		{registrationID: 1, snapshot: online, err: errors.New("gateway unavailable")},
		{registrationID: 2, snapshot: online},
		{registrationID: 1, snapshot: model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{{ID: "other", Online: true}}}},
	} {
		next, _ = m.Update(response)
		m = next.(view)
		if m.wizard == nil {
			t.Fatal("failed or unrelated online observation closed the dialog")
		}
	}
	next, _ = m.Update(m.poll()())
	if next.(view).wizard != nil {
		t.Fatal("fresh online observation did not close the dialog")
	}
}

func TestRegistrationIgnoresUnconfirmedAndStaleRedemption(t *testing.T) {
	for _, test := range []struct {
		name   string
		result invitationStatusResult
	}{
		{"pending", invitationStatusResult{id: 2, invitation: enrollment.Invitation{ID: "current"}}},
		{"expired", invitationStatusResult{id: 2, invitation: enrollment.Invitation{ID: "current", ExpiresAt: time.Now().Add(-time.Minute)}}},
		{"revoked", invitationStatusResult{id: 2, invitation: enrollment.Invitation{ID: "current", Revoked: true, RedeemedID: "node"}}},
		{"unavailable", invitationStatusResult{id: 2, err: errors.New("gateway unavailable")}},
		{"other-invitation", invitationStatusResult{id: 2, invitation: enrollment.Invitation{ID: "other", RedeemedID: "node"}}},
		{"previous-dialog", invitationStatusResult{id: 1, invitation: enrollment.Invitation{ID: "current", RedeemedID: "node"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := view{wizard: &registration{id: 2, step: "done", checking: true, link: enrollment.Link{Invitation: enrollment.Invitation{ID: "current"}}}}
			next, cmd := m.Update(test.result)
			m = next.(view)
			if m.wizard == nil || cmd != nil {
				t.Fatal("unconfirmed or stale redemption closed the dialog")
			}
			if m.wizard.checking != (test.result.id != 2) {
				t.Fatal("status completion changed the wrong dialog's polling state")
			}
		})
	}
	// Results arriving after manual dismissal cannot reopen the dialog or poll.
	m := view{}
	next, cmd := m.Update(invitationStatusResult{id: 1, invitation: enrollment.Invitation{ID: "old", RedeemedID: "node"}})
	if next.(view).wizard != nil || cmd != nil {
		t.Fatal("late redemption changed a dismissed dialog")
	}
}
