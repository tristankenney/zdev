package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// drive feeds newline-delimited JSON-RPC requests through serveMCP with a
// stubbed exec seam, returning the decoded responses in order.
func drive(t *testing.T, requests ...string) []rpcResponse {
	t.Helper()
	tools := mcpTools()
	byName := map[string]mcpTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	serveMCP(in, &out, tools, byName)

	var resps []rpcResponse
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r rpcResponse
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		resps = append(resps, r)
	}
	return resps
}

func TestMCP_InitializeAndList(t *testing.T) {
	t.Setenv("ZDEV_MCP_ACTUATE", "1") // actuator gating is exercised separately; here we want `run` advertised
	resps := drive(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification → no response
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses (notification suppressed), got %d", len(resps))
	}
	// initialize echoes the client's protocol version + advertises tools.
	init, _ := json.Marshal(resps[0].Result)
	if !strings.Contains(string(init), `"protocolVersion":"2025-06-18"`) {
		t.Errorf("initialize did not echo protocol version: %s", init)
	}
	if !strings.Contains(string(init), `"name":"zdev"`) {
		t.Errorf("initialize missing serverInfo: %s", init)
	}
	// tools/list exposes the four tools with run last carrying a real schema.
	list, _ := json.Marshal(resps[1].Result)
	for _, want := range []string{"fleet_status", "triage", "review", "run", `"required":["project","prompt"]`} {
		if !strings.Contains(string(list), want) {
			t.Errorf("tools/list missing %q: %s", want, list)
		}
	}
}

func TestMCP_ToolCall_ReadAndRun(t *testing.T) {
	t.Setenv("ZDEV_MCP_ACTUATE", "1") // register the `run` actuator for this test
	// Stub the exec seam: record calls, return canned output.
	var calls [][]string
	mcpExec = func(_ context.Context, bin string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{bin}, args...))
		return []byte("STUB-OUTPUT"), nil
	}
	t.Cleanup(func() { mcpExec = defaultMCPExec })

	resps := drive(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run","arguments":{"project":"zitcha/backend","prompt":"/rigorous-review #1234"}}}`,
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resps))
	}
	for i, r := range resps {
		body, _ := json.Marshal(r.Result)
		if !strings.Contains(string(body), "STUB-OUTPUT") {
			t.Errorf("response %d missing tool output: %s", i, body)
		}
		if strings.Contains(string(body), `"isError":true`) {
			t.Errorf("response %d unexpectedly errored: %s", i, body)
		}
	}
	// review → zdev-show review --json ; run → zdev run <project> <prompt>
	if len(calls) != 2 {
		t.Fatalf("want 2 exec calls, got %d: %v", len(calls), calls)
	}
	if got := strings.Join(calls[0], " "); got != "zdev-show review --json" {
		t.Errorf("review exec = %q", got)
	}
	if got := strings.Join(calls[1], " "); got != "zdev run --supervised -- zitcha/backend /rigorous-review #1234" {
		t.Errorf("run exec = %q", got)
	}
}

func TestMCP_ToolError_IsInBand(t *testing.T) {
	mcpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { mcpExec = defaultMCPExec })

	resps := drive(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"triage","arguments":{}}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	// A failing tool must NOT be a JSON-RPC transport error — it's isError:true
	// content so the model sees and can react to it.
	if resps[0].Error != nil {
		t.Errorf("tool failure leaked as transport error: %+v", resps[0].Error)
	}
	body, _ := json.Marshal(resps[0].Result)
	if !strings.Contains(string(body), `"isError":true`) {
		t.Errorf("tool failure not reported in-band: %s", body)
	}
}

func TestMCP_UnknownMethodAndTool(t *testing.T) {
	resps := drive(t,
		`{"jsonrpc":"2.0","id":1,"method":"bogus/method"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resps))
	}
	if resps[0].Error == nil || resps[0].Error.Code != -32601 {
		t.Errorf("unknown method should be -32601, got %+v", resps[0].Error)
	}
	if resps[1].Error == nil || resps[1].Error.Code != -32602 {
		t.Errorf("unknown tool should be -32602, got %+v", resps[1].Error)
	}
}
