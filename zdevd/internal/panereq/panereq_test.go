package panereq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reqNow = int64(1_700_000_000)

func TestOpenReadClose(t *testing.T) {
	dir := Dir(t.TempDir())

	stream, err := Open(dir, "api", "running tests", reqNow)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := filepath.Join(dir, "api.stream"); stream != want {
		t.Errorf("stream = %q, want %q", stream, want)
	}
	// Open must create the stream so `tail -F` has something to attach to.
	if _, err := os.Stat(stream); err != nil {
		t.Errorf("stream not created: %v", err)
	}

	r, ok := Read(dir, "api")
	if !ok {
		t.Fatal("Read found no request")
	}
	if r.Session != "api" || r.Title != "running tests" || r.Kind != KindStream {
		t.Errorf("unexpected request %+v", r)
	}
	if r.Stream != stream {
		t.Errorf("Stream = %q, want %q", r.Stream, stream)
	}

	if err := Close(dir, "api"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := Read(dir, "api"); ok {
		t.Error("request survived Close")
	}
	// Idempotent.
	if err := Close(dir, "api"); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// One file per session IS the one-pane-per-agent cap.
func TestOpenIsCappedAtOnePerSession(t *testing.T) {
	dir := Dir(t.TempDir())
	for _, title := range []string{"first", "second", "third"} {
		if _, err := Open(dir, "api", title, reqNow); err != nil {
			t.Fatalf("Open %s: %v", title, err)
		}
	}
	reqs, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	if reqs[0].Title != "third" {
		t.Errorf("last write should win, got %q", reqs[0].Title)
	}
}

// The stream path is DERIVED, never supplied. A session name that tries to
// escape the directory must not.
func TestStreamPathCannotEscape(t *testing.T) {
	dir := Dir(t.TempDir())
	for _, session := range []string{
		"../../etc/passwd",
		"..",
		"../..",
		"a/b/c",
		"/etc/shadow",
		"....//....//x",
	} {
		got := StreamPath(dir, session)
		// The security property is containment: the result must be a single
		// component directly inside dir. A literal ".." INSIDE a filename
		// (api_.._x.stream) is inert — only a bare "." or ".." component
		// traverses.
		if filepath.Dir(got) != dir {
			t.Errorf("session %q escaped: %q", session, got)
		}
		base := filepath.Base(got)
		if base == "." || base == ".." || strings.ContainsRune(base, filepath.Separator) {
			t.Errorf("session %q produced a traversing component: %q", session, base)
		}
		if got != filepath.Clean(got) {
			t.Errorf("session %q produced an uncleaned path: %q", session, got)
		}
	}
}

// safeSession must never emit a bare "." or ".." component, whatever it is fed.
func TestSafeSessionNeverYieldsDotComponents(t *testing.T) {
	for _, in := range []string{".", "..", "...", "./.", "../", "", "/", "//", "....."} {
		got := safeSession(in)
		if got == "." || got == ".." || got == "" || strings.ContainsRune(got, filepath.Separator) {
			t.Errorf("safeSession(%q) = %q — unsafe component", in, got)
		}
	}
}

func TestOpenWithHostileSessionStaysInDir(t *testing.T) {
	dir := Dir(t.TempDir())
	stream, err := Open(dir, "../../../tmp/evil", "x", reqNow)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if filepath.Dir(stream) != dir {
		t.Fatalf("hostile session escaped the request dir: %q", stream)
	}
}

// A malformed, oversized, or wrong-kind request is a benign absence.
func TestReadRejectsUnusableRequests(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(session, body string) {
		if err := os.WriteFile(filepath.Join(dir, session+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("bad-json", "{not json")
	write("wrong-kind", `{"kind":"exec","title":"x"}`)
	write("no-kind", `{"title":"x"}`)
	write("huge", `{"kind":"stream","title":"`+strings.Repeat("A", maxReqBytes*2)+`"}`)
	write("good", `{"kind":"stream","title":"ok"}`)

	for _, s := range []string{"bad-json", "wrong-kind", "no-kind", "huge"} {
		if _, ok := Read(dir, s); ok {
			t.Errorf("%s should have been rejected", s)
		}
	}
	if r, ok := Read(dir, "good"); !ok || r.Title != "ok" {
		t.Errorf("good request rejected: %+v ok=%v", r, ok)
	}
}

func TestReadAllIsSortedAndSkipsJunk(t *testing.T) {
	dir := Dir(t.TempDir())
	for _, s := range []string{"web", "api", "infra"} {
		if _, err := Open(dir, s, s+" work", reqNow); err != nil {
			t.Fatal(err)
		}
	}
	// Stream files and stray entries must not be mistaken for requests.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	reqs, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var got []string
	for _, r := range reqs {
		got = append(got, r.Session)
	}
	want := []string{"api", "infra", "web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadAll = %v, want %v", got, want)
	}
}

func TestReadAllMissingDirIsNotAnError(t *testing.T) {
	reqs, err := ReadAll(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("missing dir should be benign, got %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("want no requests, got %d", len(reqs))
	}
}

func TestCloseAll(t *testing.T) {
	dir := Dir(t.TempDir())
	for _, s := range []string{"a", "b", "c"} {
		if _, err := Open(dir, s, "x", reqNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := CloseAll(dir); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	reqs, _ := ReadAll(dir)
	if len(reqs) != 0 {
		t.Errorf("CloseAll left %d requests", len(reqs))
	}
}

// The title lands on a pane border, so control bytes must never survive.
func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"running tests", "running tests"},
		{"", ""},
		{"  padded   out  ", "padded out"},
		{"two\nlines", "two lines"},
		{"tab\there", "tab here"},
		{"esc\x1b[31mred", "esc[31mred"},
		{"bell\x07", "bell"},
		{"nul\x00byte", "nulbyte"},
		{"carriage\rreturn", "carriage return"},
	}
	for _, c := range cases {
		if got := SanitizeTitle(c.in); got != c.want {
			t.Errorf("SanitizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	long := SanitizeTitle(strings.Repeat("x", MaxTitle*3))
	if len([]rune(long)) > MaxTitle+1 { // +1 for the ellipsis
		t.Errorf("long title not capped: %d runes", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("capped title should be elided, got %q", long)
	}

	// Multi-byte input must not be cut mid-rune.
	wide := SanitizeTitle(strings.Repeat("日", MaxTitle))
	if !isValidUTF8(wide) {
		t.Errorf("capped multi-byte title is not valid UTF-8: %q", wide)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestFilePermissions(t *testing.T) {
	dir := Dir(t.TempDir())
	stream, err := Open(dir, "api", "x", reqNow)
	if err != nil {
		t.Fatal(err)
	}
	// The request dir is a same-uid control channel; group/world must be shut
	// out (doctor flags 0644 notify tokens for exactly this reason).
	for _, p := range []string{dir, stream, filepath.Join(dir, "api.json")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		perm := fi.Mode().Perm()
		if fi.IsDir() {
			if perm != 0o700 {
				t.Errorf("%s mode = %o, want 700", p, perm)
			}
			continue
		}
		if perm&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group/world bits", p, perm)
		}
	}
}
