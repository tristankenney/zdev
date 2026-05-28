package render

import (
	"bytes"
	"io"
)

// FrameWriter wraps an io.Writer with VIS-10 differential-render skip:
// if the new frame's bytes equal the previous frame, skip the underlying
// Write. The renderer wraps os.Stdout in a FrameWriter so terminal
// bandwidth stays minimal during high-frequency animation ticks where
// most frames are byte-equal to the previous.
//
// Source: bash baseline ~/.local/bin/zdev-sidebar-render line 661 —
// `if [ "$output" != "$last_output" ]`. Phase 3 reproduces in Go.
type FrameWriter struct {
	out      io.Writer
	last     []byte // previous frame; reused across calls to avoid allocation
	wroteOut bool   // last Write actually emitted to out (vs deduped)
}

// NewFrameWriter wraps out with differential-skip semantics.
func NewFrameWriter(out io.Writer) *FrameWriter {
	return &FrameWriter{out: out}
}

// Write emits frame to the underlying writer ONLY if frame differs from
// the previous Write's input. Returns len(frame) on skip (the caller's
// "n bytes written" contract is honored as if the bytes were buffered).
func (f *FrameWriter) Write(frame []byte) (int, error) {
	if bytes.Equal(frame, f.last) {
		f.wroteOut = false
		return len(frame), nil
	}
	n, err := f.out.Write(frame)
	if err != nil {
		// On error, do NOT update last — caller may retry the same frame.
		f.wroteOut = false
		return n, err
	}
	// Reuse the slice when capacity allows; allocate only on growth.
	if cap(f.last) >= len(frame) {
		f.last = f.last[:len(frame)]
		copy(f.last, frame)
	} else {
		f.last = append(f.last[:0], frame...)
	}
	f.wroteOut = true
	return n, nil
}

// WroteLast reports whether the most recent Write call emitted bytes to
// the underlying writer (vs short-circuited by differential dedup).
// Callers gate side effects (like @last-render-ts tmux stamps) on this so
// the stamp only fires when window_activity actually advanced.
func (f *FrameWriter) WroteLast() bool { return f.wroteOut }
