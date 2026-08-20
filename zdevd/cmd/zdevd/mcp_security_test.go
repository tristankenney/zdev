package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hasTool reports whether tools/list (via the real gated registry) advertises
// a tool by name.
func hasTool(name string) bool {
	for _, t := range mcpTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func TestMCP_ActuatorGating(t *testing.T) {
	cases := []struct {
		name        string
		actuate     string // value for ZDEV_MCP_ACTUATE; "" means unset
		wantRun     bool
		wantReadAll bool // read-only tools always present regardless
	}{
		{name: "unset → run absent", actuate: "", wantRun: false, wantReadAll: true},
		{name: "0 → run absent", actuate: "0", wantRun: false, wantReadAll: true},
		{name: "1 → run present", actuate: "1", wantRun: true, wantReadAll: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.actuate == "" {
				t.Setenv("ZDEV_MCP_ACTUATE", "")
			} else {
				t.Setenv("ZDEV_MCP_ACTUATE", tc.actuate)
			}
			if got := hasTool("run"); got != tc.wantRun {
				t.Errorf("run present = %v, want %v", got, tc.wantRun)
			}
			// The read-only surface must never depend on the gate.
			for _, name := range []string{"fleet_status", "triage", "review", "zdev_park", "zdev_anchor", "zdev_held", "zdev_schedule_push"} {
				if !hasTool(name) {
					t.Errorf("read-only tool %q missing (gate must not affect it)", name)
				}
			}
		})
	}
}

// testHandler builds the hardened HTTP handler over the real read-only tool
// set (no actuator, no exec needed — tools/list is pure dispatch).
func testHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	tools := mcpReadTools()
	byName := map[string]mcpTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	return mcpHTTPHandler(token, tools, byName)
}

func TestMCP_HTTPAuth(t *testing.T) {
	const token = "s3cret-token"
	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	cases := []struct {
		name        string
		method      string
		origin      string
		contentType string
		authz       string
		wantStatus  int
	}{
		{
			name: "happy path", method: http.MethodPost,
			contentType: "application/json", authz: "Bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name: "happy path with loopback origin", method: http.MethodPost,
			origin: "http://localhost:7399", contentType: "application/json", authz: "Bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name: "no token → 401", method: http.MethodPost,
			contentType: "application/json",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name: "wrong token → 401", method: http.MethodPost,
			contentType: "application/json", authz: "Bearer nope",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "non-loopback origin → 403", method: http.MethodPost,
			origin: "http://evil.example.com", contentType: "application/json", authz: "Bearer " + token,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "text/plain content-type → 415 (CORS-simple bypass)", method: http.MethodPost,
			contentType: "text/plain", authz: "Bearer " + token,
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "missing content-type → 415", method: http.MethodPost,
			authz:      "Bearer " + token,
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "GET → 405", method: http.MethodGet,
			contentType: "application/json", authz: "Bearer " + token,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	h := testHandler(t, token)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/mcp", strings.NewReader(body))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK && !strings.Contains(rec.Body.String(), "fleet_status") {
				t.Errorf("happy-path body did not carry the tool list: %q", rec.Body.String())
			}
		})
	}
}

func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7399", true},
		{"localhost:7399", true},
		{"[::1]:7399", true},
		{"0.0.0.0:7399", false},
		{":7399", false},
		{"192.168.1.5:7399", false},
		{"10.0.0.1:7399", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isLoopbackBind(tc.addr); got != tc.want {
				t.Errorf("isLoopbackBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true}, // non-browser client sends none
		{"http://localhost", true},
		{"http://localhost:7399", true},
		{"http://127.0.0.1:7399", true},
		{"http://[::1]:7399", true},
		{"http://evil.example.com", false},
		{"https://localhost:7399", false}, // https not in the allowlist
		{"http://localhost.evil.com", false},
		{"null", false},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			if got := originAllowed(tc.origin); got != tc.want {
				t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

func TestResolveMCPToken_EnvWins(t *testing.T) {
	t.Setenv("ZDEV_MCP_TOKEN", "from-env")
	tok, source, err := resolveMCPToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "from-env" {
		t.Errorf("token = %q, want from-env", tok)
	}
	if source != "ZDEV_MCP_TOKEN" {
		t.Errorf("source = %q, want ZDEV_MCP_TOKEN", source)
	}
}

func TestResolveMCPToken_GeneratesAndPersists(t *testing.T) {
	t.Setenv("ZDEV_MCP_TOKEN", "")
	// Point the platform data dir at a temp dir so the write is hermetic.
	// DataDir composes from these env roots on both platforms.
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("HOME", tmp)

	tok, source, err := resolveMCPToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok) != 64 { // 32 random bytes hex-encoded
		t.Errorf("token length = %d, want 64 hex chars", len(tok))
	}
	if !strings.HasPrefix(source, "generated") {
		t.Errorf("source = %q, want a generated-file note", source)
	}
	// A second call regenerates (fresh token) but must still succeed and write.
	tok2, _, err := resolveMCPToken()
	if err != nil {
		t.Fatalf("second resolve errored: %v", err)
	}
	if tok2 == "" {
		t.Error("second resolve produced empty token")
	}
}

func TestRedactPrompt(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"short prompt", "short prompt"},
		{"multi\nline\nprompt", "multi line prompt"},
		{"  padded  ", "padded"},
		{strings.Repeat("a", 100), strings.Repeat("a", 80) + "…"},
	}
	for _, tc := range cases {
		if got := redactPrompt(tc.in); got != tc.want {
			t.Errorf("redactPrompt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
