package model

import "regexp"

// TaskIDPattern is the task ID syntax nodes accept: 1-128 ASCII letters,
// digits, underscores and hyphens. Machine names may contain dots; IDs may not.
var TaskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
