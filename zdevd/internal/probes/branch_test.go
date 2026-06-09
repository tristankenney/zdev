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

// TestSetDirOverride_RefreshUsesOverride verifies SetDirOverride redirects
// Refresh to a dir outside the workspace tree (zd-bub: unmanaged-session
// branch attribution). Without an override, the project key "gt-rig-name"
// would resolve to a non-existent `workspace/gt-rig-name` and yield no VCS.
func TestSetDirOverride_RefreshUsesOverride(t *testing.T) {
	workspace := t.TempDir()
	// Override target lives OUTSIDE workspace — simulates a polecat sandbox.
	override := t.TempDir()
	if err := os.Mkdir(filepath.Join(override, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	var execDir string
	var got tmuxctl.Event
	p := NewBranchProbe(func(ev tmuxctl.Event) { got = ev }, workspace)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		execDir = dir
		joined := strings.Join(args, " ")
		if name == "git" && strings.Contains(joined, "rev-parse") {
			return []byte("feature-x\n"), nil
		}
		if name == "git" && strings.Contains(joined, "status") {
			return []byte(""), nil
		}
		return nil, errors.New("unexpected exec")
	}

	p.SetDirOverride("gt-rig-name", override)
	if err := p.Refresh(context.Background(), "gt-rig-name"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if execDir != override {
		t.Errorf("Refresh shelled out in %q; want override %q", execDir, override)
	}
	dr, ok := got.(tmuxctl.DataRefresh)
	if !ok {
		t.Fatalf("got = %T; want DataRefresh", got)
	}
	if dr.Branch != "feature-x" {
		t.Errorf("Branch = %q; want feature-x", dr.Branch)
	}
}

// TestSetDirOverride_ChangeInvalidatesVCSCache verifies that switching an
// override to a different dir clears the cached VCS detection for that key.
// Without invalidation, a session that moved from a no-VCS dir to a repo
// (or between VCS types) would keep reporting the prior result forever.
func TestSetDirOverride_ChangeInvalidatesVCSCache(t *testing.T) {
	workspace := t.TempDir()
	noVCS := t.TempDir()
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := NewBranchProbe(func(tmuxctl.Event) {}, workspace)

	p.SetDirOverride("sess", noVCS)
	if got := p.detectVCS(p.dirFor("sess"), "sess"); got != "" {
		t.Errorf("detectVCS over no-VCS dir = %q; want \"\"", got)
	}
	// Cache must hold "" so a second call doesn't re-stat.
	if got := p.detectVCS(p.dirFor("sess"), "sess"); got != "" {
		t.Errorf("cached detectVCS = %q; want \"\"", got)
	}

	p.SetDirOverride("sess", gitDir)
	if got := p.detectVCS(p.dirFor("sess"), "sess"); got != "git" {
		t.Errorf("after override change, detectVCS = %q; want git (cache should have been invalidated)", got)
	}
}

// TestSetDirOverride_EmptyClears verifies passing an empty dir clears the
// override so the key falls back to workspace-joined resolution.
func TestSetDirOverride_EmptyClears(t *testing.T) {
	workspace := t.TempDir()
	override := t.TempDir()

	p := NewBranchProbe(func(tmuxctl.Event) {}, workspace)
	p.SetDirOverride("sess", override)
	if got := p.dirFor("sess"); got != override {
		t.Fatalf("dirFor with override = %q; want %q", got, override)
	}
	p.SetDirOverride("sess", "")
	if got, want := p.dirFor("sess"), filepath.Join(workspace, "sess"); got != want {
		t.Errorf("dirFor after clear = %q; want %q", got, want)
	}
}

// TestBranchProbe_SetDirOverride verifies that SetDirOverride pins an
// unmanaged-session key to a working dir outside the workspace and that
// Refresh resolves VCS + branch at that pinned dir (zd-bub). Without the
// override, the default `workspace/<project>` join would point at no-VCS
// (or worse, the wrong project) and silently return DataRefresh with
// Branch="".
func TestBranchProbe_SetDirOverride(t *testing.T) {
	tmp := t.TempDir()
	override := filepath.Join(tmp, "outside-workspace")
	os.MkdirAll(filepath.Join(override, ".git"), 0o755)

	var got tmuxctl.Event
	// workspace points at tmp; the unmanaged session name 'gt-zdev-obsidian'
	// has no `tmp/gt-zdev-obsidian` directory — without an override Refresh
	// would resolve no VCS and return nil without emitting DataRefresh.
	p := NewBranchProbe(func(ev tmuxctl.Event) { got = ev }, tmp)

	gotDir := ""
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		gotDir = dir
		joined := strings.Join(args, " ")
		if name == "git" && strings.Contains(joined, "rev-parse") {
			return []byte("feature-x\n"), nil
		}
		if name == "git" && strings.Contains(joined, "status") {
			return []byte(" M main.go\n"), nil
		}
		return nil, errors.New("unexpected exec")
	}

	p.SetDirOverride("gt-zdev-obsidian", override)
	if err := p.Refresh(context.Background(), "gt-zdev-obsidian"); err != nil {
		t.Fatal(err)
	}
	if gotDir != override {
		t.Errorf("exec dir = %q; want override %q", gotDir, override)
	}
	dr, ok := got.(tmuxctl.DataRefresh)
	if !ok {
		t.Fatalf("got = %T; want DataRefresh", got)
	}
	if dr.Project != "gt-zdev-obsidian" {
		t.Errorf("DataRefresh.Project = %q; want gt-zdev-obsidian", dr.Project)
	}
	if dr.Branch != "feature-x" {
		t.Errorf("DataRefresh.Branch = %q; want feature-x", dr.Branch)
	}
	if dr.DirtyCount != 1 {
		t.Errorf("DataRefresh.DirtyCount = %d; want 1", dr.DirtyCount)
	}
}

// TestBranchProbe_SetDirOverride_ChangeInvalidatesCache verifies that
// changing the override dir for the same key invalidates the VCS detection
// cache for that key (zd-bub). A polecat starts in $HOME (no VCS), then
// cd's into a repo: the second Refresh must re-detect VCS at the new dir,
// not pin the empty cache entry from the first.
func TestBranchProbe_SetDirOverride_ChangeInvalidatesCache(t *testing.T) {
	tmp := t.TempDir()
	noVCS := filepath.Join(tmp, "home")
	withVCS := filepath.Join(tmp, "repo")
	os.MkdirAll(noVCS, 0o755)
	os.MkdirAll(filepath.Join(withVCS, ".git"), 0o755)

	p := NewBranchProbe(func(ev tmuxctl.Event) {}, tmp)
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "rev-parse") {
			return []byte("main\n"), nil
		}
		return []byte(""), nil
	}

	p.SetDirOverride("polecat", noVCS)
	if err := p.Refresh(context.Background(), "polecat"); err != nil {
		t.Fatal(err)
	}

	// Switch override to a dir with .git — Refresh must re-detect VCS.
	p.SetDirOverride("polecat", withVCS)
	var calls atomic.Int32
	p.execFunc = func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		calls.Add(1)
		if dir != withVCS {
			t.Errorf("exec dir = %q; want %q after override change", dir, withVCS)
		}
		if strings.Contains(strings.Join(args, " "), "rev-parse") {
			return []byte("feature-x\n"), nil
		}
		return []byte(""), nil
	}
	if err := p.Refresh(context.Background(), "polecat"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Error("Refresh did not exec after override change — VCS cache was not invalidated")
	}
}
