package model

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultArtifactPage = 50
	MaxArtifactPage     = 200
)

// ArtifactSummary omits fields an artifact page already implies: the serving
// node is named once per page, and an artifact ID is its SHA-256 digest.
type ArtifactSummary struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// ArtifactPage lists visible artifacts newest first. Total counts and bytes
// cover every visible artifact, not just this page.
type ArtifactPage struct {
	Node       string            `json:"node"`
	Artifacts  []ArtifactSummary `json:"artifacts"`
	Total      int               `json:"total"`
	TotalBytes int64             `json:"totalBytes"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

var artifactCursor = regexp.MustCompile(`^[0-9]{1,19}-[0-9a-f]{64}$`)

// PageArtifacts orders newest first, breaking creation-time ties by ID. The
// cursor names the last returned artifact, so concurrent publication or
// deletion never repeats or skips an artifact that remains visible.
func PageArtifacts(node string, artifacts []Artifact, cursor string, limit int) (ArtifactPage, error) {
	if limit == 0 {
		limit = DefaultArtifactPage
	}
	if limit < 1 || limit > MaxArtifactPage || (cursor != "" && !artifactCursor.MatchString(cursor)) {
		return ArtifactPage{}, fmt.Errorf("artifact page requires limit 1..%d and a cursor from a previous page", MaxArtifactPage)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifactAfter(artifacts[i], artifacts[j]) })
	page := ArtifactPage{Node: node, Artifacts: []ArtifactSummary{}, Total: len(artifacts)}
	var after Artifact
	if cursor != "" {
		nanos, id, _ := strings.Cut(cursor, "-")
		value, err := strconv.ParseInt(nanos, 10, 64)
		if err != nil {
			return ArtifactPage{}, errors.New("invalid artifact page cursor")
		}
		after = Artifact{ID: id, Created: time.Unix(0, value).UTC()}
	}
	for _, a := range artifacts {
		page.TotalBytes += a.Size
		if cursor != "" && !artifactAfter(after, a) {
			continue
		}
		if len(page.Artifacts) == limit {
			last := page.Artifacts[limit-1]
			page.NextCursor = fmt.Sprintf("%d-%s", last.Created.UnixNano(), last.ID)
			continue
		}
		page.Artifacts = append(page.Artifacts, ArtifactSummary{ID: a.ID, Name: a.Name, Size: a.Size, Created: a.Created})
	}
	return page, nil
}

// artifactAfter reports whether a sorts before b in newest-first order.
func artifactAfter(a, b Artifact) bool {
	if !a.Created.Equal(b.Created) {
		return a.Created.After(b.Created)
	}
	return a.ID < b.ID
}
