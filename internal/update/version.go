package update

import (
	"strconv"
	"strings"
)

// ParseReleaseVersion parses a stable release tag such as v1.2.3.
func ParseReleaseVersion(tag string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return v, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || (len(part) > 1 && part[0] == '0') || strconv.Itoa(n) != part {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// CompareReleaseVersions orders stable release tags. An unparsable version
// orders before every parsable one, so it never displaces a known floor.
func CompareReleaseVersions(a, b string) int {
	va, okA := ParseReleaseVersion(a)
	vb, okB := ParseReleaseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// AboveReleaseFloor reports whether tag may replace a fleet whose newest
// GitHub selection was floor. Tags outside vMAJOR.MINOR.PATCH, on either
// side, cannot be ordered and fall back to publication-date checks.
func AboveReleaseFloor(tag, floor string) bool {
	_, okTag := ParseReleaseVersion(tag)
	_, okFloor := ParseReleaseVersion(floor)
	return !okTag || !okFloor || CompareReleaseVersions(tag, floor) > 0
}
