package model

import "time"

// TaskSummary deliberately excludes arguments, results and artifact manifests.
type TaskSummary struct {
	ID             string    `json:"id"`
	Capability     string    `json:"capability"`
	State          string    `json:"state"`
	Phase          string    `json:"phase,omitempty"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	Error          string    `json:"error,omitempty"`
	ErrorTruncated bool      `json:"errorTruncated,omitempty"`
}

type TaskPage struct {
	Tasks      []TaskSummary `json:"tasks"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

func SummarizeTask(t Task) TaskSummary {
	s := TaskSummary{ID: t.ID, Capability: t.Spec.Capability, State: t.State, Phase: t.Phase, Created: t.Created, Updated: t.Updated, Error: t.Error}
	if len(s.Error) > 1024 {
		s.Error = s.Error[:1024]
		s.ErrorTruncated = true
	}
	return s
}
