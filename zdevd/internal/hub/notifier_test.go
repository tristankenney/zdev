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
