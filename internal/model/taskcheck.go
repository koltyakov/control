package model

// TaskCheckMethod reports whether a task specification would be accepted by
// tasks.start, without accepting it. See D181.
const TaskCheckMethod = "tasks.check"

// Task check statuses. Only fail makes OK false.
const (
	TaskCheckPass = "pass"
	TaskCheckFail = "fail"
	// TaskCheckWarn names a condition that does not reject acceptance by
	// itself, such as a maintenance wait or an input without a source grant.
	TaskCheckWarn = "warn"
	// TaskCheckSkip means the check was not evaluated, for example because
	// the caller lacks authority for the capability.
	TaskCheckSkip = "skip"
	// TaskCheckUnchecked means the node cannot evaluate this check locally.
	TaskCheckUnchecked = "unchecked"
)

// TaskCheckAdvisory is returned with every check. It is deliberately part of
// the result so that callers and logs cannot mistake an answer for a
// reservation or for evidence about a later submission.
const TaskCheckAdvisory = "advisory snapshot: state can change before submission; nothing was reserved, and this answer does not show whether a later submission was sent or accepted"

// TaskCheck is the tasks.check result. Advisory is always true.
type TaskCheck struct {
	Advisory bool            `json:"advisory"`
	OK       bool            `json:"ok"`
	TaskID   string          `json:"taskId,omitempty"`
	Checks   []TaskCheckItem `json:"checks"`
	Pools    []TaskCheckPool `json:"pools,omitempty"`
	Note     string          `json:"note"`
}

// TaskCheckItem is one named check. Message is bounded and omitted when the
// status needs no explanation.
type TaskCheckItem struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// TaskCheckPool is an advisory snapshot of a pool the task would use.
type TaskCheckPool struct {
	Name string `json:"name"`
	ConcurrencyPool
}
