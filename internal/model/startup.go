package model

// StartupRequest is a management instruction, never a shell command or a path.
type StartupRequest struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// StartupReply distinguishes admission from observed completion. Unknown means
// the instruction may have run and must not be replayed automatically.
type StartupReply struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
