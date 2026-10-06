package clipboard

import (
	"reflect"
	"testing"
)

func TestClipboardFileURIs(t *testing.T) {
	paths, err := parseURIs("#comment\r\nfile:///home/user/a%20b.txt\r\nfile://localhost/home/user/c.txt\n")
	if err != nil || !reflect.DeepEqual(paths, []string{"/home/user/a b.txt", "/home/user/c.txt"}) {
		t.Fatal(paths, err)
	}
	for _, text := range []string{"https://example.com/file", "file://remote/file", "file:relative", "file:///file?query", "file:///file#fragment", "# only comment"} {
		if _, err := parseURIs(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}
