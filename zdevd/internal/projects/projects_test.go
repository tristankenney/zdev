package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func osMkdirAll(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func filepathBase(p string) string { return filepath.Base(p) }

func TestLister_Refresh(t *testing.T) {
	var ev tmuxctl.Event
	var mu sync.Mutex
	l := NewLister(func(e tmuxctl.Event) { mu.Lock(); ev = e; mu.Unlock() }, "")
	l.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("alpha\nbeta\nexample/frontend\n\n"), nil
	}
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := l.Names()
	want := []string{"alpha", "beta", "example/frontend"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("Names() = %v; want %v", names, want)
	}
	plc, ok := ev.(tmuxctl.ProjectListChanged)
	if !ok {
		t.Fatalf("got = %T; want ProjectListChanged", ev)
	}
	if len(plc.Names) != 3 {
		t.Errorf("ProjectListChanged.Names len = %d; want 3", len(plc.Names))
	}
}

func TestLister_RefreshEmpty(t *testing.T) {
	l := NewLister(func(tmuxctl.Event) {}, "")
	l.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if names := l.Names(); len(names) != 0 {
		t.Errorf("Names(empty) = %v; want []", names)
	}
}

func TestLister_NamesIsCopy(t *testing.T) {
	l := NewLister(func(tmuxctl.Event) {}, "")
	l.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("alpha\n"), nil
	}
	l.Refresh(context.Background())
	n1 := l.Names()
	n1[0] = "MUTATED"
	n2 := l.Names()
	if n2[0] != "alpha" {
		t.Errorf("after caller mutation, Names()[0] = %q; want alpha (defensive copy expected)", n2[0])
	}
}

// TestLister_RepoCache (260512-cfg) verifies Refresh populates the repo
// cache via ResolveRepo and that Repo() returns the resolved owner/repo
// for resolvable projects, ("", true) for known-but-unresolvable ones,
// and ("", false) for unknown projects.
func TestLister_RepoCache(t *testing.T) {
	workspace := t.TempDir()
	// Create dirs matching the projects we'll emit.
	for _, p := range []string{"alpha", "beta", "missing"} {
		_ = osMkdirAll(t, workspace, p)
	}

	orig := resolverExecFunc
	defer func() { resolverExecFunc = orig }()
	resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// Pick the dir off the args.
		var dir string
		for i, a := range args {
			if (a == "-R" || a == "-C") && i+1 < len(args) {
				dir = args[i+1]
			}
		}
		switch {
		case name == "sl" && dir != "" && (dir == workspace+"/alpha" || dir == workspace+"/beta"):
			return []byte("https://github.com/example/" + filepathBase(dir) + ".git\n"), nil
		case name == "sl":
			return nil, errors.New("not a sapling repo")
		case name == "git":
			return nil, errors.New("not a git repo")
		}
		return nil, errors.New("unexpected exec")
	}

	l := NewLister(func(tmuxctl.Event) {}, workspace)
	l.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("alpha\nbeta\nmissing\n"), nil
	}
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if r, ok := l.Repo("alpha"); !ok || r != "example/alpha" {
		t.Errorf("Repo(alpha) = (%q,%v); want (example/alpha,true)", r, ok)
	}
	if r, ok := l.Repo("beta"); !ok || r != "example/beta" {
		t.Errorf("Repo(beta) = (%q,%v); want (example/beta,true)", r, ok)
	}
	if r, ok := l.Repo("missing"); !ok || r != "" {
		t.Errorf("Repo(missing) = (%q,%v); want (\"\",true) — known-but-unresolvable", r, ok)
	}
	if r, ok := l.Repo("never-seen"); ok || r != "" {
		t.Errorf("Repo(never-seen) = (%q,%v); want (\"\",false)", r, ok)
	}
}

func TestLister_RefreshError(t *testing.T) {
	l := NewLister(func(tmuxctl.Event) {}, "")
	l.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("zdev not found")
	}
	if err := l.Refresh(context.Background()); err == nil {
		t.Error("expected error on exec failure")
	}
	if names := l.Names(); names != nil && len(names) != 0 {
		t.Errorf("on error, Names() = %v; want empty", names)
	}
}
