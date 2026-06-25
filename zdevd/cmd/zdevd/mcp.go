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
//   run          -> zdev run <project> "<prompt>"   (the actuator verb)

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// mcpTools is the registry. Read tools shell to zdev-show --json; run shells
// to the zdev actuator verb.
func mcpTools() []mcpTool {
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
				cli := []string{"run", project, prompt}
				if name, _ := args["name"].(string); name != "" {
					cli = append(cli, "--name", name)
				}
				out, err := mcpExec(ctx, "zdev", cli...)
				if err != nil {
					return "", fmt.Errorf("zdev run: %w", err)
				}
				return string(out), nil
			},
		},
	}
}

// mcpSubcmd implements `zdevd mcp`: the stdio JSON-RPC serve loop.
func mcpSubcmd(_ []string) int {
	tools := mcpTools()
	byName := make(map[string]mcpTool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return serveMCP(os.Stdin, os.Stdout, tools, byName)
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
			writeResp(&rpcResponse{ID: req.ID, Result: map[string]any{
				"protocolVersion": pv,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "zdev", "version": version},
			}})
		case "notifications/initialized", "notifications/cancelled":
			// fire-and-forget; no response
		case "ping":
			if !isNotification {
				writeResp(&rpcResponse{ID: req.ID, Result: map[string]any{}})
			}
		case "tools/list":
			writeResp(&rpcResponse{ID: req.ID, Result: map[string]any{"tools": tools}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			t, ok := byName[p.Name]
			if !ok {
				writeResp(&rpcResponse{ID: req.ID, Error: &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}})
				break
			}
			text, err := t.run(context.Background(), p.Arguments)
			if err != nil {
				// Tool failure is reported in-band (isError), not as a
				// transport error — the model sees and can react to it.
				writeResp(&rpcResponse{ID: req.ID, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
					"isError": true,
				}})
				break
			}
			writeResp(&rpcResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			}})
		default:
			if !isNotification {
				writeResp(&rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
			}
		}
	}
}
