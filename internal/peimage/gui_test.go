package peimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func consoleImage(size, peOffset int) []byte {
	image := make([]byte, size)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[60:], uint32(peOffset))
	copy(image[peOffset:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(image[peOffset+20:], 112)
	binary.LittleEndian.PutUint16(image[peOffset+22:], 2)
	binary.LittleEndian.PutUint16(image[peOffset+24:], 0x20b)
	binary.LittleEndian.PutUint16(image[peOffset+24+68:], 3)
	return image
}

func TestVerifyGUICompleteImageAndChunkBoundaries(t *testing.T) {
	for _, offset := range []int{64, (32 << 10) - 93, (32 << 10) - 92, (32 << 10) - 91, 64 << 10} {
		original := consoleImage(128<<10, offset)
		derived := bytes.Clone(original)
		if err := SetGUI(derived); err != nil {
			t.Fatal(err)
		}
		sum, err := VerifyGUI(bytes.NewReader(original), int64(len(original)), bytes.NewReader(derived))
		if err != nil || sum != sha256.Sum256(original) {
			t.Fatalf("offset %d: checksum %x, error %v", offset, sum, err)
		}
		for _, position := range []int{0, offset + 24 + 68, len(derived) / 2, len(derived) - 1} {
			changed := bytes.Clone(derived)
			changed[position] ^= 1
			if _, err := VerifyGUI(bytes.NewReader(original), int64(len(original)), bytes.NewReader(changed)); err == nil {
				t.Fatalf("accepted changed byte %d", position)
			}
		}
		for _, changed := range [][]byte{derived[:len(derived)-1], append(bytes.Clone(derived), 0)} {
			if _, err := VerifyGUI(bytes.NewReader(original), int64(len(original)), bytes.NewReader(changed)); err == nil {
				t.Fatal("accepted a truncated or extended image")
			}
		}
		if !bytes.Equal(original, consoleImage(len(original), offset)) {
			t.Fatal("verification modified the source")
		}
	}
}

type boundedReader struct{ *bytes.Reader }

func (r boundedReader) Read(p []byte) (int, error) {
	if len(p) > 32<<10 {
		return 0, errors.New("verification requested an executable-sized buffer")
	}
	return r.Reader.Read(p)
}

func TestVerifyGUIBoundsReadsAndRejectsInvalidHeaders(t *testing.T) {
	original := consoleImage(8<<20, 64)
	derived := bytes.Clone(original)
	if err := SetGUI(derived); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyGUI(bytes.NewReader(original), int64(len(original)), boundedReader{bytes.NewReader(derived)}); err != nil {
		t.Fatal(err)
	}
	for _, image := range [][]byte{nil, original[:63], original[:100], bytes.Repeat([]byte{'x'}, 512)} {
		if _, err := VerifyGUI(bytes.NewReader(image), int64(len(image)), bytes.NewReader(derived)); err == nil {
			t.Fatal("accepted invalid PE source")
		}
	}
}

// Keep the former startup algorithm as a benchmark baseline, not production code.
func BenchmarkGUIValidation(b *testing.B) {
	original := consoleImage(38<<20, 64)
	derived := bytes.Clone(original)
	if err := SetGUI(derived); err != nil {
		b.Fatal(err)
	}
	b.Run("whole-images-before", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			a, err := io.ReadAll(bytes.NewReader(original))
			if err != nil {
				b.Fatal(err)
			}
			_ = sha256.Sum256(a)
			if err := SetGUI(a); err != nil {
				b.Fatal(err)
			}
			c, err := io.ReadAll(bytes.NewReader(derived))
			if err != nil || !bytes.Equal(a, c) {
				b.Fatal("comparison failed", err)
			}
		}
	})
	b.Run("streamed-after", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := VerifyGUI(bytes.NewReader(original), int64(len(original)), bytes.NewReader(derived)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
