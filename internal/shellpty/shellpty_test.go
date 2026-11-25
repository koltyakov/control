package shellpty

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestStartRejectsUnsupportedNames(t *testing.T) {
	for _, name := range []string{"", "sh", "zsh", "BASH", "bash ", "/bin/bash", "bash.exe", "bash -c id", "cmd.exe", "../bash"} {
		if slices.Contains(Names(), name) {
			continue
		}
		term, err := Start(context.Background(), name, 80, 24, "", "")
		if term != nil {
			_ = term.Close()
		}
		if !errors.Is(err, ErrUnsupportedShell) {
			t.Errorf("Start(%q) error = %v, want ErrUnsupportedShell", name, err)
		}
	}
}

func TestStartValidatesArguments(t *testing.T) {
	name := Default()
	tests := []struct {
		cols, rows uint16
		term, dir  string
	}{
		{0, 24, "", ""},
		{80, 0, "", ""},
		{MaxDimension + 1, 24, "", ""},
		{80, MaxDimension + 1, "", ""},
		{80, 24, "xterm 256color", ""},
		{80, 24, "xterm;rm", ""},
		{80, 24, strings.Repeat("x", MaxTermLength+1), ""},
		{80, 24, "", "relative/dir"},
		{80, 24, "", t.TempDir() + "/missing"},
	}
	for _, tt := range tests {
		term, err := Start(context.Background(), name, tt.cols, tt.rows, tt.term, tt.dir)
		if term != nil {
			_ = term.Close()
		}
		if err == nil || errors.Is(err, ErrShellNotFound) {
			t.Errorf("Start(%d, %d, %q, %q) error = %v, want validation error", tt.cols, tt.rows, tt.term, tt.dir, err)
		}
	}
}

func TestValidateTerm(t *testing.T) {
	for _, term := range []string{"", "xterm-256color", "screen.xterm", "vt100+fnkeys", "tmux_256", strings.Repeat("a", MaxTermLength)} {
		if err := ValidateTerm(term); err != nil {
			t.Errorf("ValidateTerm(%q) = %v", term, err)
		}
	}
	for _, term := range []string{"xterm\n", "x=y", "ä", "a/b", strings.Repeat("a", MaxTermLength+1)} {
		if ValidateTerm(term) == nil {
			t.Errorf("ValidateTerm(%q) accepted", term)
		}
	}
}

func TestValidateSizeBounds(t *testing.T) {
	if err := ValidateSize(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSize(MaxDimension, MaxDimension); err != nil {
		t.Fatal(err)
	}
}

func TestWithTermReplacesInheritedTerm(t *testing.T) {
	env := []string{"A=1", "TERM=dumb", "=C:=C:\\", "Term=x", "B=2"}
	got := withTerm(env, "xterm", false)
	want := []string{"A=1", "=C:=C:\\", "Term=x", "B=2", "TERM=xterm"}
	if !slices.Equal(got, want) {
		t.Fatalf("withTerm = %q, want %q", got, want)
	}
	got = withTerm(env, "", true)
	want = []string{"A=1", "=C:=C:\\", "B=2"}
	if !slices.Equal(got, want) {
		t.Fatalf("withTerm fold = %q, want %q", got, want)
	}
}

func TestAvailableIsSubsetOfNames(t *testing.T) {
	for _, name := range Available() {
		if !slices.Contains(Names(), name) {
			t.Fatalf("Available() includes %q outside Names()", name)
		}
	}
	if Default() != Names()[0] {
		t.Fatalf("Default() = %q", Default())
	}
}
