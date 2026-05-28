package tmuxctl

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
)

// maxLineBytes caps the bufio.Scanner buffer for a single tmux protocol line.
// Default 64 KB is too small for %output payloads on wide panes with long
// scrollback; 1 MiB matches proto.MaxSnapshotBytes for symmetry across the
// codebase (research §"Scanner setup").
const maxLineBytes = 1 * 1024 * 1024

// Parser is the line-oriented tmux -CC decoder.
//
// Single-reader-goroutine: Run() consumes bytes from r in one goroutine and
// emits typed Event values on a channel. The parser NEVER mutates shared
// state — that is the hub's job (Plan 02-03).
type Parser struct {
	r         io.Reader
	sink      OutputSink
	activeCmd int64        // 0 = idle; non-zero = inside %begin/%end block awaiting matching number
	blockBuf  bytes.Buffer // accumulated in-block bytes; reset on %end (Phase 2 discards)
}

// NewParser returns a Parser that reads from r. sink may be nil; nil is
// replaced with discardSink{} per D2-11.
func NewParser(r io.Reader, sink OutputSink) *Parser {
	if sink == nil {
		sink = discardSink{}
	}
	return &Parser{r: r, sink: sink}
}

// Run drives the read loop until r returns EOF or context cancels. Returns
// nil on clean EOF (which usually means the tmux -CC subprocess exited and
// the supervisor will reconnect). Errors propagate as-is; ctx.Err() is
// returned when the context cancels mid-stream.
func (p *Parser) Run(ctx context.Context, events chan<- Event) error {
	sc := bufio.NewScanner(p.r)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	sc.Split(bufio.ScanLines)

	firstLine := true
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		// bufio.Scanner reuses the underlying buffer; copy if we plan to
		// retain bytes across iterations. We don't retain after classify
		// returns, so direct read is fine for top-level handling. (The
		// PaneTitleChanged builder copies its payload via DecodeOctal which
		// allocates fresh bytes; ParseError uses cloneBytes.)

		// Strip DSC prefix from the very first line (only) — tmux emits
		// \x1bP1000p once at the top of the stream when entering -CC mode.
		// Live captures may include script(1) prologue bytes (`^D\b\b`)
		// before the marker; stripDSCPrefix uses a skip-past pattern.
		if firstLine {
			line = stripDSCPrefix(line)
			firstLine = false
			if len(line) == 0 {
				continue
			}
		}
		// Strip DSC suffix unconditionally — \x1b\ may appear at the end
		// of any line on -CC exit. Cheap to check on every line.
		line = stripDSCSuffix(line)
		if len(line) == 0 {
			continue
		}

		ev := p.classify(line)
		if ev == nil {
			continue
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

// classify converts one already-stripped line into an Event (or nil if the
// line is in-block payload, an empty %begin marker, or an unknown forward-
// compat notification).
func (p *Parser) classify(line []byte) Event {
	// In-block: anything other than %end <activeCmd> or %error <activeCmd>
	// is opaque payload. Critical: we MATCH on the command number, not on
	// the %end prefix alone (Pitfall 1).
	if p.activeCmd != 0 {
		if num, isError, ok := parseEndOrErrorLine(line); ok && num == p.activeCmd {
			// %end and %error both close the block. The parser-level path
			// (used for fixture diffing) doesn't interpret the body, so
			// the only observable difference here is the activeCmd reset.
			// The supervisor's separate path discriminates and logs.
			_ = isError
			p.activeCmd = 0
			p.blockBuf.Reset()
			return nil
		}
		// Opaque payload — accumulate (Phase 2 discards on %end, but
		// accumulating preserves byte-faithful fixture diffing).
		p.blockBuf.Write(line)
		p.blockBuf.WriteByte('\n')
		return nil
	}

	// Idle — top-level line. First check for %begin to enter in-block mode.
	if num, ok := parseBeginLine(line); ok {
		p.activeCmd = num
		p.blockBuf.Reset()
		return nil
	}

	// Otherwise: classify as a top-level notification.
	return p.classifyNotification(line)
}

// classifyNotification dispatches a top-level `%foo args...` line to a
// concrete Event type. Returns nil for forward-compat unknowns.
func (p *Parser) classifyNotification(line []byte) Event {
	// All notifications start with `%`. Any other top-level line is a
	// protocol violation (rare; could be the trailing ST of -CC exit
	// delivered on its own line after stripDSCSuffix already shrank it
	// to empty — that case is filtered in Run() before we get here).
	if len(line) == 0 || line[0] != '%' {
		return nil
	}

	// Tokenize: keyword is up to the first space; remainder is " args".
	sp := bytes.IndexByte(line, ' ')
	var keyword, rest string
	if sp == -1 {
		keyword = string(line)
		rest = ""
	} else {
		keyword = string(line[:sp])
		rest = string(line[sp+1:])
	}

	switch keyword {
	case "%sessions-changed":
		return SessionsChanged{}
	case "%session-changed":
		// %session-changed $<sessid> <name>
		id, name, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "session-changed: expected 2 args"}
		}
		return SessionChanged{ID: id, Name: name}
	case "%session-renamed":
		id, name, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "session-renamed: expected 2 args"}
		}
		return SessionRenamed{ID: id, NewName: name}
	case "%session-window-changed":
		sid, wid, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "session-window-changed: expected 2 args"}
		}
		return SessionWindowChanged{SessionID: sid, WindowID: wid}
	case "%window-add":
		if rest == "" {
			return ParseError{Line: cloneBytes(line), Cause: "window-add: missing window id"}
		}
		return WindowAdd{ID: rest}
	case "%window-close":
		if rest == "" {
			return ParseError{Line: cloneBytes(line), Cause: "window-close: missing window id"}
		}
		return WindowClose{ID: rest}
	case "%window-renamed":
		id, name, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "window-renamed: expected 2 args"}
		}
		return WindowRenamed{ID: id, NewName: name}
	case "%unlinked-window-add":
		return UnlinkedWindowAdd{ID: rest}
	case "%unlinked-window-close":
		return UnlinkedWindowClose{ID: rest}
	case "%unlinked-window-renamed":
		id, name, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "unlinked-window-renamed: expected 2 args"}
		}
		return UnlinkedWindowRenamed{ID: id, NewName: name}
	case "%window-pane-changed":
		wid, pid, ok := splitTwo(rest)
		if !ok {
			return ParseError{Line: cloneBytes(line), Cause: "window-pane-changed: expected 2 args"}
		}
		return WindowPaneChanged{WindowID: wid, PaneID: pid}
	case "%pane-mode-changed":
		// We don't act on copy-mode transitions in Phase 2; forward-compat skip.
		return nil
	case "%output":
		// %output %<paneid> <octal-escaped-payload>
		sp2 := strings.IndexByte(rest, ' ')
		if sp2 == -1 {
			return ParseError{Line: cloneBytes(line), Cause: "output: missing payload"}
		}
		paneID := rest[:sp2]
		payload := DecodeOctal([]byte(rest[sp2+1:]))
		p.sink.Output(paneID, payload)
		return nil
	case "%subscription-changed":
		return parseSubscriptionChanged(rest, line)
	case "%client-detached":
		return ClientDetached{Client: rest}
	case "%client-session-changed":
		// %client-session-changed client $session-id session-name
		// We only need client and session-name; skip the session-id.
		parts := strings.SplitN(rest, " ", 3)
		if len(parts) == 3 {
			return ClientSessionChanged{Client: parts[0], SessionName: parts[2]}
		}
		return nil
	case "%exit":
		return Exit{Reason: rest}
	case "%paste-buffer-changed", "%paste-buffer-deleted",
		"%message", "%layout-change", "%config-error",
		"%pause", "%continue", "%extended-output":
		// Known-but-irrelevant in Phase 2 — log at Debug elsewhere; here, skip.
		return nil
	default:
		// Forward-compat: unknown %foo notifications are NOT errors. The
		// protocol promises forward compatibility for unknown notifications
		// (Pitfall 16).
		return nil
	}
}

// parseSubscriptionChanged handles the OQ-1 line shape recorded in
// OQ-RESOLUTIONS.md. Per the Wave 0 capture, the line shape is:
//
//	%subscription-changed <name> $<sessid> @<winid> <int> %<paneid> : <octal-escaped-value>
//
// Field index (post-keyword):
//
//	[0] subscription name (e.g., "zdev-titles" or "zdev-titles-$0")
//	[1] session id ($-prefixed)
//	[2] window id (@-prefixed)
//	[3] integer (window index?; semantics not yet confirmed)
//	[4] pane id (%-prefixed)
//	then literal " : " separator, then the format value (raw bytes).
//
// We dispatch by subscription-name prefix:
//
//   - "zdev-titles" (bare) or "zdev-titles-$..." → PaneTitleChanged
//   - "zdev-cmds-$..."                           → PaneCommandChanged (DATA-03)
//   - "zdev-act-$..."                            → ActivityRefresh (DATA-07, VIS-12)
//
// Foreign subscription-changed lines are silently skipped.
func parseSubscriptionChanged(rest string, fullLine []byte) Event {
	// Split off the format value (everything after the first " : ").
	colonIdx := strings.Index(rest, " : ")
	if colonIdx == -1 {
		return ParseError{Line: cloneBytes(fullLine), Cause: "subscription-changed: missing ' : ' separator"}
	}
	head := rest[:colonIdx]
	payload := rest[colonIdx+3:]

	fields := strings.Fields(head)
	// Expected: name, $<sessid>, @<winid>, <int>, %<paneid> (5 head fields).
	if len(fields) < 5 {
		return ParseError{Line: cloneBytes(fullLine), Cause: "subscription-changed: expected at least 5 head fields"}
	}
	name := fields[0]
	sessID := fields[1] // $-prefixed session ID
	paneID := fields[4]

	switch {
	case name == "zdev-titles" || strings.HasPrefix(name, "zdev-titles-"):
		// pane_title subscription — emit PaneTitleChanged.
		if !strings.HasPrefix(paneID, "%") {
			return ParseError{Line: cloneBytes(fullLine), Cause: "subscription-changed: pane id missing %"}
		}
		title := string(DecodeOctal([]byte(payload)))
		return PaneTitleChanged{PaneID: paneID, Title: title}

	case strings.HasPrefix(name, "zdev-cmds-"):
		// pane_current_command subscription — emit PaneCommandChanged (DATA-03).
		// Drop empty payloads: empty means the format push isn't supported by
		// this tmux build; the subscriber will rely on DataRefresh.ShellCmd
		// from the branch probe as a fallback.
		if !strings.HasPrefix(paneID, "%") {
			return ParseError{Line: cloneBytes(fullLine), Cause: "subscription-changed: pane id missing %"}
		}
		cmd := strings.TrimSpace(string(DecodeOctal([]byte(payload))))
		if cmd == "" {
			return nil // empty payload — silently drop
		}
		return PaneCommandChanged{PaneID: paneID, Cmd: cmd}

	case strings.HasPrefix(name, "zdev-act-"):
		// window_activity subscription — emit ActivityRefresh (DATA-07, VIS-12).
		// Parse the payload as a unix-second int64. Drop on empty or invalid
		// (format push unsupported): the supervisor's %output-based fallback
		// handles the signal coarsely.
		_ = paneID // activity is session-scoped, not pane-scoped
		tsStr := strings.TrimSpace(string(DecodeOctal([]byte(payload))))
		if tsStr == "" {
			return nil // empty payload — silently drop
		}
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			return nil // non-integer payload — silently drop
		}
		return ActivityRefresh{Session: sessID, ActivityTS: ts}

	default:
		// Foreign subscription — not ours, ignore.
		return nil
	}
}

// parseBeginLine returns the command number from a `%begin <ts> <num> <flags>`
// line. Returns false if the line is not a %begin or is malformed.
func parseBeginLine(line []byte) (int64, bool) {
	if !bytes.HasPrefix(line, []byte("%begin ")) {
		return 0, false
	}
	fields := strings.Fields(string(line))
	// %begin <ts> <num> [<flags>...]
	if len(fields) < 3 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseEndOrErrorLine returns the command number from %end or %error lines
// plus a boolean discriminator: isError=true when the line was %error,
// false when it was %end. ok=false when the line is neither or malformed.
//
// Format: `%end <ts> <num> [<flags>...]` (or `%error <ts> <num> ...`) —
// the command number is at field index 1 of the post-prefix tokens.
//
// Callers MUST distinguish %end from %error: %end means the command
// succeeded and the accumulated block body is valid response data, while
// %error means the command failed and the body is a (possibly partial)
// error message that should NOT be parsed as a successful response.
// Staff-review PR #3 — Subprocess C1.
func parseEndOrErrorLine(line []byte) (num int64, isError, ok bool) {
	var prefix []byte
	switch {
	case bytes.HasPrefix(line, []byte("%end ")):
		prefix = []byte("%end ")
		isError = false
	case bytes.HasPrefix(line, []byte("%error ")):
		prefix = []byte("%error ")
		isError = true
	default:
		return 0, false, false
	}
	fields := strings.Fields(string(line[len(prefix):]))
	// <ts> <num> [<flags>...]
	if len(fields) < 2 {
		return 0, isError, false
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, isError, false
	}
	return n, isError, true
}

// splitTwo splits "X Y" into ("X", "Y", true); single-token returns false.
func splitTwo(s string) (a, b string, ok bool) {
	sp := strings.IndexByte(s, ' ')
	if sp == -1 {
		return "", "", false
	}
	return s[:sp], s[sp+1:], true
}

// cloneBytes copies a slice that bufio.Scanner may reuse across Scan() calls.
func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
