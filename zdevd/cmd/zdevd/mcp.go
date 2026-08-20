package main

// `zdevd mcp` — the zdev control plane as an MCP server (agent operations
// plane, phase 1b). It exposes zdev's verbs as MCP tools so any MCP client —
// local Claude Code today, a remote/phone client over tsnet tomorrow — drives
// and observes the fleet through one universal seam.
//
// Transport: newline-delimited JSON-RPC 2.0 over stdio (the MCP stdio
// transport). Hand-rolled on the stdlib — no external SDK — because the
// surface is tiny (initialize / tools/list / tools/call) and a hermetic build
// matters more than the convenience here. The same tool dispatch lifts onto a
// Streamable-HTTP handler unchanged when phase 2 binds it to the tailnet.
//
// Tools are thin adapters over the EXISTING CLIs (which already emit JSON), so
// there is one source of truth for fleet state and actuation:
//   fleet_status -> zdev-show list --json
//   triage       -> zdev-show triage --json
//   review       -> zdev-show review --json
//   run          -> zdev run --supervised -- <project> "<prompt>"   (actuator)
//
// The read tools register unconditionally; `run` is an ACTUATOR and registers
// only when ZDEV_MCP_ACTUATE=1 (mcpActuateEnabled). The HTTP transport is
// bearer-authenticated and loopback-only unless explicitly opted out. Both are
// hardening for the phase-1b security audit (C1 / H4).
//
// The focus-loop tools (zdev_park / zdev_anchor* / zdev_held* —
// docs/design/command-centre.md) are the exception: there is no
// `zdev-show anchor --json` to shell to, so they call the daemon's socket
// client directly (internal/socket) — the same one-shot round trips
// `zdevd park` / `zdevd anchor` / `zdevd held-rm` already use. This exposes
// the focus loop to any MCP client (a Claude Code skill's morning /plan
// flow first) without inventing a second wire protocol.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	socketpkg "github.com/tristankenney/zdev/zdevd/internal/socket"
)

// mcpProtocolVersion is the fallback MCP version advertised when the client
// sends none; otherwise we echo the client's requested version (the common
// pragmatic handshake — the client adapts to a server it can speak).
const mcpProtocolVersion = "2025-06-18"

// mcpToolTimeout bounds a single tool subprocess. Reads are fast; `run` only
// spawns a window (it does not wait for the agent), so this is generous.
const mcpToolTimeout = 20 * time.Second

// mcpExec is the swappable subprocess backend (tests inject a stub). It
// resolves a zdev CLI by name and runs it with args, returning combined stdout.
var mcpExec = defaultMCPExec

func defaultMCPExec(ctx context.Context, bin string, args ...string) ([]byte, error) {
	path := resolveZdevBin(bin)
	cctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	return exec.CommandContext(cctx, path, args...).Output()
}

// --- focus-loop socket seam (swappable in tests, mirrors mcpExec above) ---
//
// The park/anchor/held tools talk to the daemon's unix socket directly via
// internal/socket, not through a CLI subprocess. Each daemon round trip goes
// through one of these vars so tests can stub the daemon out entirely (no
// live zdevd needed), exactly as mcpExec stubs the exec seam.
var (
	mcpDialPark         = socketpkg.DialPark
	mcpDialAnchorSet    = socketpkg.DialAnchorSet
	mcpDialAnchorClear  = socketpkg.DialAnchorClear
	mcpDialHeldRemove   = socketpkg.DialHeldRemove
	mcpDialSchedulePush = socketpkg.DialSchedulePush
	mcpSnapshot         = defaultMCPSnapshot
)

// mcpSocketTimeout bounds one focus-loop tool's daemon round trip. The
// client's own Dial is already bounded (~1s, socket.dialTimeout) regardless
// of context, but wrapping the whole call the way mcpToolTimeout wraps
// subprocess tools keeps a wedged daemon from hanging the MCP server's
// single-threaded stdio loop.
const mcpSocketTimeout = 5 * time.Second

// mcpSocketPath resolves the daemon socket the same way every other zdevd
// subcommand does (zdevd anchor, zdevd park, zdevd cursor, zdevd diag):
// platform.ResolveSocketPath(), which honors a ZDEVD_SOCKET override before
// falling back to the computed path / daemon discovery file.
func mcpSocketPath() string { return platform.ResolveSocketPath() }

// defaultMCPSnapshot takes exactly one snapshot off the daemon socket and
// closes the connection — the same one-shot read anchorPrintSubcmd (above,
// zdevd anchor with no action word) already uses, because there is no
// dedicated "get anchor" / "get held" wire verb: the snapshot carries both
// Anchor and Held on every connect.
func defaultMCPSnapshot(ctx context.Context, socketPath string) (*proto.Snapshot, error) {
	snap, conn, err := socketpkg.Subscribe(ctx, socketPath, "", "")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return snap, nil
}

// anchorSnapshotJSON renders the zdev_anchor tool's reply from a snapshot.
// Pure — now is passed in (unix seconds) rather than sampled, per repo
// convention, so it's table-testable without a clock.
func anchorSnapshotJSON(snap *proto.Snapshot, now int64) (string, error) {
	out := map[string]any{
		"in_focus":   snap.InFocus,
		"free_until": snap.FreeUntil,
	}
	if snap.Anchor == nil {
		out["anchored"] = false
	} else {
		out["anchored"] = true
		out["title"] = snap.Anchor.Title
		out["project"] = snap.Anchor.Project
		out["since_ts"] = snap.Anchor.SinceTS
		out["elapsed_sec"] = now - snap.Anchor.SinceTS
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// heldSnapshotJSON renders the zdev_held tool's reply from a snapshot: the
// held set, chronological (Snapshot.Held already is), each item's age
// computed against the passed-in now rather than time.Now().
func heldSnapshotJSON(snap *proto.Snapshot, now int64) (string, error) {
	items := make([]map[string]any, 0, len(snap.Held))
	for _, h := range snap.Held {
		items = append(items, map[string]any{
			"id":      h.ID,
			"kind":    h.Kind,
			"title":   h.Title,
			"project": h.Project,
			"age_sec": now - h.SinceTS,
		})
	}
	b, err := json.Marshal(items)
	return string(b), err
}

// resolveZdevBin finds a zdev CLI: an explicit env override (ZDEV_SHOW_BIN /
// ZDEV_BIN), then PATH, then the conventional ~/.local/bin install dir. Hook-
// and MCP-client-spawned processes often run with a stripped PATH, so the
// fallback keeps the tools working.
func resolveZdevBin(bin string) string {
	envKey := "ZDEV_" + strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(bin, "zdev-"), "-", "_")) + "_BIN"
	if bin == "zdev" {
		envKey = "ZDEV_BIN"
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "bin", bin)
	}
	return bin
}

// --- JSON-RPC 2.0 wire shapes ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent on notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpTool is one exposed tool: its schema plus how to actuate it.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// run executes the tool and returns the text payload, or an error whose
	// message becomes an isError tool result (never a transport error — tool
	// failures are reported in-band per the MCP spec).
	run func(ctx context.Context, args map[string]any) (string, error) `json:"-"`
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// mcpActuateEnabled reports whether ACTUATOR tools (run — and anything else
// that spawns, kills, or mutates OTHER sessions) are registered. It is OFF by
// default and the operator opts in per process with ZDEV_MCP_ACTUATE=1.
//
// This is the primary containment for the C1 finding: the `run` actuator
// shells to `bin/zdev run`, which execs an agent with all permission gates
// off. A prompt-injected agent that can reach this MCP server must NOT be able
// to spawn permission-less agents across the fleet just by calling a tool.
// Read-only tools (fleet_status/triage/review) and focus-loop tools that only
// adjust the operator's OWN view (park/anchor/held/schedule) carry no such
// blast radius, so they register unconditionally; actuators do not.
func mcpActuateEnabled() bool { return os.Getenv("ZDEV_MCP_ACTUATE") == "1" }

// mcpTools is the registry actually served. Read-only tools always register;
// actuator tools register only when mcpActuateEnabled(). Both transports go
// through here (stdio and HTTP), so the gate holds regardless of wire.
func mcpTools() []mcpTool {
	tools := mcpReadTools()
	if mcpActuateEnabled() {
		tools = append(tools, mcpActuatorTools()...)
	}
	return tools
}

// mcpReadTools is the always-on surface: reads of fleet state and adjustments
// to the operator's own focus view. Nothing here spawns, kills, or mutates
// another session. Read tools shell to zdev-show --json; the focus-loop tools
// talk to the daemon socket directly.
func mcpReadTools() []mcpTool {
	readTool := func(name, desc string, showArgs ...string) mcpTool {
		return mcpTool{
			Name: name, Description: desc,
			InputSchema: objectSchema(map[string]any{}),
			run: func(ctx context.Context, _ map[string]any) (string, error) {
				out, err := mcpExec(ctx, "zdev-show", showArgs...)
				if err != nil {
					return "", fmt.Errorf("zdev-show %s: %w", strings.Join(showArgs, " "), err)
				}
				return string(out), nil
			},
		}
	}
	return []mcpTool{
		readTool("fleet_status",
			"List every project in the fleet with derived status (waiting/working/done/dead/idle), git branch, PR/CI counts, and any active agent teams. Returns JSON.",
			"list", "--json"),
		readTool("triage",
			"The ranked attention queue: which sessions need the human next, in priority order (needs-permission and deaths first, then oldest waits). Returns JSON.",
			"triage", "--json"),
		readTool("review",
			"Landing-readiness gauge: per repo, which PRs are ready to land vs need a fix vs will rot uncommitted, longest-rotting first. Returns JSON.",
			"review", "--json"),
		{
			Name:        "zdev_park",
			Description: "Park a thought into the held set (docs/design/command-centre.md — the focus loop). Use this instead of interrupting the operator with a question or a suggestion mid-focus: capture it and move on, and it gets a guaranteed hearing at the next boundary review. The reply only confirms the park succeeded — it does NOT return the created item's id. The daemon assigns one internally (convention: \"parked-<nanos>\") but that id is not echoed on this call; call zdev_held afterwards if you need it (e.g. to remove the item you just parked).",
			InputSchema: objectSchema(map[string]any{
				"text": map[string]any{"type": "string", "description": "The thought to park, one line."},
			}, "text"),
			run: func(ctx context.Context, args map[string]any) (string, error) {
				text, _ := args["text"].(string)
				if strings.TrimSpace(text) == "" {
					return "", fmt.Errorf("zdev_park requires non-empty 'text'")
				}
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				ok, err := mcpDialPark(cctx, mcpSocketPath(), text)
				if err != nil {
					return "", fmt.Errorf("zdev_park: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("zdev_park: daemon rejected the request")
				}
				return "parked", nil
			},
		},
		{
			Name:        "zdev_anchor_set",
			Description: "Set the anchor — the ONE thing the operator is on right now (docs/design/command-centre.md — \"the anchor lifecycle\"). This REPLACES any current anchor; there is only ever one, and picking a new one is the design's boundary-pick semantics, not an addition. Setting it engages the airlock: while anchored, new arrivals are held silently for the next boundary review instead of interrupting immediately. 'project' is the canonical slash-form zdev project name (e.g. \"zitcha/backend\") when this work maps to a zdev session; leave it empty for listless work (a phone call, an ad-hoc favour) — it does not need to map to anything. Only call this when the operator has actually chosen what to work on next; never guess or infer an anchor on their behalf — a wrong guess destroys the tether's credibility.",
			InputSchema: objectSchema(map[string]any{
				"title":   map[string]any{"type": "string", "description": "What the operator picked, e.g. \"IMP-97 validate deploy\"."},
				"project": map[string]any{"type": "string", "description": "Optional: canonical slash-form zdev project name this maps to; empty for listless work."},
			}, "title"),
			run: func(ctx context.Context, args map[string]any) (string, error) {
				title, _ := args["title"].(string)
				if strings.TrimSpace(title) == "" {
					return "", fmt.Errorf("zdev_anchor_set requires non-empty 'title'")
				}
				project, _ := args["project"].(string)
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				ok, err := mcpDialAnchorSet(cctx, mcpSocketPath(), title, project)
				if err != nil {
					return "", fmt.Errorf("zdev_anchor_set: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("zdev_anchor_set: daemon rejected the request")
				}
				return "anchored: " + title, nil
			},
		},
		{
			Name:        "zdev_anchor_clear",
			Description: "Explicitly clear the anchor — the boundary the focus loop is built around (docs/design/command-centre.md). Fires the daemon's boundary notification so the held set gets its hearing. Call this only when the operator says they're done with the anchored thing (or explicitly asks to unanchor); it is a deliberate release, not a side effect of picking something else (anchoring a new thing replaces the anchor on its own via zdev_anchor_set).",
			InputSchema: objectSchema(map[string]any{}),
			run: func(ctx context.Context, _ map[string]any) (string, error) {
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				ok, err := mcpDialAnchorClear(cctx, mcpSocketPath())
				if err != nil {
					return "", fmt.Errorf("zdev_anchor_clear: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("zdev_anchor_clear: daemon rejected the request")
				}
				return "unanchored", nil
			},
		},
		{
			Name:        "zdev_anchor",
			Description: "Read the operator's current focus state in one call: the anchor (title/project/since/elapsed) or \"unanchored\" when there is none, plus in_focus (anchored or inside a calendar commitment) and free_until (unix time of the next commitment, 0 if the rest of the day is clear). Use this before deciding whether to interrupt vs. park — e.g. prefer zdev_park over speaking up when anchored is true. Returns JSON.",
			InputSchema: objectSchema(map[string]any{}),
			run: func(ctx context.Context, _ map[string]any) (string, error) {
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				snap, err := mcpSnapshot(cctx, mcpSocketPath())
				if err != nil {
					return "", fmt.Errorf("zdev_anchor: %w", err)
				}
				out, err := anchorSnapshotJSON(snap, time.Now().Unix())
				if err != nil {
					return "", fmt.Errorf("zdev_anchor: marshal: %w", err)
				}
				return out, nil
			},
		},
		{
			Name:        "zdev_held",
			Description: "Read the held set: everything captured while the operator was anchored, awaiting its hearing at the next boundary review — arrivals (waits, finishes) and parked thoughts alike, chronological (oldest first). Returns a JSON array of {id, kind, title, project, age_sec}. Use this to reason over what's queued (e.g. shutdown reconciliation asking \"what's still held\"); it does not consume anything — see zdev_held_remove for that.",
			InputSchema: objectSchema(map[string]any{}),
			run: func(ctx context.Context, _ map[string]any) (string, error) {
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				snap, err := mcpSnapshot(cctx, mcpSocketPath())
				if err != nil {
					return "", fmt.Errorf("zdev_held: %w", err)
				}
				out, err := heldSnapshotJSON(snap, time.Now().Unix())
				if err != nil {
					return "", fmt.Errorf("zdev_held: marshal: %w", err)
				}
				return out, nil
			},
		},
		{
			Name:        "zdev_held_remove",
			Description: "Remove one item from the held set by id, or \"*\" to clear the whole set. Consuming an item is the OPERATOR'S act, normally made at the boundary review — only call this for an item a skill created itself (e.g. a park it just made and no longer needs) or one the operator explicitly asked to consume/clear. Never remove held items as a guess on the operator's behalf; that undermines the airlock's one guarantee (nothing deferred is lost until it's shown).",
			InputSchema: objectSchema(map[string]any{
				"id": map[string]any{"type": "string", "description": "The held item's id, or \"*\" to clear the entire held set."},
			}, "id"),
			run: func(ctx context.Context, args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				if strings.TrimSpace(id) == "" {
					return "", fmt.Errorf("zdev_held_remove requires non-empty 'id' (or \"*\" for all)")
				}
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				ok, err := mcpDialHeldRemove(cctx, mcpSocketPath(), id)
				if err != nil {
					return "", fmt.Errorf("zdev_held_remove: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("zdev_held_remove: daemon rejected the request")
				}
				return "removed: " + id, nil
			},
		},
		{
			Name:        "zdev_schedule_push",
			Description: "Push a source's calendar/run-sheet commitments into zdev's focus loop (docs/design/command-centre.md — \"The scheduled anchor and the push surface\"). Each call REPLACES this source's ENTIRE previous set wholesale — pass everything still valid, not a delta; an empty commitments array is how a source clears itself. Sources stay dumb; do the enrichment (titles, project mapping, filtering) BEFORE calling this — records pushed here are treated as already-decided. `at`/`until` are unix seconds (`until` omitted defaults to at+30m for display purposes). `source` must be a stable name you own (e.g. \"plan\") and must NOT be \"ics\" — that name is reserved for zdev's own calendar probe. Set a record's `kind` to \"task:<project>\" (e.g. \"task:zitcha/backend\", using zdev's canonical slash-form project name) to make that block anchor-eligible: while `now` falls inside such a block and the operator has no explicit anchor, zdev auto-anchors to it — overriding an inferred \"(auto)\" presence anchor, but NEVER an explicit pick, and never re-grabbing a block the operator explicitly overrode. Plain \"task\" (no project suffix) and any other kind (e.g. \"meeting\") are valid but never anchor-eligible.",
			InputSchema: objectSchema(map[string]any{
				"source": map[string]any{"type": "string", "description": "Stable name for this push's source, e.g. \"plan\". Must not be \"ics\" (reserved for the calendar probe)."},
				"commitments": map[string]any{
					"type":        "array",
					"description": "The FULL replacement set for this source — omit nothing still valid. Empty array clears the source.",
					"items": objectSchema(map[string]any{
						"id":    map[string]any{"type": "string", "description": "Stable per-source id — the dedup key. Re-pushing the same id updates that record."},
						"title": map[string]any{"type": "string", "description": "Display title."},
						"at":    map[string]any{"type": "integer", "description": "Unix seconds, block start."},
						"until": map[string]any{"type": "integer", "description": "Unix seconds, block end. Omit if unknown (defaults to at+30m for display)."},
						"kind":  map[string]any{"type": "string", "description": "Free-form, e.g. \"meeting\", \"focus\", or \"task:<project>\" to make the block anchor-eligible."},
						"url":   map[string]any{"type": "string", "description": "Optional join/reference link."},
					}, "id", "title", "at"),
				},
			}, "source", "commitments"),
			run: func(ctx context.Context, args map[string]any) (string, error) {
				source, _ := args["source"].(string)
				if strings.TrimSpace(source) == "" {
					return "", fmt.Errorf("zdev_schedule_push requires non-empty 'source'")
				}
				rawList, _ := args["commitments"].([]any)
				commitments := make([]proto.Commitment, 0, len(rawList))
				for _, raw := range rawList {
					m, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					var c proto.Commitment
					if id, ok := m["id"].(string); ok {
						c.ID = id
					}
					if title, ok := m["title"].(string); ok {
						c.Title = title
					}
					if at, ok := m["at"].(float64); ok {
						c.At = int64(at)
					}
					if until, ok := m["until"].(float64); ok {
						c.Until = int64(until)
					}
					if kind, ok := m["kind"].(string); ok {
						c.Kind = kind
					}
					if url, ok := m["url"].(string); ok {
						c.URL = url
					}
					commitments = append(commitments, c)
				}
				cctx, cancel := context.WithTimeout(ctx, mcpSocketTimeout)
				defer cancel()
				ok, err := mcpDialSchedulePush(cctx, mcpSocketPath(), source, commitments)
				if err != nil {
					return "", fmt.Errorf("zdev_schedule_push: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("zdev_schedule_push: daemon rejected the request")
				}
				return fmt.Sprintf("pushed %d commitment(s) for source %q", len(commitments), source), nil
			},
		},
	}
}

// mcpActuatorTools is the gated surface — tools that spawn/kill/mutate OTHER
// sessions. Registered only when mcpActuateEnabled(); see mcpActuateEnabled
// for why the default is off. Today the only actuator is `run`.
func mcpActuatorTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "run",
			Description: "Spawn a SUPERVISED agent loop in a project: opens a window running an agent seeded with <prompt>, supervised by zdev like any session. The actuator for triggers (a PR/Slack/cron event calls this). Returns the spawned target.",
			InputSchema: objectSchema(map[string]any{
				"project": map[string]any{"type": "string", "description": "Configured project name (e.g. zitcha/backend)."},
				"prompt":  map[string]any{"type": "string", "description": "The initial prompt or slash command (e.g. \"/rigorous-review #1234\")."},
				"name":    map[string]any{"type": "string", "description": "Optional window name; defaults to a slug of the prompt."},
			}, "project", "prompt"),
			run: func(ctx context.Context, args map[string]any) (string, error) {
				project, _ := args["project"].(string)
				prompt, _ := args["prompt"].(string)
				if project == "" || prompt == "" {
					return "", fmt.Errorf("run requires 'project' and 'prompt'")
				}
				// `--supervised` is the agreed contract with bin/zdev: run the
				// agent WITHOUT --dangerously-skip-permissions. `--` stops
				// bin/zdev's option parsing so a prompt beginning with '-' can
				// never be read as a flag. Both must precede the positionals.
				cli := []string{"run", "--supervised", "--", project, prompt}
				if name, _ := args["name"].(string); name != "" {
					cli = append(cli, "--name", name)
				}
				logActuatorFired("run", project, prompt)
				out, err := mcpExec(ctx, "zdev", cli...)
				if err != nil {
					return "", fmt.Errorf("zdev run: %w", err)
				}
				return string(out), nil
			},
		},
	}
}

// logActuatorFired records that an MCP actuator fired, for the forensic trail
// the C1 finding asks for (which project, a redacted prompt summary, that it
// came from MCP).
//
// It uses slog rather than the eventlog.Writer on purpose: `zdevd mcp` runs as
// its OWN process, and eventlog's single-writer goroutine (its file handle and
// rotation state) is owned by the DAEMON process — see internal/eventlog. From
// here a real eventlog record is out of reach without either a new daemon
// socket verb (internal/socket + the hub, owned elsewhere) or a second Writer
// racing the daemon on the same events.ndjson and its rotation, which would
// violate that single-writer invariant. slog.Info lands in the same structured
// log stream the daemon uses and is the correct fallback the task allows.
//
// The prompt is redacted to a bounded, single-line preview so a long or
// sensitive prompt is not spilled verbatim into the log while still leaving
// something identifiable for forensics.
func logActuatorFired(tool, project, prompt string) {
	slog.Info("mcp actuator fired",
		"source", "mcp",
		"tool", tool,
		"project", project,
		"prompt_chars", len(prompt),
		"prompt_preview", redactPrompt(prompt),
	)
}

// redactPrompt collapses a prompt to a bounded single-line preview: newlines
// folded to spaces, trimmed, and capped with an ellipsis. Pure.
func redactPrompt(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	const max = 80
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// mcpSubcmd implements `zdevd mcp`: the stdio JSON-RPC serve loop.
func mcpSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	httpAddr := fs.String("http", "", "serve MCP over HTTP at this address (e.g. 127.0.0.1:7399) instead of stdio; phase 2 binds this to a tailnet (tsnet) listener")
	insecureBind := fs.Bool("insecure-bind", false, "allow --http to bind a non-loopback address (off-host reachable); required to bind anything other than localhost/127.0.0.1")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tools := mcpTools()
	byName := make(map[string]mcpTool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	if *httpAddr != "" {
		return serveMCPHTTP(*httpAddr, *insecureBind, tools, byName)
	}
	return serveMCP(os.Stdin, os.Stdout, tools, byName)
}

// handleRPC dispatches one JSON-RPC request to its response. reply is false
// for notifications and other no-response cases, so both transports know to
// stay silent. Transport-agnostic on purpose: the stdio loop and the HTTP
// handler share this exact dispatch — and the tsnet (tailnet) listener in
// phase 2 reuses serveMCPHTTP unchanged.
func handleRPC(req rpcRequest, tools []mcpTool, byName map[string]mcpTool) (resp *rpcResponse, reply bool) {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		pv := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
		return &rpcResponse{ID: req.ID, Result: map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "zdev", "version": version},
		}}, true
	case "notifications/initialized", "notifications/cancelled":
		return nil, false
	case "ping":
		if isNotification {
			return nil, false
		}
		return &rpcResponse{ID: req.ID, Result: map[string]any{}}, true
	case "tools/list":
		return &rpcResponse{ID: req.ID, Result: map[string]any{"tools": tools}}, true
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		t, ok := byName[p.Name]
		if !ok {
			return &rpcResponse{ID: req.ID, Error: &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}}, true
		}
		text, err := t.run(context.Background(), p.Arguments)
		if err != nil {
			// Tool failure is reported in-band (isError), not as a transport
			// error — the model sees and can react to it.
			return &rpcResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
				"isError": true,
			}}, true
		}
		return &rpcResponse{ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}}, true
	default:
		if isNotification {
			return nil, false
		}
		return &rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}, true
	}
}

func serveMCP(in io.Reader, out io.Writer, tools []mcpTool, byName map[string]mcpTool) int {
	dec := json.NewDecoder(in)
	w := bufio.NewWriter(out)
	writeResp := func(resp *rpcResponse) {
		resp.JSONRPC = "2.0"
		b, err := json.Marshal(resp)
		if err != nil {
			return
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
		_ = w.Flush()
	}
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return 0
			}
			// Malformed frame — JSON-RPC parse error, no id to correlate.
			writeResp(&rpcResponse{Error: &rpcError{Code: -32700, Message: "parse error"}})
			return 1
		}
		if resp, reply := handleRPC(req, tools, byName); reply {
			writeResp(resp)
		}
	}
}

// mcpTokenPath is where a generated bearer token is persisted (0600) when
// ZDEV_MCP_TOKEN is unset. It sits beside the daemon's other state under the
// platform data dir so it survives reboots and is resolvable by whoever needs
// to read the token back (the operator wiring an MCP client).
func mcpTokenPath() string { return filepath.Join(platform.DataDir(), "mcp-token") }

// resolveMCPToken returns the HTTP bearer token and a human-readable note of
// where it came from. ZDEV_MCP_TOKEN wins when set; otherwise a 256-bit token
// is generated with crypto/rand and written 0600 to mcpTokenPath().
func resolveMCPToken() (token, source string, err error) {
	if v := os.Getenv("ZDEV_MCP_TOKEN"); v != "" {
		return v, "ZDEV_MCP_TOKEN", nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	p := mcpTokenPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir token dir: %w", err)
	}
	if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
		return "", "", fmt.Errorf("write token %s: %w", p, err)
	}
	return tok, "generated → " + p, nil
}

// isLoopbackBind reports whether addr binds only the loopback interface. An
// empty host ("" or ":7399") means all interfaces and is NOT loopback.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// originAllowed enforces the loopback Origin allowlist. A legitimate MCP
// client sends no Origin; a browser always does, so a same-simple-request
// attack from a web page carries a non-loopback (or null) Origin and is
// refused. Empty Origin passes (non-browser client).
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, p := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]"} {
		if origin == p || strings.HasPrefix(origin, p+":") || strings.HasPrefix(origin, p+"/") {
			return true
		}
	}
	return false
}

// bearerOK constant-time-compares the request's Authorization bearer against
// the expected token.
func bearerOK(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// mcpHTTPHandler builds the hardened /mcp handler. Split out from
// serveMCPHTTP so the auth/origin/content-type paths are testable with
// httptest without binding a socket. Check order is cheap-rejections first
// (method, Origin, Content-Type) then the bearer, so a browser probe never
// even reaches the token comparison.
func mcpHTTPHandler(token string, tools []mcpTool, byName map[string]mcpTool) http.HandlerFunc {
	return func(wr http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(wr, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !originAllowed(r.Header.Get("Origin")) {
			http.Error(wr, "forbidden origin", http.StatusForbidden)
			return
		}
		// Reject the CORS-simple bypass: text/plain is sendable cross-origin
		// without a preflight, application/json is not.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			http.Error(wr, "Content-Type: application/json required", http.StatusUnsupportedMediaType)
			return
		}
		if !bearerOK(r, token) {
			http.Error(wr, "unauthorized", http.StatusUnauthorized)
			return
		}
		defer r.Body.Close()
		var req rpcRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			wr.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(wr).Encode(&rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		resp, reply := handleRPC(req, tools, byName)
		if !reply {
			wr.WriteHeader(http.StatusAccepted)
			return
		}
		resp.JSONRPC = "2.0"
		wr.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(wr).Encode(resp)
	}
}

// serveMCPHTTP serves the SAME dispatch over a minimal Streamable-HTTP
// transport: one POST endpoint that takes a single JSON-RPC message and
// returns its response (202 for a notification). Server-initiated SSE streams
// aren't needed yet — every tool is request/response — so this is the minimal
// compliant shape Claude Code's `--transport http` consumes. Phase 2 swaps
// this listener for a tsnet (tailnet-only) one; the handler is unchanged.
//
// It is hardened per the H4 finding: a non-loopback bind is refused unless
// insecureBind is set (and warns loudly when it is), and every request must
// carry a matching bearer token, a loopback Origin (or none), and an
// application/json Content-Type — see mcpHTTPHandler.
func serveMCPHTTP(addr string, insecureBind bool, tools []mcpTool, byName map[string]mcpTool) int {
	if !isLoopbackBind(addr) {
		if !insecureBind {
			fmt.Fprintf(os.Stderr, "zdevd mcp: refusing to bind non-loopback address %q without --insecure-bind (the control plane would be reachable off-host)\n", addr)
			return 2
		}
		slog.Warn("zdevd mcp: binding a NON-LOOPBACK address; the control plane is reachable off-host", "addr", addr)
		fmt.Fprintf(os.Stderr, "zdevd mcp: WARNING: binding non-loopback %q (--insecure-bind) — the control plane is reachable off-host\n", addr)
	}
	token, source, err := resolveMCPToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd mcp: bearer token: %v\n", err)
		return 1
	}
	slog.Info("zdevd mcp: HTTP bearer token ready", "source", source)
	fmt.Fprintf(os.Stderr, "zdevd mcp: HTTP bearer token (%s)\n", source)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", mcpHTTPHandler(token, tools, byName))
	fmt.Fprintf(os.Stderr, "zdevd mcp: serving HTTP at %s/mcp\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd mcp: http serve: %v\n", err)
		return 1
	}
	return 0
}
