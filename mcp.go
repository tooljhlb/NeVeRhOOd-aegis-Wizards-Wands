package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// A narrowly scoped Streamable HTTP MCP client. It supports tools/list and
// tools/call. Sampling, subscriptions, resources and stdio are not implemented.
const defaultMCPVersion = "2026-07-28"

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type mcTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}
type mcpRegistry struct {
	defs    []ToolDefinition
	targets map[string]mcpTarget
	safe    map[string]bool
}
type mcpTarget struct {
	profile MCPProfile
	tool    string
}

func mcpVersion(p MCPProfile) string {
	if p.ProtocolVersion != "" {
		return p.ProtocolVersion
	}
	return defaultMCPVersion
}
func mcMeta(p MCPProfile) map[string]any {
	return map[string]any{"io.modelcontextprotocol/protocolVersion": mcpVersion(p), "io.modelcontextprotocol/clientInfo": map[string]any{"name": "wizards-wands", "version": "0.1.0"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
}
func mcpRequest(ctx context.Context, profile MCPProfile, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = mcMeta(profile)
	payload := map[string]any{"jsonrpc": "2.0", "id": newID(), "method": method, "params": params}
	buf, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, "POST", profile.URL, bytes.NewReader(buf))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpVersion(profile))
	req.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		if name, _ := params["name"].(string); name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	if profile.TokenEnv != "" {
		v := os.Getenv(profile.TokenEnv)
		if v == "" {
			return nil, fmt.Errorf("MCP credential %s is unset", profile.TokenEnv)
		}
		req.Header.Set("Authorization", "Bearer "+v)
	}
	client := &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirects disabled") }}
	res, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return nil, fmt.Errorf("MCP HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var result json.RawMessage
	parse := func(data []byte) error {
		var r mcpResponse
		if e := json.Unmarshal(data, &r); e != nil {
			return e
		}
		if r.Error != nil {
			return fmt.Errorf("MCP JSON-RPC %d: %s", r.Error.Code, r.Error.Message)
		}
		if r.Result != nil {
			result = append(json.RawMessage{}, r.Result...)
		}
		return nil
	}
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		e = readSSE(io.LimitReader(res.Body, 8<<20), parse)
	} else {
		b, e2 := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if e2 != nil {
			return nil, e2
		}
		e = parse(b)
	}
	if e != nil {
		return nil, e
	}
	if result == nil {
		return nil, errors.New("MCP response contained no result")
	}
	return result, nil
}
func (c Config) loadMCP(ctx context.Context) mcpRegistry {
	out := mcpRegistry{defs: []ToolDefinition{}, targets: map[string]mcpTarget{}, safe: map[string]bool{}}
	for _, server := range c.MCPServers {
		// Tools are optional: a failed server cannot block core chat. Server errors
		// are visible through /api/connectors, not silently declared successful.
		var cursor string
		for pages := 0; pages < 6; pages++ {
			params := map[string]any{}
			if cursor != "" {
				params["cursor"] = cursor
			}
			raw, e := mcpRequest(ctx, server, "tools/list", params)
			if e != nil {
				break
			}
			var data struct {
				Tools      []mcTool `json:"tools"`
				NextCursor string   `json:"nextCursor"`
			}
			if json.Unmarshal(raw, &data) != nil {
				break
			}
			for _, tool := range data.Tools {
				if !isToolIdentifier(tool.Name) {
					continue
				}
				name := "mcp__" + server.Name + "__" + tool.Name
				if len(name) > 160 {
					continue
				}
				if _, exists := out.targets[name]; exists {
					continue
				}
				schema := any(tool.InputSchema)
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				out.defs = append(out.defs, defineTool(name, tool.Description, schema))
				out.targets[name] = mcpTarget{profile: server, tool: tool.Name}
				for _, t := range server.SafeTools {
					if t == tool.Name {
						out.safe[name] = true
						break
					}
				}
			}
			if data.NextCursor == "" || data.NextCursor == cursor {
				break
			}
			cursor = data.NextCursor
		}
	}
	return out
}
func callMCP(ctx context.Context, t mcpTarget, args map[string]any) (json.RawMessage, error) {
	return mcpRequest(ctx, t.profile, "tools/call", map[string]any{"name": t.tool, "arguments": args})
}
