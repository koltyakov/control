package shellstream

import (
	"bytes"
	"io"
	"testing"
)

func TestRecordRoundTripAndLimits(t *testing.T) {
	var wire bytes.Buffer
	var writer Writer
	payload := bytes.Repeat([]byte{0xff}, MaxData)
	if err := writer.Write(&wire, Output, payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(&wire, Exit, []byte{0, 0, 0, 27}); err != nil {
		t.Fatal(err)
	}
	var scratch [MaxData]byte
	kind, data, err := Read(&wire, &scratch)
	if err != nil || kind != Output || !bytes.Equal(data, payload) {
		t.Fatalf("output kind=%v size=%v err=%v", kind, len(data), err)
	}
	kind, data, err = Read(&wire, &scratch)
	if err != nil || kind != Exit || !bytes.Equal(data, []byte{0, 0, 0, 27}) {
		t.Fatalf("exit kind=%v data=%v err=%v", kind, data, err)
	}
	if _, _, err := Read(&wire, &scratch); err != io.EOF {
		t.Fatal("expected stream EOF", err)
	}

	for _, invalid := range [][]byte{
		{0xff, 0, 1}, {Input, 0, 0}, {Output, 0x40, 1},
		{Resize, 0, 3}, {Exit, 0, 5}, {Input, 0, 1}, {Output, 0},
	} {
		if _, _, err := Read(bytes.NewReader(invalid), &scratch); err == nil || err == io.EOF {
			t.Errorf("accepted malformed or truncated record %v: %v", invalid, err)
		}
	}
	if err := writer.Write(&wire, Input, make([]byte, MaxData+1)); err == nil {
		t.Fatal("wrote oversized record")
	}
	if wire.Len() != 0 {
		t.Fatal("invalid write sent bytes")
	}
}
