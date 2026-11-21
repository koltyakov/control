package update

import "testing"

func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v0.10.5", "v0.10.4", 1},
		{"v0.10.5", "v0.10.5", 0},
		{"v0.9.12", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.10.5-2-gabc", "v0.10.5", -1},
		{"v0.10.5", "dev", 1},
		{"v01.0.0", "v0.0.1", -1},
	} {
		if got := CompareReleaseVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareReleaseVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
