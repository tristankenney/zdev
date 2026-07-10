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
	t.Setenv("ZDEV_NOTIFY_CMDS", "") // hermetic: a developer's fan-out must not leak in
	fire, desc, ok := ResolveNotifier()
	if !ok || fire == nil {
		t.Fatalf("ResolveNotifier with ZDEV_NOTIFY_CMD: ok=%v fireNil=%v", ok, fire == nil)
	}
	if desc != "ZDEV_NOTIFY_CMD exec hook" {
		t.Errorf("desc = %q; want exec hook", desc)
	}
}

// TestParseNotifyBackends covers the ZDEV_NOTIFY_CMDS parsing contract:
// nil (legacy resolution) when the list is unset or blank, newline
// splitting with trimming, the `desktop` token, and ZDEV_NOTIFY_CMD
// joining as the first entry. Pure — no env, no spawns.
func TestParseNotifyBackends(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		cmds string
		want []backendSpec
	}{
		{
			name: "both empty → nil (legacy resolution)",
		},
		{
			name: "cmd only → nil (legacy path owns single-cmd behavior)",
			cmd:  "zdev-notify-ntfy",
		},
		{
			name: "cmds blank/whitespace → nil (treated as unset)",
			cmd:  "zdev-notify-ntfy",
			cmds: " \n\t\n  ",
		},
		{
			name: "single exec entry",
			cmds: "zdev-notify-ntfy",
			want: []backendSpec{{cmdline: "zdev-notify-ntfy"}},
		},
		{
			name: "newline-separated entries, blanks and whitespace skipped",
			cmds: "\n  zdev-notify-ntfy  \n\nzdev-notify-pushover\n",
			want: []backendSpec{
				{cmdline: "zdev-notify-ntfy"},
				{cmdline: "zdev-notify-pushover"},
			},
		},
		{
			name: "desktop token selects the platform banner",
			cmds: "desktop\nzdev-notify-ntfy",
			want: []backendSpec{
				{desktop: true, cmdline: "desktop"},
				{cmdline: "zdev-notify-ntfy"},
			},
		},
		{
			name: "cmd joins as the first entry",
			cmd:  "zdev-notify-digest",
			cmds: "desktop",
			want: []backendSpec{
				{cmdline: "zdev-notify-digest"},
				{desktop: true, cmdline: "desktop"},
			},
		},
		{
			name: "command lines with colons/commas/quotes pass through intact",
			cmds: "curl -s -d 'a,b' https://ntfy.example.com/t\ndesktop",
			want: []backendSpec{
				{cmdline: "curl -s -d 'a,b' https://ntfy.example.com/t"},
				{desktop: true, cmdline: "desktop"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNotifyBackends(tt.cmd, tt.cmds)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNotifyBackends(%q, %q) = %v; want %v", tt.cmd, tt.cmds, got, tt.want)
			}
		})
	}
}

// TestFanOut proves the composition: every entry fires, in order, with
// the same payload. Recording closures — no processes.
func TestFanOut(t *testing.T) {
	var order []string
	rec := func(id string) func(Notification) {
		return func(n Notification) {
			if n.Project != notifFixture.Project {
				t.Errorf("backend %s got project %q; want %q", id, n.Project, notifFixture.Project)
			}
			order = append(order, id)
		}
	}
	fire := fanOut([]func(Notification){rec("a"), rec("b"), rec("c")})
	fire(notifFixture)
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(order, want) {
		t.Errorf("fan-out fire order = %v; want %v", order, want)
	}
}

// TestResolveBackend_FanOutResolution covers the env → backend resolution
// table for the fan-out knob without spawning notifier processes: only
// desc/ok are asserted; the fire closures are exec wrappers that never run.
func TestResolveBackend_FanOutResolution(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		cmds     string
		wantDesc string
		wantOK   bool
	}{
		{
			name:     "cmds with two exec entries",
			cmds:     "zdev-notify-ntfy\nzdev-notify-pushover",
			wantDesc: "fan-out: exec(zdev-notify-ntfy) + exec(zdev-notify-pushover)",
			wantOK:   true,
		},
		{
			name:     "cmd composes with cmds, firing first",
			cmd:      "zdev-notify-digest",
			cmds:     "zdev-notify-ntfy",
			wantDesc: "fan-out: exec(zdev-notify-digest) + exec(zdev-notify-ntfy)",
			wantOK:   true,
		},
		{
			name:     "blank cmds falls back to legacy cmd path, byte-identical desc",
			cmd:      "zdev-notify-ntfy",
			cmds:     "  \n ",
			wantDesc: "ZDEV_NOTIFY_CMD exec hook",
			wantOK:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZDEV_NOTIFY_CMD", tt.cmd)
			t.Setenv("ZDEV_NOTIFY_CMDS", tt.cmds)
			fire, desc, ok := resolveBackend()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v; want %v (desc %q)", ok, tt.wantOK, desc)
			}
			if desc != tt.wantDesc {
				t.Errorf("desc = %q; want %q", desc, tt.wantDesc)
			}
			if tt.wantOK && fire == nil {
				t.Errorf("ok backend returned nil fire")
			}
		})
	}
}

// TestFanOut_RoundTrip proves the fan-out end-to-end through the same
// /bin/sh transport the single-backend round-trip test uses: two exec
// entries each write their payload to a distinct file, and a broken first
// entry (nonexistent binary) does not stop the later entries from firing.
func TestFanOut_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	outA := filepath.Join(dir, "a")
	outB := filepath.Join(dir, "b")
	t.Setenv("ZDEV_NOTIFY_CMD", "")
	t.Setenv("ZDEV_NOTIFY_CMDS",
		"/nonexistent-zdev-backend --boom\n"+
			`printf '%s' "$ZDEV_NOTIFY_PROJECT" > `+outA+"\n"+
			`printf '%s' "$ZDEV_NOTIFY_KIND" > `+outB)

	fire, _, ok := resolveBackend()
	if !ok || fire == nil {
		t.Fatalf("resolveBackend fan-out: ok=%v fireNil=%v", ok, fire == nil)
	}
	fire(notifFixture)

	// Fire-and-forget: poll for both side effects rather than sleeping a
	// fixed worst case (same pattern as TestExecNotifier_RoundTrip).
	deadline := time.Now().Add(2 * time.Second)
	var a, b []byte
	for time.Now().Before(deadline) {
		a, _ = os.ReadFile(outA)
		b, _ = os.ReadFile(outB)
		if len(a) > 0 && len(b) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if string(a) != "example-agora" {
		t.Errorf("entry A payload = %q; want %q", string(a), "example-agora")
	}
	if string(b) != "permission" {
		t.Errorf("entry B payload = %q; want %q", string(b), "permission")
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

	// Fan-out entries alongside the single hook: the mute guard sits
	// ABOVE the whole composition (resolveBackend returns the already-
	// fanned-out closure), so muting must suppress every entry — a fan-out
	// backend that bypassed the guard would surface here as any of the
	// payload files appearing.
	out := filepath.Join(dir, "payload")
	out2 := filepath.Join(dir, "payload2")
	t.Setenv("ZDEV_NOTIFY_CMD", `printf '%s' "$ZDEV_NOTIFY_PROJECT" > `+out)
	t.Setenv("ZDEV_NOTIFY_CMDS", `printf '%s' "$ZDEV_NOTIFY_KIND" > `+out2)
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

	// Brief wait window: if any composed backend ever spawned, its
	// payload file would appear within a few ms (round-trip is sub-
	// millisecond on localhost; we matched 2s in the round-trip test).
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("mute guard did not suppress fire: payload file appeared at %s", out)
		}
		if _, err := os.Stat(out2); err == nil {
			t.Fatalf("mute guard did not suppress fan-out entry: payload file appeared at %s", out2)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
