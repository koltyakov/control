// Package peimage derives console-free Windows executables without changing code.
package peimage

import (
	"encoding/binary"
	"errors"
)

// SetGUI changes only the PE Subsystem field, after validating the fixed headers.
func SetGUI(image []byte) error {
	if len(image) < 64 || string(image[:2]) != "MZ" {
		return errors.New("user startup requires a Windows PE executable")
	}
	peOffset := uint64(binary.LittleEndian.Uint32(image[60:64]))
	optional := peOffset + 4 + 20
	if optional+70 > uint64(len(image)) || string(image[peOffset:peOffset+4]) != "PE\x00\x00" {
		return errors.New("user startup image has a truncated or invalid PE header")
	}
	optionalSize := uint64(binary.LittleEndian.Uint16(image[peOffset+20 : peOffset+22]))
	var minimum uint64
	switch binary.LittleEndian.Uint16(image[optional : optional+2]) {
	case 0x10b:
		minimum = 96
	case 0x20b:
		minimum = 112
	default:
		return errors.New("user startup image has no executable optional header")
	}
	if optionalSize < minimum || optional+optionalSize > uint64(len(image)) {
		return errors.New("user startup image has a truncated optional header")
	}
	characteristics := binary.LittleEndian.Uint16(image[peOffset+22 : peOffset+24])
	if characteristics&0x0002 == 0 || characteristics&0x2000 != 0 {
		return errors.New("user startup requires an executable, not a DLL")
	}
	offset := optional + 68
	subsystem := binary.LittleEndian.Uint16(image[offset : offset+2])
	if subsystem != 2 && subsystem != 3 {
		return errors.New("user startup requires a console or GUI executable")
	}
	binary.LittleEndian.PutUint16(image[offset:offset+2], 2)
	return nil
}
