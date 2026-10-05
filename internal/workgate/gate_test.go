package workgate

import (
	"context"
	"testing"
	"time"
)

func TestDisabledAdmissionPreservesWorkAndMaintenance(t *testing.T) {
	g := New()
	release, err := g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g.SetDisabled(true)
	if _, err := g.Enter(context.Background()); err == nil {
		t.Fatal("disabled gate admitted work")
	}
	if active, _ := g.State(); active != 1 {
		t.Fatal("disable discarded active work")
	}
	release()
	if !g.PauseIfIdle() {
		t.Fatal("disabled idle machine cannot update")
	}
	g.Resume()
	if _, err := g.Enter(context.Background()); err == nil {
		t.Fatal("update resume enabled machine")
	}
	if !g.PauseIfIdle() {
		t.Fatal("pause")
	}
	g.SetDisabled(false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Enter(ctx); err == nil {
		t.Fatal("enable bypassed maintenance")
	}
	g.Resume()
	release, err = g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
}
