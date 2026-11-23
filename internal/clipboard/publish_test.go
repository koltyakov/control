package clipboard

import (
	"os"
	"testing"
)

func TestPublishWithoutLinkNeverReplaces(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	write := func(name, data string) {
		if err := root.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".temp-a", "new")
	if err := publishWithoutLink(root, ".temp-a", "pasted.txt"); err != nil {
		t.Fatal(err)
	}
	write(".temp-b", "other")
	if err := publishWithoutLink(root, ".temp-b", "pasted.txt"); err == nil {
		t.Fatal("existing file replaced")
	}
	if data, err := root.ReadFile("pasted.txt"); err != nil || string(data) != "new" {
		t.Fatal("published file changed", string(data), err)
	}
}
