package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		want      string
		wantErr   bool
		errIsNGH  bool // true when error should be ErrNotGitHubRemote
	}{
		{"https_with_git", "https://github.com/zitcha/agora.git", "zitcha/agora", false, false},
		{"https_no_git", "https://github.com/zitcha/agora", "zitcha/agora", false, false},
		{"https_trailing_slash", "https://github.com/zitcha/agora/", "zitcha/agora", false, false},
		{"ssh_at_form", "git@github.com:zitcha/agora.git", "zitcha/agora", false, false},
		{"ssh_at_form_no_git", "git@github.com:zitcha/agora", "zitcha/agora", false, false},
		{"ssh_url_form", "ssh://git@github.com/zitcha/agora.git", "zitcha/agora", false, false},
		{"whitespace_trimmed", "  https://github.com/zitcha/agora.git\n", "zitcha/agora", false, false},
		{"empty", "", "", true, false},
		{"gitlab_returns_NGH", "https://gitlab.com/zitcha/agora.git", "", true, true},
		{"bitbucket_returns_NGH", "git@bitbucket.org:zitcha/agora.git", "", true, true},
		{"unsupported_scheme", "ftp://example.com/x/y", "", true, false},
		{"missing_path", "https://github.com", "", true, false},
		{"too_few_segments", "https://github.com/owner", "", true, false},
		{"too_many_segments", "https://github.com/owner/repo/sub", "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseGitHubURL(c.url)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseGitHubURL(%q) = (%q, nil); want error", c.url, got)
				}
				if c.errIsNGH && !errors.Is(err, ErrNotGitHubRemote) {
					t.Errorf("ParseGitHubURL(%q) error = %v; want errors.Is ErrNotGitHubRemote", c.url, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGitHubURL(%q) error: %v", c.url, err)
			}
			if got != c.want {
				t.Errorf("ParseGitHubURL(%q) = %q; want %q", c.url, got, c.want)
			}
		})
	}
}

func TestResolveRepo_DirMissing(t *testing.T) {
	_, err := ResolveRepo(context.Background(), "/nonexistent/path-that-cannot-exist")
	if err == nil {
		t.Fatal("ResolveRepo with missing dir returned nil error")
	}
}

func TestResolveRepo_SaplingSucceeds(t *testing.T) {
	dir := t.TempDir()
	orig := resolverExecFunc
	defer func() { resolverExecFunc = orig }()

	var slCalled, gitCalled bool
	resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "sl":
			slCalled = true
			return []byte("https://github.com/zitcha/agora.git\n"), nil
		case "git":
			gitCalled = true
			t.Error("git fallback should not run when sl succeeds")
			return nil, nil
		}
		return nil, errors.New("unexpected exec")
	}

	repo, err := ResolveRepo(context.Background(), dir)
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if repo != "zitcha/agora" {
		t.Errorf("repo = %q; want zitcha/agora", repo)
	}
	if !slCalled {
		t.Error("sl backend was not invoked")
	}
	if gitCalled {
		t.Error("git fallback should not have been called")
	}
}

func TestResolveRepo_FallsBackToGit(t *testing.T) {
	dir := t.TempDir()
	orig := resolverExecFunc
	defer func() { resolverExecFunc = orig }()

	var gitArgs []string
	resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "sl":
			return nil, errors.New("not a sapling repo")
		case "git":
			gitArgs = append([]string(nil), args...)
			return []byte("git@github.com:zitcha/agora.git\n"), nil
		}
		return nil, errors.New("unexpected exec")
	}

	repo, err := ResolveRepo(context.Background(), dir)
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if repo != "zitcha/agora" {
		t.Errorf("repo = %q; want zitcha/agora", repo)
	}
	// Regression guard for the lock-leak fix: every git invocation must
	// pass --no-optional-locks so a SIGKILL'd git can't leave a stale
	// .git/index.lock behind.
	var sawFlag bool
	for _, a := range gitArgs {
		if a == "--no-optional-locks" {
			sawFlag = true
			break
		}
	}
	if !sawFlag {
		t.Errorf("git invocation missing --no-optional-locks: %v", gitArgs)
	}
}

func TestResolveRepo_BothFail(t *testing.T) {
	dir := t.TempDir()
	orig := resolverExecFunc
	defer func() { resolverExecFunc = orig }()

	resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("simulated " + name + " failure")
	}

	_, err := ResolveRepo(context.Background(), dir)
	if err == nil {
		t.Fatal("ResolveRepo with both backends failing returned nil error")
	}
	if !strings.Contains(err.Error(), "no remote") {
		t.Errorf("error = %v; want it to mention 'no remote'", err)
	}
}

func TestResolveRepo_NonGitHubRemote(t *testing.T) {
	dir := t.TempDir()
	orig := resolverExecFunc
	defer func() { resolverExecFunc = orig }()

	resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "sl" {
			return []byte("https://gitlab.com/zitcha/agora.git\n"), nil
		}
		return nil, errors.New("not called")
	}

	_, err := ResolveRepo(context.Background(), dir)
	if !errors.Is(err, ErrNotGitHubRemote) {
		t.Errorf("ResolveRepo with gitlab remote: error = %v; want errors.Is ErrNotGitHubRemote", err)
	}
}

// TestResolveRepo_RealDir is a smoke test: if the test runs from a checkout
// of this repo, ResolveRepo on the repo root should return "tristankenney/dotfiles"
// or whatever the real remote is. Skipped when no remote is configured.
func TestResolveRepo_RealDir(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test against real working copy")
	}
	// Walk up until we find .sl or .git
	d, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(d, ".sl")); err == nil {
			break
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break
		}
		next := filepath.Dir(d)
		if next == d {
			t.Skip("no repo root found in ancestors")
		}
		d = next
	}
	repo, err := ResolveRepo(context.Background(), d)
	if err != nil {
		t.Skipf("smoke test skipped: %v", err)
	}
	if repo == "" || !strings.Contains(repo, "/") {
		t.Errorf("ResolveRepo(repo root) = %q; want owner/repo form", repo)
	}
	t.Logf("resolved repo at %s = %q", d, repo)
}
