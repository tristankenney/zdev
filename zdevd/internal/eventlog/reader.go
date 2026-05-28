package eventlog

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// tailChunkSize is the read-backwards window. 4 KB is comfortably larger
// than any single Event line and small enough that we don't over-read for
// short tails.
const tailChunkSize = 4 * 1024

// TailLines returns the last n raw NDJSON lines across `path` and
// `path+".1"` in newest-first order. The current file is read newest-first,
// then `.1` is read newest-first if more lines are still needed.
//
// Per D4-12 / D4-08:
//   - history reads `events.ndjson` directly so it works during daemon
//     outage; rotation at 10MB means at most two files exist on disk.
//   - newest-first ordering: `events.ndjson` lines (newer) precede
//     `events.ndjson.1` lines (older).
//
// Missing files are not errors — TailLines returns whatever it could
// collect (possibly nil) with err==nil. Other I/O errors propagate.
//
// Returned []byte slices are independent copies; callers may retain or
// modify them freely.
func TailLines(path string, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, nil
	}

	current, err := tailFile(path, n)
	if err != nil {
		return nil, err
	}
	if len(current) >= n {
		return current[:n], nil
	}

	rest, err := tailFile(path+".1", n-len(current))
	if err != nil {
		return nil, err
	}
	return append(current, rest...), nil
}

// tailFile returns up to n newest-first lines from a single file. Missing
// file → empty slice + nil err. The implementation reads chunks from the
// end of the file backward, splitting on '\n', until either n lines are
// collected or BOF is reached.
func tailFile(path string, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("eventlog: open %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("eventlog: stat %s: %w", path, err)
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}

	// Read the file in chunks from the end, accumulating into a buffer.
	// When we have at least n+1 newlines (so the first line — possibly
	// truncated at our chunk boundary — is bounded), or we hit BOF, stop.
	//
	// `tail` represents the bytes between [pos, size) of the file. We
	// prepend earlier chunks so its leftmost byte is always at offset
	// `pos` in the file.
	var (
		pos  = size
		tail []byte
	)
	for pos > 0 && countNewlines(tail) <= n {
		readSize := int64(tailChunkSize)
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("eventlog: read %s: %w", path, err)
		}
		tail = append(buf, tail...)
	}

	return splitLastNLines(tail, n), nil
}

// countNewlines returns the number of '\n' bytes in b.
func countNewlines(b []byte) int {
	c := 0
	for _, x := range b {
		if x == '\n' {
			c++
		}
	}
	return c
}

// splitLastNLines splits buf on '\n' and returns up to n lines in
// newest-first order. Trailing empty lines (from a final '\n' on the last
// emitted line) are skipped. Each returned line is a freshly-allocated
// []byte copy.
func splitLastNLines(buf []byte, n int) [][]byte {
	if len(buf) == 0 {
		return nil
	}
	// Drop a single trailing newline so the last "real" line is not split
	// into an empty tail element. This matches the writer, which emits
	// "{json}\n" per event.
	if buf[len(buf)-1] == '\n' {
		buf = buf[:len(buf)-1]
	}

	out := make([][]byte, 0, n)
	end := len(buf)
	for end > 0 && len(out) < n {
		start := end - 1
		for start >= 0 && buf[start] != '\n' {
			start--
		}
		// `start` is either -1 (BOF) or the index of '\n'. The line we
		// want is [start+1, end).
		line := buf[start+1 : end]
		if len(line) > 0 { // skip empty lines from accidental "\n\n"
			cp := make([]byte, len(line))
			copy(cp, line)
			out = append(out, cp)
		}
		end = start
	}
	return out
}
