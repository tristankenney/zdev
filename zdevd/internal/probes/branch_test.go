package probes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestBranchProbe_Class(t *testing.T) {
	p := NewBranchProbe(func(tmuxctl.Event) {}, "/ws")
	if p.Class() != "branch" {
		t.Errorf("Class() = %q; want %q", p.Class(), "branch")
	}
}

func TestDetectVCS(t *testing.T) {
	tmp := t.TempDir()
	sl := filepath.Join(tmp, "alpha")
	git := filepath.Join(tmp, "beta")
	none := filepath.Join(tmp, "gamma")
	for _, d := range []string{sl, git, none} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(sl, ".sl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(git, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := NewBranchProbe(func(tmuxctl.Event) {}, tmp)
	if got := p.detectVCS(sl, "alpha"); got != "sl" {
		t.Errorf("detectVCS(.sl) = %q; want sl", got)
	}
	if got := p.detectVCS(git, "beta"); got != "git" {
		t.Errorf("detectVCS(.git) = %q; want git", got)
	}
	if got := p.detectVCS(none, "gamma"); got != "" {
		t.Errorf("detectVCS(neither) = %q; want \"\"", got)
	}
}

func TestBranchProbe_SaplingPath(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "alpha")
	os.MkdirAll(filepath.Join(proj, ".sl"), 0o755)

	var got tmuxctl.Event
	p := NewBranchProbe(func(ev tmuxctl.Event) { got = ev }, tmp)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if name == "sl" && len(args) > 0 && args[0] == "bookmark" {
			return []byte(" * feature-x        42:abc123\n   other            41:def\n"), nil
		}
		if name == "sl" && len(args) > 1 && args[0] == "status" {
			return []byte("M src/main.go\nA new.go\n? untracked.go\n"), nil
		}
		return nil, errors.New("unexpected exec")
	}
	if err := p.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}

	dr, ok := got.(tmuxctl.DataRefresh)
	if !ok {
		t.Fatalf("got = %T; want DataRefresh", got)
	}
	if dr.Branch != "feature-x" {
		t.Errorf("Branch = %q; want feature-x", dr.Branch)
	}
	if dr.DirtyCount != 2 {
		t.Errorf("DirtyCount = %d; want 2 (M+A only)", dr.DirtyCount)
	}
}

func TestBranchProbe_GitPath_DefaultBranchSuppressed(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "beta")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)

	var got tmuxctl.Event
	p := NewBranchProbe(func(ev tmuxctl.Event) { got = ev }, tmp)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "git" && strings.Contains(joined, "rev-parse") {
			return []byte("develop\n"), nil
		}
		if name == "git" && strings.Contains(joined, "status") {
			return []byte(" M file1.go\n?? file2.go\nMM file3.go\n"), nil
		}
		return nil, errors.New("unexpected exec")
	}
	if err := p.Refresh(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}

	dr, ok := got.(tmuxctl.DataRefresh)
	if !ok {
		t.Fatalf("got = %T; want DataRefresh", got)
	}
	if dr.Branch != "" {
		t.Errorf("Branch = %q; want \"\" (default-branch suppression)", dr.Branch)
	}
	if dr.DirtyCount != 3 {
		t.Errorf("DirtyCount = %d; want 3", dr.DirtyCount)
	}
}

func TestBranchProbe_GitPath_NonDefaultBranch(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "gamma")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)

	var got tmuxctl.Event
	p := NewBranchProbe(func(ev tmuxctl.Event) { got = ev }, tmp)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "git" && strings.Contains(joined, "rev-parse --abbrev-ref HEAD") {
			return []byte("feature-x\n"), nil
		}
		if name == "git" && strings.Contains(joined, "status --porcelain") {
			return []byte(""), nil
		}
		return nil, errors.New("unexpected")
	}
	if err := p.Refresh(context.Background(), "gamma"); err != nil {
		t.Fatal(err)
	}

	dr, ok := got.(tmuxctl.DataRefresh)
	if !ok {
		t.Fatalf("got = %T; want DataRefresh", got)
	}
	if dr.Branch != "feature-x" {
		t.Errorf("Branch = %q; want feature-x", dr.Branch)
	}
	if dr.DirtyCount != 0 {
		t.Errorf("DirtyCount = %d; want 0 (clean tree)", dr.DirtyCount)
	}
}

func TestBranchProbe_NoVCS(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "alpha"), 0o755)

	var calls int64
	submit := func(tmuxctl.Event) { atomic.AddInt64(&calls, 1) }
	p := NewBranchProbe(submit, tmp)
	if err := p.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("calls = %d; want 0 (no VCS)", calls)
	}
}

func TestBranchProbe_VCSCacheStable(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "alpha")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)

	var stats int64
	p := NewBranchProbe(func(tmuxctl.Event) {}, tmp)
	p.statFunc = func(path string) (os.FileInfo, error) {
		atomic.AddInt64(&stats, 1)
		return os.Stat(path)
	}
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("feature-x\n"), nil
	}

	p.detectVCS(proj, "alpha")
	p.detectVCS(proj, "alpha")
	p.detectVCS(proj, "alpha")
	// First call stats up to 3 paths (.jj first, miss; .sl, miss; .git, hit).
	// Subsequent calls hit the cache → no new stats.
	if got := atomic.LoadInt64(&stats); got > 3 {
		t.Errorf("stats = %d; want <= 3 (cache should suppress repeat stats)", got)
	}
}

// TestDetectVCS_JJBeatsColocatedGit pins the colocated-repo rule: a .jj
// directory wins over the .git beside it — under git the jj working copy
// is a detached HEAD, so the git path would report the branch "HEAD".
func TestDetectVCS_JJBeatsColocatedGit(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "alpha")
	os.MkdirAll(filepath.Join(proj, ".jj"), 0o755)
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)

	p := NewBranchProbe(func(tmuxctl.Event) {}, tmp)
	if got := p.detectVCS(proj, "alpha"); got != "jj" {
		t.Errorf("detectVCS(colocated) = %q; want jj", got)
	}
}

// TestBranchProbe_RefreshJJ exercises the jj path end-to-end through
// stubbed shellouts: bookmark from the stack-head revset, dirty count
// from the @ diff summary, and --ignore-working-copy on every call.
func TestBranchProbe_RefreshJJ(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "alpha")
	os.MkdirAll(filepath.Join(proj, ".jj"), 0o755)

	var got tmuxctl.DataRefresh
	p := NewBranchProbe(func(e tmuxctl.Event) { got = e.(tmuxctl.DataRefresh) }, tmp)
	p.execFunc = func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		if name != "jj" {
			t.Errorf("unexpected tool %q (args %v)", name, args)
		}
		ignored := false
		for _, a := range args {
			if a == "--ignore-working-copy" {
				ignored = true
			}
		}
		if !ignored {
			t.Errorf("jj %v missing --ignore-working-copy (daemon must never snapshot)", args)
		}
		if args[0] == "log" {
			return []byte("feature/imp-406-test-seeding\n"), nil
		}
		return []byte("M app/Console/Commands/DeliverQaBanners.php\nM database/seeders/OnsiteBannerSeeder.php\n"), nil
	}
	if err := p.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if got.Branch != "feature/imp-406-test-seeding" || got.DirtyCount != 2 {
		t.Errorf("DataRefresh = branch=%q dirty=%d; want feature/imp-406-test-seeding/2", got.Branch, got.DirtyCount)
	}
}

func TestParseJJBookmark(t *testing.T) {
	cases := []struct{ in, want string }{
		{"feature-x\n", "feature-x"},
		{"\nfeature-x other-mark\n", "feature-x"}, // two bookmarks on one commit → first
		{"feature-x*\n", "feature-x"},             // conflicted-bookmark suffix stripped
		{"", ""},
	}
	for _, c := range cases {
		if got := parseJJBookmark([]byte(c.in)); got != c.want {
			t.Errorf("parseJJBookmark(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestCountJJDiffDirty(t *testing.T) {
	in := []byte("M file1\nA file2\nD file3\n\n")
	if got := countJJDiffDirty(in); got != 3 {
		t.Errorf("countJJDiffDirty = %d; want 3", got)
	}
}

func TestParseSaplingBookmark(t *testing.T) {
	in := []byte(` * feature-x        42:abc123
   other            41:def
`)
	if got := parseSaplingBookmark(in); got != "feature-x" {
		t.Errorf("parseSaplingBookmark = %q; want feature-x", got)
	}
}

func TestCountSlStatusDirty(t *testing.T) {
	in := []byte("M file1\nA file2\nR file3\n? file4\n! file5\n")
	if got := countSlStatusDirty(in); got != 3 {
		t.Errorf("countSlStatusDirty = %d; want 3 (M+A+R)", got)
	}
}

func TestCountGitPorcelainDirty(t *testing.T) {
	in := []byte(" M file1\n?? file2\nMM file3\n   ignore-prefix-spaces\n")
	if got := countGitPorcelainDirty(in); got != 3 {
		t.Errorf("countGitPorcelainDirty = %d; want 3", got)
	}
}

// TestBranchProbe_PassesNoOptionalLocks (260512 lock-leak fix) verifies that
// every git invocation includes `--no-optional-locks` so a SIGKILL'd git can
// never leave a stale .git/index.lock file behind. Regression guard.
func TestBranchProbe_PassesNoOptionalLocks(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "delta")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)

	var gitInvocations [][]string
	p := NewBranchProbe(func(tmuxctl.Event) {}, tmp)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if name == "git" {
			gitInvocations = append(gitInvocations, append([]string(nil), args...))
		}
		joined := strings.Join(args, " ")
		if name == "git" && strings.Contains(joined, "rev-parse") {
			return []byte("feature-y\n"), nil
		}
		if name == "git" && strings.Contains(joined, "status") {
			return []byte(""), nil
		}
		return nil, errors.New("unexpected exec")
	}
	if err := p.Refresh(context.Background(), "delta"); err != nil {
		t.Fatal(err)
	}
	if len(gitInvocations) == 0 {
		t.Fatal("no git invocations recorded")
	}
	for i, args := range gitInvocations {
		var seen bool
		for _, a := range args {
			if a == "--no-optional-locks" {
				seen = true
				break
			}
		}
		if !seen {
			t.Errorf("git invocation #%d missing --no-optional-locks: %v", i, args)
		}
	}
}

// TestDetectLocalBranches_PassesNoOptionalLocks: same regression guard for
// the 260513-dpr helper. Stubs the sl backend to fail so the git fallback
// fires; asserts --no-optional-locks is on every git invocation.
func TestDetectLocalBranches_PassesNoOptionalLocks(t *testing.T) {
	var sawFlag bool
	exec := func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if name == "sl" {
			return nil, errors.New("not a sapling repo")
		}
		if name == "git" {
			for _, a := range args {
				if a == "--no-optional-locks" {
					sawFlag = true
				}
			}
			return []byte("main\n"), nil
		}
		return nil, errors.New("unexpected exec")
	}
	branches, detected := detectLocalBranches(context.Background(), exec, "/some/dir")
	if !detected || len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("detectLocalBranches = (%v, %v); want ([main], true)", branches, detected)
	}
	if !sawFlag {
		t.Error("git invocation in detectLocalBranches missing --no-optional-locks")
	}
}
