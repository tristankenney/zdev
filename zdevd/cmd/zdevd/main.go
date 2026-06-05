// Command zdevd is the long-lived state-emission daemon for the zdev tmux
// sidebar. Phase 3 extends the Phase 2 wiring with a probe scheduler, three
// concrete probes (gh/lsof/branch), two fsnotify watchers (notif/workspace),
// and a project lister.
//
// Lifecycle (per CONTEXT D-09/D-10/D-11/D2-02; OPS-02 — Apple-mandated
// foreground-only under launchd):
//
//  1. Parse flags + ZDEVD_DEBOUNCE_MS env var (OQ-6: strict validation).
//  2. setupSlog → ~/Library/Logs/zdev/zdevd.log JSON handler.
//  3. signal.NotifyContext for SIGINT/SIGTERM.
//  4. Construct Hub, Supervisor, Server; Phase 3 adds Lister, Scheduler,
//     GHProbe, LsofProbe, BranchProbe, notif.Watcher, workspace.Watcher.
//  5. SetHub on Server (required by Phase 2 contract — Plan 02-04).
//  6. Listen on socket (BEFORE the errgroup so the bind happens before any
//     renderer can race-connect).
//  7. errgroup.WithContext: run hub, supervisor, server.Serve, notifW, workspaceW.
//  8. errgroup.Wait blocks until ctx cancels OR any goroutine errors out.
//  9. defer Server.Close to remove the socket file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tristankenney/zdev/zdevd/internal/config"
	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/notif"
	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/probes"
	"github.com/tristankenney/zdev/zdevd/internal/projects"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
	"github.com/tristankenney/zdev/zdevd/internal/workspace"
)

// debounceDefault is the locked Phase 2 default per D2-01 / ARCH-05.
const debounceDefault = 16 * time.Millisecond

// statusDwellDefault is the minimum-dwell window applied to each project's
// displayed status (Attention) to suppress flapping — a status must hold for
// this long before it replaces what's shown. 250ms comfortably covers the
// sub-second working→waiting→working blips that aren't worth surfacing while
// staying imperceptible on genuine transitions. Override with
// ZDEVD_STATUS_DWELL_MS; set it to 0 to disable the debounce.
const statusDwellDefault = 250 * time.Millisecond

// version is injected at build time via -ldflags="-X main.version=…".
// Falls back to "dev" for `go build` / `go install` without ldflags.
var version = "dev"

func main() {
	// D4-06 subcommand routing: route BEFORE flag.Parse / Run so the diag
	// and history clients never bind the daemon's socket. Subcommands run
	// to completion and exit; only the no-arg path (or explicit `serve`)
	// proceeds into run() and the daemon lifecycle.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "diag":
			os.Exit(diagSubcmd(os.Args[2:]))
		case "history":
			os.Exit(historySubcmd(os.Args[2:]))
		case "-v", "--version", "version":
			fmt.Println(version)
			os.Exit(0)
		case "serve":
			// Allow explicit `serve` for documentation. Strip it from
			// os.Args so flag.Parse inside run() sees the rest as flags.
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		default:
			// Any non-flag first argument that isn't a known subcommand
			// is a usage error — the daemon takes flags only.
			if !strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr,
					"zdevd: unknown subcommand %q (expected: diag, history, version, or no args for daemon)\n",
					os.Args[1])
				os.Exit(2)
			}
		}
	}
	if err := run(); err != nil {
		// Mirror to stderr so launchd's StandardErrorPath captures the
		// failure even if slog is unconfigured.
		fmt.Fprintf(os.Stderr, "zdevd: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socketFlag = flag.String("socket", defaultSocketPath(), "unix socket path")
		logFlag    = flag.String("log", defaultLogPath(), "slog JSON log path")
		stateFlag  = flag.String("state", defaultStatePath(), "persisted state file path (empty = disabled)")
	)
	flag.Parse()

	if err := setupSlog(*logFlag); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd: setupSlog: %v\n", err)
		return err
	}

	// Phase 4 (D4-14, CONFIG-01..05): load TOML config before any subsystem
	// initialization. config.Load handles the four CONFIG paths in one
	// entry point — file-not-found returns Defaults() silently; parse
	// errors return non-nil err with line/col context already logged via
	// slog.Error inside Load. Returning the error here exits the process
	// with code 1; launchd's KeepAlive={Crashed:true} respawns and
	// ThrottleInterval=30 prevents a crash loop.
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd: config.Load: %v\n", err)
		return err
	}
	// TODO(post-phase-4): wire cfg.StaleSeconds into renderer thresholds,
	// cfg.PortsMax into probes.LsofProbe, etc. Phase 4 lands the load
	// contract (CONFIG-01..05); per-field consumption is incremental.
	_ = cfg

	debounce, err := parseDebounceWindow(os.Getenv("ZDEVD_DEBOUNCE_MS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd: ZDEVD_DEBOUNCE_MS: %v\n", err)
		return err
	}

	statusDwell, err := parseStatusDwell(os.Getenv("ZDEVD_STATUS_DWELL_MS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd: ZDEVD_STATUS_DWELL_MS: %v\n", err)
		return err
	}

	tmuxSocket := os.Getenv("ZDEVD_TMUX_SOCKET")

	slog.Info("zdevd starting (Phase 4)",
		"socket", *socketFlag,
		"log", *logFlag,
		"state", *stateFlag,
		"pid", os.Getpid(),
		"debounce_ms", debounce.Milliseconds(),
		"status_dwell_ms", statusDwell.Milliseconds(),
		"schema", proto.SchemaVersion,
		"tmux_version", tmuxVersion(),
		"config_path", config.DefaultPath(),
		"workspace", cfg.Workspace,
		"width", cfg.Width,
		"tmux_socket", tmuxSocket,
	)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Phase 4 (D4-10..12, LOG-01..03): construct the eventlog Writer at
	// production cap=16 (Plan 01 ships DefaultChanCap baked into
	// eventlog.New). The writer's Run goroutine is wired into the errgroup
	// below alongside the other I/O subsystems.
	evlog := eventlog.New(eventlog.DefaultPath())
	slog.Info("eventlog wired", "path", eventlog.DefaultPath())

	// Construct subsystems.
	//
	// All hub dependencies bundle into a single hub.Config (staff-review PR
	// #4 — Arch MAJOR #3: replaces the prior WithSocketPath/WithEventLog/
	// WithStatePath/WithNotifier fluent chain). Fields are read-only after
	// Run starts; nil/empty values stay disabled.
	hubCfg := hub.Config{
		Debounce:    debounce,
		StatusDwell: statusDwell,
		SocketPath:  *socketFlag,
		EventLog:    evlog,
		StatePath:   *stateFlag,
		Agents:      cfg.AgentRegistry(),
	}

	// Wait-tier notifications: opt-out via ZDEV_NOTIFY=0; otherwise resolve
	// terminal-notifier on PATH and wire it. If terminal-notifier is missing,
	// log Info and continue with Notifier == nil (tierCheck no-ops).
	if os.Getenv("ZDEV_NOTIFY") != "0" {
		if path, err := exec.LookPath("terminal-notifier"); err == nil {
			hubCfg.Notifier = hub.RealNotifier(path)
			slog.Info("tier notifications enabled", "binary", path)
		} else {
			slog.Info("terminal-notifier not found; tier notifications disabled")
		}
	} else {
		slog.Info("tier notifications disabled by ZDEV_NOTIFY=0")
	}

	h := hub.NewHub(hubCfg)

	// Restore persisted state BEFORE the listener starts accepting renderer
	// connections. A renderer dialing on launch would otherwise observe an
	// empty-state snapshot followed by a restored-state snapshot, causing
	// visible chip flicker.
	if err := h.LoadPersistedState(); err != nil {
		slog.Warn("hub: load persisted state failed (starting clean)", "err", err, "path", *stateFlag)
		// Non-fatal — daemon proceeds with empty state. loadState
		// already returns nil for missing-file / version-mismatch /
		// malformed cases; reaching this branch implies a genuinely
		// unexpected error.
	}

	srv := socket.NewServer(*socketFlag)
	srv.SetHub(h)
	if err := srv.Listen(); err != nil {
		slog.Error("listen failed", "err", err, "socket", *socketFlag)
		return err
	}
	defer func() {
		if err := srv.Close(); err != nil {
			slog.Warn("server close error", "err", err)
		}
	}()
	slog.Info("zdevd listening", "socket", *socketFlag)

	// Phase 3 — probes + fsnotify watchers + scheduler.

	// Probe scheduler (D3-01): single-flight + max-staleness gating.
	// Constructed before submitEvent so the lister's ProjectListChanged
	// events can drive scheduler.Forget for projects that drop out of the
	// workspace (release per-project lastOK bookkeeping).
	sched := probes.NewScheduler()

	// All Phase 3 producers submit events to the hub via the same closure
	// pattern as the supervisor (Phase 2 invariant: hub goroutine is the
	// sole state mutator). The closure also detects ProjectListChanged
	// transitions and forgets scheduler bookkeeping for dropped projects.
	var lastProjects []string
	submitEvent := func(ev tmuxctl.Event) {
		if plc, ok := ev.(tmuxctl.ProjectListChanged); ok {
			next := make(map[string]struct{}, len(plc.Names))
			for _, n := range plc.Names {
				next[n] = struct{}{}
			}
			for _, n := range lastProjects {
				if _, still := next[n]; still {
					continue
				}
				// Forget both slash-form and dash-form keys since
				// RefreshIfStale callers may use either depending on
				// whether a tmux session name matched.
				sched.Forget(n)
				sched.Forget(proto.SessionKey(n))
			}
			lastProjects = append(lastProjects[:0], plc.Names...)
		}
		_ = h.Submit(ev)
	}

	// Workspace dir resolution: ZDEV_WORKSPACE env var or ~/workspace default.
	// Resolved before NewLister so per-project repo resolution can run during
	// the initial Refresh.
	workspaceDir := os.Getenv("ZDEV_WORKSPACE")
	if workspaceDir == "" {
		workspaceDir = filepath.Join(os.Getenv("HOME"), "workspace")
	}

	// Project lister (DATA-10): canonical row source from `zdev --list-projects`.
	// 260512-cfg: also caches name → GitHub owner/repo by reading sl/git remotes.
	lister := projects.NewLister(submitEvent, workspaceDir)
	if err := lister.Refresh(ctx); err != nil {
		slog.Warn("initial project list refresh failed", "err", err)
		// Continue — fsnotify watcher will retry on workspace changes.
	}

	// Concrete probes.
	// GHProbe, LsofProbe, BranchProbe: Phase 3 originals.
	// CIProbe (260509-gfz): per-project CI status via gh run list.
	ghProbe := probes.NewGHProbe(submitEvent, lister, workspaceDir)
	ciProbe := probes.NewCIProbe(submitEvent, workspaceDir, lister)
	lsofProbe := probes.NewLsofProbe(submitEvent, workspaceDir, lister.Names)
	branchProbe := probes.NewBranchProbe(submitEvent, workspaceDir)

	// fsnotify watchers (D3-05 + D3-06).
	//
	// notif watch path is a PRIVATE subdir of the daemon's TMPDIR
	// (notif.WatchDir) per Pitfall 20 — NOT a shell-export. launchd sets
	// TMPDIR per-user (e.g., /var/folders/hk/.../T/) at process spawn time.
	//
	// Watching TMPDIR directly would wake the daemon on every file event
	// from Spotlight, browsers, IDEs, `go test`, etc. The private subdir
	// scopes wake-ups to zdev-notify's own writes. The writer
	// (~/.local/bin/zdev-notify) must mkdir the same subdir before writing.
	tmpParent := os.Getenv("TMPDIR")
	if tmpParent == "" {
		tmpParent = "/tmp"
	}
	notifDir := notif.WatchDir(tmpParent)
	notifW := notif.NewWatcher(notifDir, submitEvent)
	workspaceW := workspace.NewWatcher(workspaceDir, lister)

	sup := tmuxctl.NewSupervisor(func(ev tmuxctl.Event) {
		// Always forward to the hub (Phase 2 contract).
		_ = h.Submit(ev)

		// Phase 3 (D3-01): schedule probe refreshes on tmux events that
		// imply probe-relevance. RefreshIfStale is non-blocking — workers
		// run off-thread.
		switch e := ev.(type) {
		case tmuxctl.SessionChanged:
			if e.Name == "" {
				return
			}
			// Resolve the canonical slash-form project name for this session.
			// zdev --list-projects returns slash-form ("example/backend") but
			// tmux session names use dash-form ("example-backend"). Match by
			// normalizing the project list entry and comparing to e.Name.
			probeKey := e.Name
			for _, name := range lister.Names() {
				if proto.SessionKey(name) == e.Name {
					probeKey = name
					break
				}
			}
			// 260514 perf: branch staleness raised from 10s → 60s. The probe
			// shells out to `sl bookmark`/`sl status -q` (or git equivalents)
			// — each call takes seconds on big sapling repos like agora, and
			// multiple project probes were running in parallel as the user
			// navigated sessions, eating CPU and contributing to typing lag.
			// Branch + dirty count tolerate up-to-1-minute staleness.
			sched.RefreshIfStale(ctx, branchProbe, probeKey, 60*time.Second)
			sched.RefreshIfStale(ctx, ghProbe, probeKey, 5*time.Minute)
			sched.RefreshIfStale(ctx, ciProbe, probeKey, 5*time.Minute)
			sched.RefreshIfStale(ctx, lsofProbe, "", 10*time.Second)
		case tmuxctl.WindowPaneChanged:
			// Pane churn — refresh lsof for any session that owns the
			// window. Cheap to over-schedule: max-staleness gates dedup.
			_ = e // silence unused-variable warning
			sched.RefreshIfStale(ctx, lsofProbe, "", 10*time.Second)
		case tmuxctl.PaneTitleChanged:
			// Pane title change implies agent state may have flipped; the
			// hub.recomputeAgents handles that synchronously. Don't
			// schedule probes here — title changes don't affect git/PR/lsof.
		}
	},
		tmuxctl.WithSocketName(tmuxSocket),
	)

	// Phase 4 (D4-10): daemon-start marker. Submitted BEFORE the eventlog
	// goroutine starts draining — Plan 01's cap=16 buffer absorbs the event
	// in flight; once the goroutine starts (a few lines below) it drains
	// the buffer naturally. This non-blocking Submit pattern is the same
	// one Plan 01 documented for the daemon-start burst case.
	evlog.Submit(eventlog.Event{
		Ts:      time.Now().UTC(),
		Type:    "daemon-start",
		Version: proto.SchemaVersion,
		PID:     os.Getpid(),
	})

	// Phase 4 (D4-10..12, LOG-01..03): eventlog Writer runs on its own
	// context so it outlives the errgroup, letting us submit the
	// daemon-stop marker AFTER the other subsystems have shut down and
	// still have the writer drain it. If evlog shared gctx, g.Wait()
	// would return only after Run had already drained and closed the
	// file, and the daemon-stop Submit would land on a dead channel.
	logCtx, logCancel := context.WithCancel(context.Background())
	logDone := make(chan error, 1)
	go func() { logDone <- evlog.Run(logCtx) }()

	// errgroup orchestration: any goroutine returning an error cancels the
	// shared ctx and triggers the others to shut down.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return h.Run(gctx) })
	g.Go(func() error { return sup.Run(gctx) })
	g.Go(func() error {
		if err := srv.Serve(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})

	// Phase 3 watchers — fsnotify-based, ctx-cancellable.
	g.Go(func() error { return notifW.Run(gctx) })
	g.Go(func() error { return workspaceW.Run(gctx) })

	wErr := g.Wait()

	// All event producers have stopped — submit daemon-stop into a writer
	// that is still alive, then bring the writer down. Run's deferred
	// drain (eventlog.go) picks up the marker before closing the file.
	evlog.Submit(eventlog.Event{
		Ts:   time.Now().UTC(),
		Type: "daemon-stop",
		PID:  os.Getpid(),
	})
	logCancel()
	if lerr := <-logDone; lerr != nil && !errors.Is(lerr, context.Canceled) {
		slog.Warn("eventlog: Run returned error", "err", lerr)
	}

	if wErr != nil && !errors.Is(wErr, context.Canceled) {
		slog.Error("errgroup returned error", "err", wErr)
		return wErr
	}
	slog.Info("zdevd stopped cleanly")
	return nil
}

// parseDebounceWindow honors OQ-6: empty string → 16ms default; positive
// integer → that many ms; 0 / negative / non-integer → error. The daemon
// refuses to start on a bad value rather than silently falling back to the
// default — operator error should surface immediately.
func parseDebounceWindow(raw string) (time.Duration, error) {
	if raw == "" {
		return debounceDefault, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid ZDEVD_DEBOUNCE_MS=%q: must be a positive integer (ms)", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("ZDEVD_DEBOUNCE_MS=%d: must be > 0", n)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// parseStatusDwell honors the status-flap debounce knob: empty string →
// statusDwellDefault; 0 → disabled (no debounce); positive integer → that many
// ms; negative / non-integer → error. Unlike ZDEVD_DEBOUNCE_MS, 0 is a valid
// value here (it disables the feature) rather than an error.
func parseStatusDwell(raw string) (time.Duration, error) {
	if raw == "" {
		return statusDwellDefault, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid ZDEVD_STATUS_DWELL_MS=%q: must be a non-negative integer (ms)", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("ZDEVD_STATUS_DWELL_MS=%d: must be >= 0", n)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// tmuxVersion runs `tmux -V` once at startup and returns the trimmed output.
// Empty string on error. OQ-4 visibility — surfaces tmux protocol version
// drift in daemon logs.
//
// 1s timeout: tmux -V is a near-instant local command. The timeout guards
// against a hung tmux binary blocking the daemon's startup banner — staff-
// review PR #2 — Subprocess M6.
func tmuxVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "-V").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Defaults delegate to internal/platform so the same binary works on
// macOS (Library/...) and Linux (XDG dirs).
func defaultSocketPath() string { return platform.SocketPath() }
func defaultStatePath() string  { return platform.StatePath() }
func defaultLogPath() string    { return platform.LogPath("zdevd") }

// setupSlog is unchanged from Phase 1: opens (or creates) the JSON log file,
// mkdir-ing the parent dir at 0700 if missing (Pitfall E). The slog default
// handler is set to write JSON lines at LevelInfo with source attribution.
func setupSlog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}
