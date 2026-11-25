// Package shellstream implements the bounded binary records of a shell stream.
package shellstream

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	Input  byte = 1
	Resize byte = 2
	Output byte = 3
	Exit   byte = 4

	MaxData = 16 << 10
	header  = 3
)

var errRecord = errors.New("invalid shell stream record")

func valid(kind byte, size int) bool {
	switch kind {
	case Input, Output:
		return size > 0 && size <= MaxData
	case Resize, Exit:
		return size == 4
	default:
		return false
	}
}

// Read returns a payload borrowed from the caller's scratch buffer.
func Read(r io.Reader, buf *[MaxData]byte) (byte, []byte, error) {
	var prefix [header]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return 0, nil, err
	}
	kind, size := prefix[0], int(binary.BigEndian.Uint16(prefix[1:]))
	if !valid(kind, size) {
		return 0, nil, errRecord
	}
	if _, err := io.ReadFull(r, buf[:size]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return kind, buf[:size], nil
}

// Writer needs external serialization when shared by input and resize.
type Writer struct {
	scratch [header + MaxData]byte
}

func (w *Writer) Write(dst io.Writer, kind byte, data []byte) error {
	if !valid(kind, len(data)) {
		return errRecord
	}
	w.scratch[0] = kind
	binary.BigEndian.PutUint16(w.scratch[1:header], uint16(len(data)))
	copy(w.scratch[header:], data)
	size := header + len(data)
	n, err := dst.Write(w.scratch[:size])
	if n != size && err == nil {
		err = io.ErrShortWrite
	}
	return err
}
