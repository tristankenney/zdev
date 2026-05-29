package probes

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// lsofProbeTimeout caps total wall-clock for the lsof Refresh: two
// local lsof calls. 3s tolerates a moderately-loaded kernel state
// lookup; typical < 200ms. Staff-review PR #2 — Subprocess M1.
const lsofProbeTimeout = 3 * time.Second

// LsofProbe queries `lsof -nP -iTCP -sTCP:LISTEN -F pcn` once per refresh
// and resolves each listening PID's cwd via a single multi-PID
// `lsof -p PID1,PID2,...,PIDN -d cwd -F n` call (per OQ-5 confirmation).
//
// Subsumes ~/.local/bin/zdev-sidebar-ports-refresh (D3-03; SC4 dtruss-verifies
// no external invocation in steady state).
type LsofProbe struct {
	submit    func(tmuxctl.Event)
	workspace string
	projects  func() []string // returns the canonical project list (DATA-10)

	execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewLsofProbe constructs an LsofProbe.
//
//	submit    — closure to emit PortsRefresh events
//	workspace — absolute path to ~/workspace; cwd matching uses HasPrefix
//	projects  — closure returning current project names from internal/projects.Lister
func NewLsofProbe(submit func(tmuxctl.Event), workspace string, projects func() []string) *LsofProbe {
	return &LsofProbe{
		submit:    submit,
		workspace: workspace,
		projects:  projects,
		execFunc:  defaultExec,
	}
}

// Class implements Probe.
func (l *LsofProbe) Class() string { return "lsof" }

// Refresh runs both lsof calls and emits PortsRefresh per project.
// The key argument is unused — lsof is a single global probe.
func (l *LsofProbe) Refresh(ctx context.Context, _ string) error {
	ctx, cancel := context.WithTimeout(ctx, lsofProbeTimeout)
	defer cancel()

	listenOut, err := l.execFunc(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn")
	if err != nil {
		// lsof exits 1 when no matching FDs exist — treat as empty.
		slog.Debug("lsof listen exited non-zero", "err", err)
		return nil
	}
	pidPorts := parseLsofF(listenOut)
	if len(pidPorts) == 0 {
		return nil
	}

	pids := make([]string, 0, len(pidPorts))
	for pid := range pidPorts {
		pids = append(pids, pid)
	}
	cwdOut, err := l.execFunc(ctx, "lsof",
		"-p", strings.Join(pids, ","),
		"-d", "cwd", "-F", "n")
	if err != nil {
		slog.Warn("lsof cwd lookup failed", "err", err)
		// Don't return — emit nothing rather than fail loudly.
		return nil
	}
	pidCwd := parseLsofCwd(cwdOut)

	projectPorts := make(map[string][]int)
	for pid, ports := range pidPorts {
		cwd := pidCwd[pid]
		proj := projectFromCwd(cwd, l.workspace)
		if proj == "" {
			continue
		}
		projectPorts[proj] = append(projectPorts[proj], ports...)
	}
	for proj, ports := range projectPorts {
		sort.Ints(ports)
		if len(ports) > 4 {
			ports = ports[:4]
		}
		l.submit(tmuxctl.PortsRefresh{Project: proj, Ports: ports})
	}
	return nil
}

// parseLsofF parses `lsof -F pcn` output. The output format emits one
// field per line, each prefixed with a 1-char field type:
//
//	p<pid>     — PID line; starts a new process record
//	c<command> — command name
//	n<address> — network address (e.g., *:3000, 127.0.0.1:9229, [::1]:5000)
//
// Returns map[pid][]port for IPv4 and IPv6 listening sockets.
func parseLsofF(b []byte) map[string][]int {
	out := make(map[string][]int)
	var currentPID string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			currentPID = line[1:]
		case 'n':
			if currentPID == "" {
				continue
			}
			port := portFromAddress(line[1:])
			if port > 0 {
				out[currentPID] = append(out[currentPID], port)
			}
		}
	}
	return out
}

// portFromAddress extracts the port number from lsof -F n shapes:
//
//	"*:3000"         → 3000
//	"127.0.0.1:9229" → 9229
//	"[::1]:5000"     → 5000
//	"*:5001"         → 5001
//
// Returns 0 on parse failure.
func portFromAddress(addr string) int {
	// Strip IPv6 brackets if present.
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		n, err := strconv.Atoi(addr[i+1:])
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// parseLsofCwd parses `lsof -p PIDS -d cwd -F n` output. Per-PID:
//
//	p<pid>
//	n<absolute path>
//
// Returns map[pid]cwdPath.
func parseLsofCwd(b []byte) map[string]string {
	out := make(map[string]string)
	var currentPID string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			currentPID = line[1:]
		case 'n':
			if currentPID != "" {
				out[currentPID] = line[1:]
			}
		}
	}
	return out
}

// projectFromCwd extracts the project name from a cwd path. Returns "" if
// cwd is not under the workspace.
//
// Workspace = "/Users/me/workspace", cwd = "/Users/me/workspace/myorg/frontend"
// → project = "myorg" (first path component below workspace).
//
// Bash baseline (~/.local/bin/zdev-sidebar-ports-refresh) uses ps-tree
// ancestor walk; D3-03 simplifies to cwd-based attribution.
func projectFromCwd(cwd, workspace string) string {
	if cwd == "" || workspace == "" {
		return ""
	}
	workspace = strings.TrimSuffix(workspace, "/")
	if !strings.HasPrefix(cwd, workspace+"/") {
		return ""
	}
	rest := cwd[len(workspace)+1:]
	if rest == "" {
		return ""
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}
