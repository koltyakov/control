package clipboard

const OpenMethod = "clipboard.open"
const PasteMethod = "clipboard.paste"

type Request struct {
	Protocol string  `json:"protocol"`
	Content  Content `json:"content,omitempty"`
	Dir      string  `json:"dir,omitempty"`
}

type Ack struct {
	Protocol string  `json:"protocol"`
	Content  Content `json:"content,omitempty"`
}

type Ready struct {
	Ready bool `json:"ready"`
}

// Result deliberately excludes clipboard text and source paths from tool output.
type Result struct {
	Kind  string   `json:"kind"`
	Files []string `json:"files,omitempty"`
	Bytes int64    `json:"bytes"`
}

func (c Content) Result() Result {
	r := Result{Kind: c.Kind, Bytes: int64(len(c.Text))}
	for _, f := range c.Files {
		r.Files = append(r.Files, f.Name)
		r.Bytes += f.Size
	}
	return r
}
