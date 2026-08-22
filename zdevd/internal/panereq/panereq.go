// Package panereq is the agent → daemon request channel for turn-scoped
// panes: Claude asks for a viewport, the daemon decides whether it appears.
//
// # Why a file channel
//
// Same shape as internal/notif, and for the same reason: the writer is an
// agent-side process (an MCP tool call, or `zdevd pane open` from a hook) that
// must not need a socket handshake, and the daemon must not need a new hub
// request path. One file per session IS the one-pane-per-agent cap — the cap is
// structural, not a counter that can drift.
//
// # Security posture
//
// Inherited wholesale from internal/notif: a same-uid control channel with no
// authentication, so every field is treated as hostile.
//
//	the session is derived from the FILENAME, never read from the content —
//	  an agent cannot request a pane in someone else's window;
//	the stream path is DERIVED, never supplied — otherwise `zdev pane open`
//	  would make the daemon tail an arbitrary path (/etc/shadow) into a
//	  visible pane, which is the whole exploit;
//	the title is sanitized to one short line — it lands on a pane border;
//	reads are bounded (a legitimate request is a few hundred bytes);
//	a future-dated or ancient ts is clamped, not trusted.
//
// Nothing here grants the agent a capability it lacks. It already has bash; a
// pane is a different DISPLAY for output it can already produce, and it names
// no command — it writes to the stream file and the daemon tails it.
package panereq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// SubdirName is the private subdirectory under $TMPDIR holding pane
	// requests. Exported so the daemon, the CLI and the MCP tool agree.
	SubdirName = "zdevd-panes"

	reqSuffix    = ".json"
	streamSuffix = ".stream"

	// maxReqBytes bounds one request read. A legitimate request is a title
	// and a timestamp; 8 KiB is generous headroom while stopping a runaway
	// writer from pointing the daemon at a huge file.
	maxReqBytes = 8 * 1024

	// MaxTitle caps the sanitized title. It renders on a pane border next to
	// the agent glyph, so anything longer is already unreadable.
	MaxTitle = 48

	// KindStream is the only kind v1 accepts: a viewport onto the stream file
	// the agent writes. No kind names a command, deliberately.
	KindStream = "stream"
)

// Request is one agent's standing pane request. Session and Stream are filled
// by the reader from the filename, never from the file body.
type Request struct {
	Session string `json:"-"`
	Stream  string `json:"-"`

	Kind  string `json:"kind"`
	Title string `json:"title"`
	TS    int64  `json:"ts"`
}

// Dir returns the request directory: parent/SubdirName. Production callers
// pass $TMPDIR; tests pass t.TempDir().
func Dir(parent string) string { return filepath.Join(parent, SubdirName) }

// StreamPath returns the ONLY path a pane for this session may tail. Derived,
// never supplied — see the package security note.
func StreamPath(dir, session string) string {
	return filepath.Join(dir, safeSession(session)+streamSuffix)
}

func reqPath(dir, session string) string {
	return filepath.Join(dir, safeSession(session)+reqSuffix)
}

// safeSession reduces a session name to something that cannot escape dir or
// collide with the suffixes. tmux allows almost anything in a session name,
// including '/' and '..'.
func safeSession(session string) string {
	var b strings.Builder
	for _, r := range session {
		switch {
		case r == '-' || r == '_' || r == '.':
			// '.' is allowed inside but a leading run of dots is stripped
			// below, so "../.." cannot survive.
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if out == "" {
		return "_"
	}
	return out
}

// Open records a pane request for session and returns the stream path the
// agent should write to. Replaces any existing request — one per agent.
//
// nowUnix is threaded so the writer's clock is explicit at the call site.
func Open(dir, session, title string, nowUnix int64) (string, error) {
	if strings.TrimSpace(session) == "" {
		return "", errors.New("panereq: empty session")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("panereq: mkdir: %w", err)
	}
	stream := StreamPath(dir, session)
	// Create (do not truncate) the stream so `tail -F` has something to open
	// immediately; a re-open of an existing pane keeps prior output.
	f, err := os.OpenFile(stream, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("panereq: stream: %w", err)
	}
	_ = f.Close()

	body, err := json.Marshal(Request{
		Kind:  KindStream,
		Title: SanitizeTitle(title),
		TS:    nowUnix,
	})
	if err != nil {
		return "", err
	}
	// Write-then-rename so the daemon never reads a half-written request.
	tmp := reqPath(dir, session) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("panereq: write: %w", err)
	}
	if err := os.Rename(tmp, reqPath(dir, session)); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("panereq: rename: %w", err)
	}
	return stream, nil
}

// Close removes session's request. The pane goes on the daemon's next
// reconcile. Absent request is not an error — Close is idempotent.
func Close(dir, session string) error {
	if err := os.Remove(reqPath(dir, session)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CloseAll removes every request. Used on daemon shutdown so a restart never
// resurrects panes for turns that ended while it was down.
func CloseAll(dir string) error {
	reqs, err := ReadAll(dir)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if err := Close(dir, r.Session); err != nil {
			return err
		}
	}
	return nil
}

// Read returns session's request, or (_, false) when there is none or it is
// unusable. A malformed request is a benign absence, never an error the daemon
// acts on — the writer is untrusted.
func Read(dir, session string) (Request, bool) {
	return readFile(dir, reqPath(dir, session), safeSession(session))
}

// ReadAll returns every well-formed request in dir, sorted by session for a
// deterministic plan.
func ReadAll(dir string) ([]Request, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Request
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, reqSuffix) {
			continue
		}
		session := strings.TrimSuffix(name, reqSuffix)
		if r, ok := readFile(dir, filepath.Join(dir, name), session); ok {
			out = append(out, r)
		}
	}
	// Deterministic order; session names are already filename-safe.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Session < out[j-1].Session; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func readFile(dir, path, session string) (Request, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Request{}, false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxReqBytes))
	if err != nil {
		return Request{}, false
	}
	var r Request
	if err := json.Unmarshal(body, &r); err != nil {
		return Request{}, false
	}
	if r.Kind != KindStream {
		return Request{}, false
	}
	// Session and stream come from the filesystem, never the body.
	r.Session = session
	r.Stream = StreamPath(dir, session)
	r.Title = SanitizeTitle(r.Title)
	return r, true
}

// SanitizeTitle reduces an agent-authored title to one short printable line.
// It renders on a pane border, so control bytes could otherwise repaint the
// operator's screen (the same class of hazard render/sanitize.go guards on the
// wire), and a newline would break the border outright.
func SanitizeTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Drop control bytes outright.
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > MaxTitle {
		// Trim on a rune boundary.
		for len(out) > MaxTitle {
			_, size := utf8DecodeLast(out)
			out = out[:len(out)-size]
		}
		out = strings.TrimSpace(out) + "…"
	}
	return out
}

func utf8DecodeLast(s string) (rune, int) {
	for i := len(s) - 1; i >= 0 && i > len(s)-5; i-- {
		if r := []rune(s[i:]); len(r) == 1 {
			return r[0], len(s) - i
		}
	}
	return 0, 1
}
