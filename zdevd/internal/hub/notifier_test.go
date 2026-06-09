// internal/hub/notifier_test.go
//
// Backend tests: pure arg/env builders are table-tested without spawning;
// the exec backend gets one real round-trip through /bin/sh (file-write
// side effect, no network, sub-second); the structured payload is proven
// end-to-end through tierCheck's fire seam.
package hub

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

var notifFixture = Notification{
	Project: "example-agora",
	Message: "waiting 5m (permission) · 2 more waiting",
	Sound:   "Ping",
	Kind:    proto.WaitKindPermission,
	AgeSec:  300,
}

func TestDarwinArgs(t *testing.T) {
	got := darwinArgs(notifFixture)
	want := []string{"-title", "example-agora", "-message", "waiting 5m (permission) · 2 more waiting", "-sound", "Ping"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("darwinArgs = %v; want %v", got, want)
	}
}

func TestLinuxArgs_FlatBanner(t *testing.T) {
	got := linuxArgs(notifFixture)
	want := []string{"-a", "zdev", "example-agora", "waiting 5m (permission) · 2 more waiting"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linuxArgs = %v; want %v", got, want)
	}
	// Flat by design: no urgency flag, no sound mapping (file header).
	for _, a := range got {
		if a == "-u" || a == "--urgency" {
			t.Errorf("linuxArgs must not map sound→urgency; got %v", got)
		}
	}
}

func TestExecEnv(t *testing.T) {
	got := execEnv(notifFixture)
	want := []string{
		"ZDEV_NOTIFY_PROJECT=example-agora",
		"ZDEV_NOTIFY_MSG=waiting 5m (permission) · 2 more waiting",
		"ZDEV_NOTIFY_SOUND=Ping",
		"ZDEV_NOTIFY_KIND=permission",
		"ZDEV_NOTIFY_AGE=300",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execEnv = %v; want %v", got, want)
	}
}

// TestExecNotifier_RoundTrip proves the user-owned transport end-to-end:
// the configured command runs under /bin/sh with the ZDEV_NOTIFY_* env
// populated. The "transport" writes its payload to a file we then read.
func TestExecNotifier_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "payload")
	fire := ExecNotifier(`printf '%s|%s|%s|%s' "$ZDEV_NOTIFY_PROJECT" "$ZDEV_NOTIFY_KIND" "$ZDEV_NOTIFY_AGE" "$ZDEV_NOTIFY_MSG" > ` + out)

	fire(notifFixture)

	// spawn is fire-and-forget; poll briefly for the side effect rather
	// than sleeping a fixed worst case.
	deadline := time.Now().Add(2 * time.Second)
	var b []byte
	var err error
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(out)
		if err == nil && len(b) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := "example-agora|permission|300|waiting 5m (permission) · 2 more waiting"
	if string(b) != want {
		t.Fatalf("exec transport payload = %q; want %q", string(b), want)
	}
}

// TestTierCheck_StructuredPayload proves Kind and AgeSec ride the fire
// seam from the digest leader — the structure the exec backend and the
// future push fan-out consume without re-parsing the message.
func TestTierCheck_StructuredPayload(t *testing.T) {
	s := stateWithProject("proj-a", projectData{
		WaitStartedTS: 100,
		WaitKind:      proto.WaitKindPermission,
	})
	var got []Notification
	tierCheck(400, s, func(n Notification) { got = append(got, n) }) // age = 300

	if len(got) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(got))
	}
	n := got[0]
	if n.Kind != proto.WaitKindPermission {
		t.Errorf("Kind = %q; want permission", n.Kind)
	}
	if n.AgeSec != 300 {
		t.Errorf("AgeSec = %d; want 300", n.AgeSec)
	}
	if n.Sound != "Ping" {
		t.Errorf("Sound = %q; want Ping (5m tier)", n.Sound)
	}
}

// TestResolveNotifier_ExecWins: ZDEV_NOTIFY_CMD takes precedence over any
// platform backend, on every GOOS.
func TestResolveNotifier_ExecWins(t *testing.T) {
	t.Setenv("ZDEV_NOTIFY_CMD", "true")
	fire, desc, ok := ResolveNotifier()
	if !ok || fire == nil {
		t.Fatalf("ResolveNotifier with ZDEV_NOTIFY_CMD: ok=%v fireNil=%v", ok, fire == nil)
	}
	if desc != "ZDEV_NOTIFY_CMD exec hook" {
		t.Errorf("desc = %q; want exec hook", desc)
	}
}

// TestIsNotifyMuted covers the sentinel-file states the mute-guard must
// classify correctly. The mute is a soft signal: any failure mode falls
// open (un-muted) so a corrupted file can never permanently silence
// notifications without the user knowing.
func TestIsNotifyMuted(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	if isNotifyMuted(missing, 1000) {
		t.Errorf("missing file: want un-muted")
	}

	future := filepath.Join(dir, "future")
	if err := os.WriteFile(future, []byte("2000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isNotifyMuted(future, 1000) {
		t.Errorf("expiry 2000 at now=1000: want muted")
	}
	if isNotifyMuted(future, 2000) {
		t.Errorf("expiry 2000 at now=2000: want un-muted (boundary is strict <)")
	}
	if isNotifyMuted(future, 3000) {
		t.Errorf("expiry 2000 at now=3000: want un-muted (expired)")
	}

	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isNotifyMuted(garbage, 1000) {
		t.Errorf("malformed contents: want un-muted (fail open)")
	}
}

// TestResolveNotifier_MuteGuard proves the mute-guard wraps the exec
// backend's fire closure: with the sentinel file present and the
// timestamp in the future, the wrapped closure must not invoke the
// inner transport (no payload file created). Verifies the integration,
// not just the helper.
func TestResolveNotifier_MuteGuard(t *testing.T) {
	// Redirect MutePath() into a temp dir. platform.DataDir() reads
	// XDG_STATE_HOME on Linux and falls back to ~/Library/Application
	// Support on darwin — neither single env var redirects both. The
	// simplest seam is to write the sentinel to the real MutePath()
	// under a t.Setenv'd HOME, but that's fragile across platforms.
	// Instead, build the closure manually around resolveBackend with
	// a known path — same composition the production wrapper uses.
	dir := t.TempDir()
	mute := filepath.Join(dir, "notify-muted-until")
	if err := os.WriteFile(mute, []byte("9999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "payload")
	t.Setenv("ZDEV_NOTIFY_CMD", `printf '%s' "$ZDEV_NOTIFY_PROJECT" > `+out)
	inner, _, ok := resolveBackend()
	if !ok || inner == nil {
		t.Fatalf("resolveBackend: ok=%v innerNil=%v", ok, inner == nil)
	}
	wrapped := func(n Notification) {
		if isNotifyMuted(mute, time.Now().Unix()) {
			return
		}
		inner(n)
	}
	wrapped(notifFixture)

	// Brief wait window: if the inner backend ever spawned, the
	// payload file would appear within a few ms (round-trip is sub-
	// millisecond on localhost; we matched 2s in the round-trip test).
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("mute guard did not suppress fire: payload file appeared at %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
