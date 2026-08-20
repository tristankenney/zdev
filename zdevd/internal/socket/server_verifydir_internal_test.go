package socket

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifySocketDir exercises the M7 bind-time parent-dir enforcement: the
// socket's parent must be a real directory owned by the current uid with mode
// exactly 0700. Table-driven with a temp dir the test chmods per case. The
// wrong-owner branch needs a second uid (root) to construct, so it is covered
// by inspection of verifySocketDir, not here.
func TestVerifySocketDir(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, base string) string // returns dir to verify
		wantErr bool
	}{
		{
			name: "real dir mode 0700 accepted",
			setup: func(t *testing.T, base string) string {
				d := filepath.Join(base, "good")
				mkdirMode(t, d, 0o700)
				return d
			},
			wantErr: false,
		},
		{
			name: "mode 0755 refused",
			setup: func(t *testing.T, base string) string {
				d := filepath.Join(base, "loose755")
				mkdirMode(t, d, 0o755)
				return d
			},
			wantErr: true,
		},
		{
			name: "mode 0777 refused",
			setup: func(t *testing.T, base string) string {
				d := filepath.Join(base, "loose777")
				mkdirMode(t, d, 0o777)
				return d
			},
			wantErr: true,
		},
		{
			name: "group-readable 0750 refused",
			setup: func(t *testing.T, base string) string {
				d := filepath.Join(base, "loose750")
				mkdirMode(t, d, 0o750)
				return d
			},
			wantErr: true,
		},
		{
			name: "symlink refused even when target is a 0700 dir",
			setup: func(t *testing.T, base string) string {
				target := filepath.Join(base, "realtarget")
				mkdirMode(t, target, 0o700)
				link := filepath.Join(base, "link")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			wantErr: true,
		},
		{
			name: "regular file refused (not a directory)",
			setup: func(t *testing.T, base string) string {
				f := filepath.Join(base, "afile")
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return f
			},
			wantErr: true,
		},
		{
			name: "missing dir refused",
			setup: func(t *testing.T, base string) string {
				return filepath.Join(base, "does-not-exist")
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dir := tc.setup(t, base)
			err := verifySocketDir(dir)
			if tc.wantErr && err == nil {
				t.Fatalf("verifySocketDir(%q) = nil; want error", dir)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifySocketDir(%q) = %v; want nil", dir, err)
			}
		})
	}
}

// TestBindOrCleanStale_RefusesLooseParent proves the enforcement is wired into
// the bind path, not just the helper: BindOrCleanStale must refuse to bind a
// socket under a world-readable parent directory.
func TestBindOrCleanStale_RefusesLooseParent(t *testing.T) {
	base := shortTempSocketDir(t)
	dir := filepath.Join(base, "sockdir")
	mkdirMode(t, dir, 0o755)

	ln, err := BindOrCleanStale(filepath.Join(dir, "zdevd.sock"))
	if err == nil {
		_ = ln.Close()
		t.Fatalf("BindOrCleanStale bound under a 0755 parent; want refusal")
	}
}

// TestBindOrCleanStale_AcceptsTightParent is the positive control: a 0700
// parent binds cleanly.
func TestBindOrCleanStale_AcceptsTightParent(t *testing.T) {
	base := shortTempSocketDir(t)
	dir := filepath.Join(base, "sockdir")
	mkdirMode(t, dir, 0o700)

	ln, err := BindOrCleanStale(filepath.Join(dir, "zdevd.sock"))
	if err != nil {
		t.Fatalf("BindOrCleanStale under a 0700 parent: %v", err)
	}
	_ = ln.Close()
}

// mkdirMode creates dir and forces its mode past the process umask.
func mkdirMode(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}
