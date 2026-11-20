package model

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPageArtifactsNewestFirstWithStableCursor(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var artifacts []Artifact
	for i := range 7 {
		// Two artifacts share each timestamp to exercise the ID tie-break.
		id := strings.Repeat(fmt.Sprintf("%x", i), 64)
		artifacts = append(artifacts, Artifact{Node: "node-id", ID: id, SHA256: id, Name: fmt.Sprint(i), Size: int64(i + 1), Created: base.Add(time.Duration(i/2) * time.Hour)})
	}
	var seen []string
	cursor := ""
	for range 10 {
		page, err := PageArtifacts("node-id", append([]Artifact(nil), artifacts...), cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 7 || page.TotalBytes != 28 || page.Node != "node-id" {
			t.Fatalf("totals must cover every artifact: %+v", page)
		}
		for _, a := range page.Artifacts {
			seen = append(seen, a.Name)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if got := strings.Join(seen, ","); got != "6,4,5,2,3,0,1" {
		t.Fatalf("pages were not newest first without repeats or gaps: %s", got)
	}
	for _, q := range []struct {
		cursor string
		limit  int
	}{{"", 201}, {"", -1}, {"not-a-cursor", 3}, {"1-" + strings.Repeat("g", 64), 3}} {
		if _, err := PageArtifacts("node-id", artifacts, q.cursor, q.limit); err == nil {
			t.Errorf("accepted invalid page request %+v", q)
		}
	}
}
