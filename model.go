package main

import (
	"bufio"
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

type ToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

func defineTool(name, description string, schema any) ToolDefinition {
	var t ToolDefinition
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = schema
	return t
}

type ModelResult struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
	Finish    string
}
type deltaTool struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Function FunctionCall `json:"function"`
}
type modelChunk struct {
	Choices []struct {
		Delta struct {
			Content   string      `json:"content"`
			Reasoning string      `json:"reasoning_content"`
			ToolCalls []deltaTool `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		Finish string `json:"finish_reason"`
	} `json:"choices"`
}

var modelHTTPClient = &http.Client{Timeout: 4 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("model API redirects disabled") }}

func modelGenerate(ctx context.Context, profile ModelProfile, messages []Message, tools []ToolDefinition, thinking string, onDelta func(string, string)) (ModelResult, error) {
	if profile.Provider == "demo" {
		var s string
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				s = messages[i].Content
				break
			}
		}
		if strings.Contains(s, "[tool:clock.now]") && len(messages) > 0 && messages[len(messages)-1].Role != "tool" {
			return ModelResult{ToolCalls: []ToolCall{{ID: "demo-call", Type: "function", Function: FunctionCall{Name: "clock_now", Arguments: "{}"}}}, Finish: "tool_calls"}, nil
		}
		text := "Demo mode is running. Connect an OpenAI-compatible model in config.json to enable live AI. Your message: " + s
		if messages[len(messages)-1].Role == "tool" {
			text = "Tool result received: " + messages[len(messages)-1].Content
		}
		for _, w := range strings.Fields(text) {
			select {
			case <-ctx.Done():
				return ModelResult{}, ctx.Err()
			default:
			}
			onDelta("content", w+" ")
		}
		return ModelResult{Content: text, Finish: "stop"}, nil
	}
	endpoint := strings.TrimRight(profile.BaseURL, "/") + "/chat/completions"
	reqBody := map[string]any{"model": profile.Model, "stream": true, "messages": messages}
	if len(tools) > 0 {
		reqBody["tools"] = tools
		reqBody["tool_choice"] = "auto"
	}
	if thinking == "low" || thinking == "medium" || thinking == "high" {
		reqBody["reasoning_effort"] = thinking
	} // Provider support varies; use auto when not configured.
	raw, _ := json.Marshal(reqBody)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if e != nil {
		return ModelResult{}, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if profile.APIKeyEnv != "" {
		key := os.Getenv(profile.APIKeyEnv)
		if key == "" {
			return ModelResult{}, fmt.Errorf("model credential environment %s is unset", profile.APIKeyEnv)
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, e := modelHTTPClient.Do(req)
	if e != nil {
		return ModelResult{}, e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return ModelResult{}, fmt.Errorf("model HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	out := ModelResult{}
	calls := map[int]*ToolCall{}
	consume := func(data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var chunk modelChunk
		if e = json.Unmarshal(data, &chunk); e != nil {
			return fmt.Errorf("malformed model stream: %w", e)
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		c := chunk.Choices[0]
		if c.Delta.Content != "" {
			out.Content += c.Delta.Content
			onDelta("content", c.Delta.Content)
		}
		if c.Delta.Reasoning != "" {
			out.Reasoning += c.Delta.Reasoning
			onDelta("reasoning", c.Delta.Reasoning)
		}
		for _, dt := range c.Delta.ToolCalls {
			tc := calls[dt.Index]
			if tc == nil {
				tc = &ToolCall{Type: "function"}
				calls[dt.Index] = tc
			}
			if dt.ID != "" {
				tc.ID = dt.ID
			}
			if dt.Function.Name != "" {
				tc.Function.Name += dt.Function.Name
			}
			tc.Function.Arguments += dt.Function.Arguments
		}
		if c.Finish != "" {
			out.Finish = c.Finish
		}
		if c.Message.Content != "" {
			out.Content += c.Message.Content
			onDelta("content", c.Message.Content)
		}
		if len(c.Message.ToolCalls) > 0 {
			out.ToolCalls = append(out.ToolCalls, c.Message.ToolCalls...)
		}
		return nil
	}
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		e = readSSE(res.Body, consume)
	} else {
		raw, e2 := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		if e2 != nil {
			return out, e2
		}
		e = consume(raw)
	}
	if e != nil {
		return out, e
	}
	for i := 0; i < len(calls); i++ {
		if call := calls[i]; call != nil {
			out.ToolCalls = append(out.ToolCalls, *call)
		}
	}
	for i := range out.ToolCalls {
		if out.ToolCalls[i].ID == "" {
			out.ToolCalls[i].ID = newID()
		}
	}
	return out, nil
}

// Reads data-only or named SSE events, supports CRLF and multiple data lines.
func readSSE(r io.Reader, handle func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var lines []string
	flush := func() error {
		if len(lines) == 0 {
			return nil
		}
		data := strings.Join(lines, "\n")
		lines = nil
		if data == "[DONE]" {
			return io.EOF
		}
		return handle([]byte(data))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if e := flush(); e != nil {
				if errors.Is(e, io.EOF) {
					return nil
				}
				return e
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if e := scanner.Err(); e != nil {
		return e
	}
	if e := flush(); errors.Is(e, io.EOF) {
		return nil
	} else {
		return e
	}
}
