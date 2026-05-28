// Package projects' resolver.go — resolves a working-copy directory to its
// canonical GitHub repo identifier (owner/repo) by consulting the local VCS.
//
// Why this exists (260512-cfg): the project list emits names like
// "example/agora-a", which is the local *worktree directory name*, not the
// GitHub repo. Multiple worktree directories (agora-a, agora-b, agora-c) can
// share a single upstream repo (example/agora). Without resolving the actual
// remote, gh probes ask GitHub for repos that don't exist.
//
// Resolution strategy:
//  1. `sl -R <dir> config paths.default` — works for Sapling repos.
//  2. `git -C <dir> remote get-url origin` — fallback for plain git repos.
//  3. Parse the URL → owner/repo using ParseGitHubURL.
//
// Both backends are tried in order so the same helper works for a mixed
// workspace (some repos sl, some git). Non-GitHub remotes return an error
// so the caller can skip the probe rather than send malformed gh calls.

package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// resolveTimeout caps the wall-clock of a single backend (sl or git) call.
// Both are local filesystem operations; 2s is generous and bounds total
// resolution wall-time per project to ~4s in the worst case.
const resolveTimeout = 2 * time.Second

// ErrNotGitHubRemote is returned by ParseGitHubURL when the URL is parseable
// but doesn't point at github.com. Distinct from a parse failure so callers
// can log it differently (expected for gitlab/bitbucket workspaces).
var ErrNotGitHubRemote = errors.New("projects: remote is not github.com")

// resolverExecFunc is the exec backend. Package-level variable so tests can
// stub without an exec.Cmd round-trip. Default invokes exec.CommandContext.
var resolverExecFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return out, augmentExecError(err)
}

// ResolveRepo resolves the GitHub owner/repo for the working copy at dir.
// Returns ("", err) when:
//   - dir doesn't exist (fs.PathError surfaces via os.Stat)
//   - neither sl nor git can read a remote (both backends fail)
//   - the remote URL parses but isn't github.com (ErrNotGitHubRemote)
//
// Caller should INFO-log the error once and skip the probe for that project
// — these failures are expected for synthetic sessions, scratch dirs, and
// non-github workspaces.
func ResolveRepo(ctx context.Context, dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("resolve repo: stat %s: %w", dir, err)
	}

	// Try Sapling first — Sapling-only repos have no .git/ for git to read.
	slCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	out, err := resolverExecFunc(slCtx, "sl", "-R", dir, "config", "paths.default")
	cancel()
	if err == nil {
		url := strings.TrimSpace(string(out))
		if url != "" {
			return ParseGitHubURL(url)
		}
	}

	// Fall back to git.
	gitCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	// --no-optional-locks: defensive (remote get-url shouldn't take .git/index.lock,
	// but make the contract explicit so future git versions can't surprise us).
	out, err = resolverExecFunc(gitCtx, "git", "--no-optional-locks", "-C", dir, "remote", "get-url", "origin")
	cancel()
	if err == nil {
		url := strings.TrimSpace(string(out))
		if url != "" {
			return ParseGitHubURL(url)
		}
	}

	return "", fmt.Errorf("resolve repo: %s: no remote from sl or git", dir)
}

// ParseGitHubURL extracts owner/repo from a github.com remote URL. Supported
// shapes:
//
//	https://github.com/owner/repo
//	https://github.com/owner/repo.git
//	git@github.com:owner/repo
//	git@github.com:owner/repo.git
//	ssh://git@github.com/owner/repo.git
//
// Returns ErrNotGitHubRemote for parseable non-github URLs (gitlab,
// bitbucket, custom remotes) and a generic error for malformed input.
func ParseGitHubURL(url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", errors.New("parse github url: empty input")
	}

	var hostAndPath string
	switch {
	case strings.HasPrefix(url, "https://"):
		hostAndPath = strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "ssh://"):
		hostAndPath = strings.TrimPrefix(url, "ssh://")
		// ssh:// URLs include git@ user prefix; strip it.
		if at := strings.Index(hostAndPath, "@"); at >= 0 {
			hostAndPath = hostAndPath[at+1:]
		}
	case strings.HasPrefix(url, "git@"):
		// SCP-style: git@host:owner/repo. Normalize to host/owner/repo so the
		// rest of the parser doesn't fork.
		hostAndPath = strings.TrimPrefix(url, "git@")
		hostAndPath = strings.Replace(hostAndPath, ":", "/", 1)
	default:
		return "", fmt.Errorf("parse github url: unsupported scheme: %q", url)
	}

	// Split host/path.
	parts := strings.SplitN(hostAndPath, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("parse github url: missing path: %q", url)
	}
	host, path := parts[0], parts[1]
	if host != "github.com" {
		return "", fmt.Errorf("%w: %s", ErrNotGitHubRemote, host)
	}

	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")

	// owner/repo must be exactly two non-empty segments.
	segs := strings.Split(path, "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", fmt.Errorf("parse github url: expected owner/repo, got %q", path)
	}
	return segs[0] + "/" + segs[1], nil
}
