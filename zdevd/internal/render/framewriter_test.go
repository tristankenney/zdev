package render

import (
	"bytes"
	"testing"
)

func TestFrameWriter_FirstWriteAlwaysWritten(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	n, err := fw.Write([]byte("frame1"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 6 {
		t.Errorf("n = %d; want 6", n)
	}
	if !bytes.Equal(buf.Bytes(), []byte("frame1")) {
		t.Errorf("buf = %q; want %q", buf.Bytes(), "frame1")
	}
}

func TestFrameWriter_SkipEqualFrame(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	fw.Write([]byte("frame1")) //nolint:errcheck
	if buf.Len() != 6 {
		t.Fatalf("setup: buf.Len = %d", buf.Len())
	}
	n, err := fw.Write([]byte("frame1"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 6 {
		t.Errorf("n = %d; want 6 (caller contract honored on skip)", n)
	}
	if buf.Len() != 6 {
		t.Errorf("buf.Len = %d; want 6 (no second write — VIS-10 dedup)", buf.Len())
	}
}

func TestFrameWriter_WriteDifferentFrame(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	fw.Write([]byte("frame1")) //nolint:errcheck
	fw.Write([]byte("frame2")) //nolint:errcheck
	want := []byte("frame1frame2")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("buf = %q; want %q", buf.Bytes(), want)
	}
}

func TestFrameWriter_WriteEmpty(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	fw.Write([]byte{}) //nolint:errcheck
	fw.Write([]byte{}) //nolint:errcheck
	if buf.Len() != 0 {
		t.Errorf("buf.Len = %d; want 0 (empty == empty)", buf.Len())
	}
}
