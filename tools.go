package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func schema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func textParam(description string) any {
	return map[string]any{"type": "string", "description": description}
}
func (a *App) tools(ctx context.Context) ([]ToolDefinition, mcpRegistry) {
	p := map[string]any{"expression": textParam("Arithmetic using + - * / and parentheses")}
	defs := []ToolDefinition{
		defineTool("clock_now", "Get the current UTC timestamp", schema(map[string]any{})),
		defineTool("math_calculate", "Evaluate a numeric arithmetic expression", schema(p, "expression")),
		defineTool("web_fetch", "Fetch public HTTPS web content (read-only, bounded, no redirects)", schema(map[string]any{"url": textParam("Public HTTPS URL")}, "url")),
		defineTool("artifact_inspect", "Inspect an uploaded artifact's metadata", schema(map[string]any{"id": textParam("Artifact identifier")}, "id")),
		defineTool("delegate_agent", "Send one task to another configured model (no recursive tools)", schema(map[string]any{"model": textParam("Configured model profile name"), "task": textParam("Narrow delegated task")}, "model", "task")),
	}
	if a.config.DataFrameworks.BaseURL != "" {
		defs = append(defs, defineTool("data_capabilities", "Inspect connected AEGIS DATA Frameworks capabilities", schema(map[string]any{})))
		defs = append(defs, defineTool("data_readiness", "Inspect connected AEGIS DATA Frameworks acceptance coverage", schema(map[string]any{})))
	}
	mcp := a.config.loadMCP(ctx)
	defs = append(defs, mcp.defs...)
	return defs, mcp
}
func calc(expr string) (float64, error) {
	if len(expr) > 256 {
		return 0, errors.New("expression exceeds 256 characters")
	}
	tree, e := parser.ParseExpr(expr)
	if e != nil {
		return 0, e
	}
	var walk func(ast.Expr, int) (float64, error)
	walk = func(n ast.Expr, d int) (float64, error) {
		if d > 32 {
			return 0, errors.New("expression nesting exceeded")
		}
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.INT && x.Kind != token.FLOAT {
				return 0, errors.New("numeric literals only")
			}
			return strconv.ParseFloat(x.Value, 64)
		case *ast.ParenExpr:
			return walk(x.X, d+1)
		case *ast.UnaryExpr:
			v, e := walk(x.X, d+1)
			if e != nil {
				return 0, e
			}
			if x.Op == token.SUB {
				return -v, nil
			}
			if x.Op == token.ADD {
				return v, nil
			}
			return 0, errors.New("invalid unary operation")
		case *ast.BinaryExpr:
			a, e := walk(x.X, d+1)
			if e != nil {
				return 0, e
			}
			b, e := walk(x.Y, d+1)
			if e != nil {
				return 0, e
			}
			switch x.Op {
			case token.ADD:
				return a + b, nil
			case token.SUB:
				return a - b, nil
			case token.MUL:
				return a * b, nil
			case token.QUO:
				if b == 0 {
					return 0, errors.New("division by zero")
				}
				return a / b, nil
			}
			return 0, errors.New("unsupported operator")
		default:
			return 0, errors.New("arbitrary expressions are not allowed")
		}
	}
	v, e := walk(tree, 0)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("non-finite result")
	}
	return v, e
}
func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast()
}
func webFetch(ctx context.Context, raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("web_fetch requires a public HTTPS URL")
	}
	if net.ParseIP(u.Hostname()) != nil {
		return "", errors.New("IP-literal URLs prohibited")
	}
	resolve, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return "", err
	}
	if len(resolve) == 0 {
		return "", errors.New("host not resolved")
	}
	for _, ip := range resolve {
		if !publicIP(ip.IP) {
			return "", errors.New("private or reserved destination prohibited")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, DialContext: func(c context.Context, network, address string) (net.Conn, error) {
		_, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		return dialer.DialContext(c, "tcp", net.JoinHostPort(resolve[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 12 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects disabled") }}
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Accept", "text/html,text/plain,application/json")
	res, e := client.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	contentType := res.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/") && !strings.Contains(contentType, "json") {
		return "", errors.New("non-text content; use an MCP media adapter")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 128<<10))
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("URL: %s\nContent-Type: %s\nBody (up to 128 KiB):\n%s", u.String(), contentType, string(b)), nil
}
func (a *App) readData(ctx context.Context, route string) (string, error) {
	d := a.config.DataFrameworks
	req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(d.BaseURL, "/")+route, nil)
	if e != nil {
		return "", e
	}
	if d.APIKeyEnv != "" {
		v := os.Getenv(d.APIKeyEnv)
		if v == "" {
			return "", fmt.Errorf("DATA credential %s is unset", d.APIKeyEnv)
		}
		req.Header.Set("Authorization", "Bearer "+v)
	}
	cli := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect disabled") }}
	res, e := cli.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("DATA API HTTP %d", res.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(res.Body, 128<<10))
	return string(data), e
}
func (a *App) executeTool(ctx context.Context, call ToolCall, reg mcpRegistry) (string, error) {
	if len(call.Function.Arguments) > 32768 {
		return "", errors.New("tool arguments exceed limit")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	get := func(k string) (string, error) {
		v, ok := args[k].(string)
		if !ok || v == "" {
			return "", fmt.Errorf("missing string argument %s", k)
		}
		return v, nil
	}
	switch call.Function.Name {
	case "clock_now":
		return time.Now().UTC().Format(time.RFC3339Nano), nil
	case "math_calculate":
		s, e := get("expression")
		if e != nil {
			return "", e
		}
		v, e := calc(s)
		if e != nil {
			return "", e
		}
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case "web_fetch":
		s, e := get("url")
		if e != nil {
			return "", e
		}
		return webFetch(ctx, s)
	case "data_capabilities":
		if a.config.DataFrameworks.BaseURL == "" {
			return "", errors.New("DATA adapter not configured")
		}
		return a.readData(ctx, "/v1/capabilities")
	case "data_readiness":
		if a.config.DataFrameworks.BaseURL == "" {
			return "", errors.New("DATA adapter not configured")
		}
		return a.readData(ctx, "/v1/readiness")
	case "artifact_inspect":
		id, e := get("id")
		if e != nil {
			return "", e
		}
		v, ok := a.artifacts.get(id)
		if !ok {
			return "", errors.New("artifact not found")
		}
		b, _ := json.Marshal(v)
		return string(b), nil
	case "delegate_agent":
		name, e := get("model")
		if e != nil {
			return "", e
		}
		task, e := get("task")
		if e != nil {
			return "", e
		}
		if len(task) > 12000 {
			return "", errors.New("task too long")
		}
		p, ok := a.config.model(name)
		if !ok {
			return "", fmt.Errorf("unknown expert model: %s", name)
		}
		out, e := modelGenerate(ctx, p, []Message{{Role: "system", Content: "You are a delegated specialist. Complete only the assigned task and return a concise factual result. No tools are available."}, {Role: "user", Content: task}}, nil, "auto", func(string, string) {})
		if e != nil {
			return "", e
		}
		if len(out.ToolCalls) > 0 {
			return "", errors.New("delegated model attempted unavailable tools")
		}
		return out.Content, nil
	default:
		t, ok := reg.targets[call.Function.Name]
		if !ok {
			return "", errors.New("tool not registered")
		}
		raw, e := callMCP(ctx, t, args)
		if e != nil {
			return "", e
		}
		if len(raw) > 128<<10 {
			return "", errors.New("tool output exceeds limit")
		}
		return string(raw), nil
	}
}
func approveByDefault(name, mode string, safe map[string]bool) bool {
	switch name {
	case "clock_now", "math_calculate", "artifact_inspect", "data_capabilities", "data_readiness":
		return true
	case "web_fetch":
		return mode == "autopilot"
	case "delegate_agent":
		return mode == "autopilot"
	default:
		return mode == "autopilot" && safe[name]
	}
}
func artifactID(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
