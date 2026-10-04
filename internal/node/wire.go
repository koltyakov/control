package node

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const maxFrame = 8 << 20

// Length-prefixing leaves the following bytes untouched for streaming methods.
func writeFrame(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return errors.New("control message exceeds 8 MiB")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readFrame(r io.Reader, value any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > maxFrame {
		return errors.New("control message exceeds 8 MiB")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
