// Package peimage derives console-free Windows executables without changing code.
package peimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// SetGUI changes only the PE Subsystem field, after validating the fixed headers.
func SetGUI(image []byte) error {
	offset, err := guiSubsystemOffset(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint16(image[offset:offset+2], 2)
	return nil
}

func guiSubsystemOffset(image io.ReaderAt, size int64) (int64, error) {
	var dos [64]byte
	if size < int64(len(dos)) {
		return 0, errors.New("user startup requires a Windows PE executable")
	}
	if _, err := image.ReadAt(dos[:], 0); err != nil {
		return 0, err
	}
	if string(dos[:2]) != "MZ" {
		return 0, errors.New("user startup requires a Windows PE executable")
	}
	peOffset := int64(binary.LittleEndian.Uint32(dos[60:64]))
	optional := peOffset + 4 + 20
	var coff [24]byte
	var header [70]byte
	if optional+int64(len(header)) > size {
		return 0, errors.New("user startup image has a truncated or invalid PE header")
	}
	if _, err := image.ReadAt(coff[:], peOffset); err != nil {
		return 0, err
	}
	if string(coff[:4]) != "PE\x00\x00" {
		return 0, errors.New("user startup image has a truncated or invalid PE header")
	}
	if _, err := image.ReadAt(header[:], optional); err != nil {
		return 0, err
	}
	optionalSize := int64(binary.LittleEndian.Uint16(coff[20:22]))
	var minimum int64
	switch binary.LittleEndian.Uint16(header[:2]) {
	case 0x10b:
		minimum = 96
	case 0x20b:
		minimum = 112
	default:
		return 0, errors.New("user startup image has no executable optional header")
	}
	if optionalSize < minimum || optional+optionalSize > size {
		return 0, errors.New("user startup image has a truncated optional header")
	}
	characteristics := binary.LittleEndian.Uint16(coff[22:24])
	if characteristics&0x0002 == 0 || characteristics&0x2000 != 0 {
		return 0, errors.New("user startup requires an executable, not a DLL")
	}
	subsystem := binary.LittleEndian.Uint16(header[68:70])
	if subsystem != 2 && subsystem != 3 {
		return 0, errors.New("user startup requires a console or GUI executable")
	}
	return optional + 68, nil
}

// VerifyGUI checks every byte using fixed-size buffers and returns the original
// source checksum. Only the validated PE Subsystem field may differ. Startup
// must not allocate whole executable images in the persistent User process.
func VerifyGUI(source io.ReaderAt, size int64, derived io.Reader) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	offset, err := guiSubsystemOffset(source, size)
	if err != nil {
		return zero, err
	}
	hash := sha256.New()
	var original, running [32 << 10]byte
	reader := io.NewSectionReader(source, 0, size)
	for position := int64(0); position < size; {
		n := int(min(int64(len(original)), size-position))
		if _, err = io.ReadFull(reader, original[:n]); err != nil {
			return zero, err
		}
		_, _ = hash.Write(original[:n])
		for i, value := range []byte{2, 0} {
			if at := offset + int64(i) - position; at >= 0 && at < int64(n) {
				original[at] = value
			}
		}
		if _, err = io.ReadFull(derived, running[:n]); err != nil {
			return zero, err
		}
		if !bytes.Equal(original[:n], running[:n]) {
			return zero, errors.New("GUI runtime does not match its original executable")
		}
		position += int64(n)
	}
	var extra [1]byte
	if _, err = io.ReadFull(derived, extra[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("GUI runtime exceeds its original executable size")
		}
		return zero, err
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}
