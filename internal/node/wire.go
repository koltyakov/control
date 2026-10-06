package node

import (
	"io"

	"github.com/koltyakov/control/internal/wire"
)

const maxFrame = wire.MaxFrame

// Length-prefixing leaves the following bytes untouched for streaming methods.
func writeFrame(w io.Writer, value any) error {
	return wire.WriteFrame(w, value)
}

func readFrame(r io.Reader, value any) error {
	return wire.ReadFrame(r, value)
}
