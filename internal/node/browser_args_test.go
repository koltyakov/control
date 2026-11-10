package node

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestBrowserScreenshotArgs(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "page.png")
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"path", []string{"screenshot", "page.png"}, []string{"screenshot", abs}},
		{"selector and path", []string{"screenshot", "@e1", "page.png"}, []string{"screenshot", "@e1", abs}},
		{"extensionless path", []string{"screenshot", ".card", "capture"}, []string{"screenshot", ".card", filepath.Join(dir, "capture")}},
		{"relative path", []string{"screenshot", "./.page.png"}, []string{"screenshot", filepath.Join(dir, ".page.png")}},
		{"parent path", []string{"screenshot", "../page.png"}, []string{"screenshot", filepath.Join(filepath.Dir(dir), "page.png")}},
		{"capture flags", []string{"screenshot", "--full", "--threshold", "0.1", "page.png", "--if-changed"}, []string{"screenshot", "--full", "--threshold", "0.1", abs, "--if-changed"}},
		{"global flags", []string{"--session", "work", "screenshot", "--annotate", "false", "--screenshot-format", "jpeg", "page.png"}, []string{"--session", "work", "screenshot", "--annotate", "false", "--screenshot-format", "jpeg", abs}},
		{"flag values are not paths", []string{"screenshot", "--profile", "profile.png", "@e1", "page.png"}, []string{"screenshot", "--profile", "profile.png", "@e1", abs}},
		{"absolute path", []string{"screenshot", abs}, nil},
		{"default output", []string{"screenshot"}, nil},
		{"ref only", []string{"screenshot", "@e1"}, nil},
		{"CSS only", []string{"screenshot", ".card.png"}, nil},
		{"selector only", []string{"screenshot", "button"}, nil},
		{"other command", []string{"fill", "@e1", "page.png"}, nil},
		{"batch", []string{"batch", "screenshot page.png"}, nil},
		{"unknown flag", []string{"screenshot", "--future-option", "value.png"}, nil},
		{"ambiguous restore", []string{"--restore", "work", "screenshot", "page.png"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slices.Clone(tc.args)
			want := tc.want
			if want == nil {
				want = original
			}
			if got := browserScreenshotArgs(tc.args, dir); !slices.Equal(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			if !slices.Equal(tc.args, original) {
				t.Fatal("persisted request arguments were mutated")
			}
		})
	}
}
