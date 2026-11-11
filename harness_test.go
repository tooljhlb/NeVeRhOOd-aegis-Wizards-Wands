package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	c := demoConfig()
	c.DataDir = t.TempDir()
	a, e := newApp(c, "test-secret")
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func TestNumericSandbox(t *testing.T) {
	for _, tt := range []struct {
		expr string
		want float64
	}{{"2+3*4", 14}, {"(10-4)/3", 2}, {"-2.5+3", .5}} {
		got, e := calc(tt.expr)
		if e != nil || got != tt.want {
			t.Fatalf("%q: %v %v", tt.expr, got, e)
		}
	}
	for _, s := range []string{"1/0", "os.Remove(\"/\")", "1<<40", "foo+1", "(2+3", strings.Repeat("1", 300)} {
		if _, e := calc(s); e == nil {
			t.Fatalf("unsafe expression accepted %q", s)
		}
	}
}
func TestConfigEndpointPolicies(t *testing.T) {
	c := demoConfig()
	c.DataDir = t.TempDir()
	if _, e := loadConfig(""); e != nil {
		t.Fatal(e)
	}
	for _, url := range []string{"http://127.0.0.1:11434/v1", "http://localhost:1234/v1", "https://api.example.com/v1"} {
		if e := validateConfiguredEndpoint(url); e != nil {
			t.Fatalf("%s %v", url, e)
		}
	}
	for _, url := range []string{"http://evil.example.com/v1", "file:///etc/passwd", "http://user:pass@localhost:8000"} {
		if e := validateConfiguredEndpoint(url); e == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	if publicIP(netParse("127.0.0.1")) || publicIP(netParse("10.8.0.1")) {
		t.Fatal("private IP marked public")
	}
}
func netParse(s string) net.IP { return net.ParseIP(s) }
func TestModelStreamToolCallFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		var req map[string]any
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		if req["stream"] != true {
			t.Error("expected stream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, text := range []string{
			`{"choices":[{"delta":{"content":"Hello "}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"checking"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"id1","function":{"name":"math_","arguments":"{\\\"expression\\\":\\\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"calculate","arguments":"2+2\\\"}"}}]}}],"choices_unused":0}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", text)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	profile := ModelProfile{Name: "local", Provider: "openai-compatible", BaseURL: server.URL + "/v1", Model: "fake"}
	var out strings.Builder
	r, e := modelGenerate(context.Background(), profile, []Message{{Role: "user", Content: "hello"}}, nil, "auto", func(k, d string) { out.WriteString(k + ":" + d + "|") })
	if e != nil {
		t.Fatal(e)
	}
	if r.Content != "Hello " || r.Reasoning != "checking" || !strings.Contains(out.String(), "content:Hello ") {
		t.Fatalf("bad stream %+v %s", r, out.String())
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Function.Name != "math_calculate" {
		t.Fatalf("tool chunks %+v", r.ToolCalls)
	}
}
func TestMCPRemoteCatalogAndCall(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("MCP-Protocol-Version") != "2026-07-28" {
			t.Errorf("version header %q", r.Header.Get("MCP-Protocol-Version"))
		}
		var msg struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "application/json")
		switch msg.Method {
		case "tools/list":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"x","result":{"tools":[{"name":"ping","description":"Ping","inputSchema":{"type":"object","properties":{}}}]}}`)
		case "tools/call":
			called = true
			if r.Header.Get("Mcp-Name") != "ping" {
				t.Error("missing name header")
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"x","result":{"content":[{"type":"text","text":"pong"}]}}`)
		default:
			t.Errorf("unexpected method %s", msg.Method)
		}
	}))
	defer server.Close()
	a := testApp(t)
	a.config.MCPServers = []MCPProfile{{Name: "test", URL: server.URL, SafeTools: []string{"ping"}}}
	reg := a.config.loadMCP(context.Background())
	if len(reg.defs) != 1 || !reg.safe["mcp__test__ping"] {
		t.Fatalf("catalog %+v", reg)
	}
	out, e := a.executeTool(context.Background(), ToolCall{ID: "x", Function: FunctionCall{Name: "mcp__test__ping", Arguments: "{}"}}, reg)
	if e != nil || !called || !strings.Contains(out, "pong") {
		t.Fatalf("MCP failed %q %v", out, e)
	}
}
func TestSessionStreamApprovalAndReplay(t *testing.T) {
	a := testApp(t)
	s := a.store.create("demo", "chat")
	var ev []Event
	e := a.runTurn(context.Background(), s, &ChatInput{Message: "Hello", Model: "demo", Mode: "chat", Thinking: "auto"}, nil, func(e Event) { ev = append(ev, e) })
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Messages) != 2 || len(ev) == 0 || s.Messages[1].Role != "assistant" {
		t.Fatalf("incomplete turn %+v", s.Messages)
	}
	if e = a.runTurn(context.Background(), s, &ChatInput{Message: "[tool:clock.now]", Mode: "chat"}, nil, nil); e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, m := range s.Messages {
		if m.Role == "tool" {
			count++
		}
	}
	if count != 1 || s.Pending != nil {
		t.Fatalf("tool not executed count=%d pending=%+v", count, s.Pending)
	}
	if _, ok := a.store.get(s.ID); !ok {
		t.Fatal("session lost")
	}
	recovered, e := newStore(a.config.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	rs, ok := recovered.get(s.ID)
	if !ok || len(rs.Events) != len(s.Events) {
		t.Fatal("events not persisted")
	}
	if replay := recovered.replay(rs, 2); len(replay) != len(rs.Events)-2 {
		t.Fatal("bad replay")
	}
}
func TestApprovalPolicyAndFlow(t *testing.T) {
	if approveByDefault("web_fetch", "chat", nil) || approveByDefault("mcp__test__write", "autopilot", nil) {
		t.Fatal("side effect auto-approved")
	}
	if !approveByDefault("mcp__test__read", "autopilot", map[string]bool{"mcp__test__read": true}) {
		t.Fatal("explicitly safe MCP tool denied")
	}
	a := testApp(t)
	s := a.store.create("demo", "chat")
	call := ToolCall{ID: "fetch1", Function: FunctionCall{Name: "web_fetch", Arguments: `{"url":"https://example.com"}`}}
	e := a.handleCalls(context.Background(), s, []ToolCall{call}, 0, "chat", 1, mcpRegistry{safe: map[string]bool{}}, nil, func(string, any) {})
	if e != nil || s.Pending == nil {
		t.Fatal("expected approval required", e)
	}
	if e = a.handleCalls(context.Background(), s, s.Pending.Calls, 0, "chat", 1, mcpRegistry{safe: map[string]bool{}}, boolPtr(false), func(string, any) {}); e != nil {
		t.Fatal(e)
	}
	if s.Messages[0].Content != "Denied by human approval policy." {
		t.Fatal("denial not recorded")
	}
	spec := FlowSpec{Name: "Research and summarize", Steps: []FlowStep{{ID: "gather", Model: "demo", Task: "Find data"}, {ID: "synthesize", Model: "demo", Task: "Summarize", DependsOn: []string{"gather"}}}}
	if e := validateFlow(spec, a.config); e != nil {
		t.Fatal(e)
	}
	v, e := a.executeFlow(context.Background(), spec, nil)
	if e != nil || v.Status != "completed" || len(v.Outputs) != 2 {
		t.Fatalf("flow %+v %v", v, e)
	}
	v2, e := newFlowStore(a.config.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := v2.get(v.ID); !ok {
		t.Fatal("flow not durable")
	}
	spec.Steps[0].DependsOn = []string{"synthesize"}
	if e = validateFlow(spec, a.config); e == nil {
		t.Fatal("cycle accepted")
	}
}
func boolPtr(v bool) *bool { return &v }
func TestHTTPAuthAndChat(t *testing.T) {
	a := testApp(t)
	srv := httptest.NewServer(a)
	defer srv.Close()
	res, e := http.Get(srv.URL + "/api/status")
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("unauthorized request succeeded")
	}
	request := func(method, uri, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+uri, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set("Content-Type", "application/json")
		r, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	create := request("POST", "/api/sessions", `{"model":"demo","mode":"chat"}`)
	if create.StatusCode != 201 {
		t.Fatal(create.Status)
	}
	var s Session
	_ = json.NewDecoder(create.Body).Decode(&s)
	create.Body.Close()
	reply := request("POST", "/api/sessions/"+s.ID+"/chat", `{"message":"hello","model":"demo","mode":"chat"}`)
	b, _ := io.ReadAll(reply.Body)
	reply.Body.Close()
	if !bytes.Contains(b, []byte("turn.completed")) {
		t.Fatalf("no completed SSE event: %s", b)
	}
	replay := request("GET", "/api/sessions/"+s.ID+"/events?after=0", "")
	var events []Event
	_ = json.NewDecoder(replay.Body).Decode(&events)
	replay.Body.Close()
	if len(events) < 3 {
		t.Fatalf("missing replay events %d", len(events))
	}
	status := request("GET", "/api/status", "")
	if status.StatusCode != 200 {
		t.Fatal(status.Status)
	}
	status.Body.Close()
}
func TestStorageRecoveryInterruptedFlow(t *testing.T) {
	dir := t.TempDir()
	st, e := newFlowStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	v := &FlowState{ID: newID(), Status: "running", Created: time.Now(), Outputs: map[string]string{"ok": "done"}, Steps: map[string]string{"ok": "completed"}}
	if e = st.save(v); e != nil {
		t.Fatal(e)
	}
	s, e := newFlowStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	v2, ok := s.get(v.ID)
	if !ok || v2.Status != "interrupted" || v2.Outputs["ok"] != "done" {
		t.Fatal("lost recovery state")
	}
}
func TestJSONArtifacts(t *testing.T) {
	a := testApp(t)
	dir := filepath.Join(a.config.DataDir, "artifacts")
	if _, e := os.Stat(dir); e != nil {
		t.Fatal(e)
	}
	if _, ok := a.artifacts.get("../../etc/passwd"); ok {
		t.Fatal("path accepted")
	}
}
