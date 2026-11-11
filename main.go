package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

type App struct {
	flows     *FlowStore
	config    Config
	store     *Store
	artifacts *Artifacts
	token     string
}

func newApp(config Config, token string) (*App, error) {
	s, e := newStore(config.DataDir)
	if e != nil {
		return nil, e
	}
	a, e := newArtifacts(config.DataDir)
	if e != nil {
		return nil, e
	}
	flows, e := newFlowStore(config.DataDir)
	if e != nil {
		return nil, e
	}
	return &App{config: config, store: s, artifacts: a, flows: flows, token: token}, nil
}
func (a *App) authorized(r *http.Request) bool {
	input := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if input == r.Header.Get("Authorization") {
		input = r.Header.Get("X-WW-Token")
	}
	return subtle.ConstantTimeCompare([]byte(input), []byte(a.token)) == 1
}
func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}
func decodeJSON(r *http.Request, dest any) error {
	defer r.Body.Close()
	d := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(dest); e != nil {
		return e
	}
	var rest any
	if e := d.Decode(&rest); !errors.Is(e, io.EOF) {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}
func sseInit(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
}
func sendEvent(w http.ResponseWriter, e Event) {
	b, _ := json.Marshal(e)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]any{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
		if !a.authorized(r) {
			writeJSON(w, 401, map[string]any{"error": "bearer token required"})
			return
		}
		a.api(w, r)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	filename := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if filename == "." {
		filename = "index.html"
	}
	if strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}
	sub, _ := fs.Sub(webFS, "web")
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}
func (a *App) api(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/status":
		if r.Method != "GET" {
			break
		}
		writeJSON(w, 200, map[string]any{"name": "Wizards & Wands", "version": "0.1.0", "default_model": a.config.DefaultModel, "models": len(a.config.Models), "mcp_servers": len(a.config.MCPServers), "data_adapter_configured": a.config.DataFrameworks.BaseURL != "", "max_steps": a.config.MaxSteps})
		return
	case "/api/models", "/v1/models":
		if r.Method != "GET" {
			break
		}
		models := []map[string]any{}
		for _, m := range a.config.Models {
			models = append(models, map[string]any{"id": m.Name, "model": m.Model, "provider": m.Provider})
		}
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"object": "list", "data": models})
		} else {
			writeJSON(w, 200, models)
		}
		return
	case "/api/connectors":
		if r.Method != "GET" {
			break
		}
		servers := []map[string]any{}
		for _, m := range a.config.MCPServers {
			servers = append(servers, map[string]any{"name": m.Name, "configured": true, "version": mcpVersion(m)})
		}
		writeJSON(w, 200, map[string]any{"mcp": servers, "data_adapter_configured": a.config.DataFrameworks.BaseURL != ""})
		return
	case "/api/tools":
		if r.Method != "GET" {
			break
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		defs, _ := a.tools(ctx)
		writeJSON(w, 200, defs)
		return
	case "/api/sessions":
		switch r.Method {
		case "GET":
			items := []map[string]any{}
			for _, s := range a.store.list() {
				s.mu.Lock()
				items = append(items, map[string]any{"id": s.ID, "name": s.Name, "mode": s.Mode, "model": s.Model, "updated": s.Updated, "pending": s.Pending != nil})
				s.mu.Unlock()
			}
			writeJSON(w, 200, items)
			return
		case "POST":
			var req struct {
				Model string `json:"model"`
				Mode  string `json:"mode"`
			}
			if e := decodeJSON(r, &req); e != nil {
				writeJSON(w, 400, map[string]any{"error": e.Error()})
				return
			}
			if req.Model == "" {
				req.Model = a.config.DefaultModel
			}
			if _, ok := a.config.model(req.Model); !ok {
				writeJSON(w, 400, map[string]any{"error": "unknown model"})
				return
			}
			if req.Mode == "" {
				req.Mode = "chat"
			}
			if !validMode(req.Mode) {
				writeJSON(w, 400, map[string]any{"error": "invalid mode"})
				return
			}
			s := a.store.create(req.Model, req.Mode)
			if e := a.store.save(s); e != nil {
				writeJSON(w, 500, map[string]any{"error": e.Error()})
				return
			}
			writeJSON(w, 201, s)
			return
		}
	case "/api/workflows":
		if r.Method != "GET" {
			break
		}
		writeJSON(w, 200, a.flows.list())
		return
	case "/api/workflows/execute":
		if r.Method != "POST" {
			break
		}
		var spec FlowSpec
		if e := decodeJSON(r, &spec); e != nil {
			writeJSON(w, 400, map[string]any{"error": e.Error()})
			return
		}
		if e := validateFlow(spec, a.config); e != nil {
			writeJSON(w, 400, map[string]any{"error": e.Error()})
			return
		}
		sseInit(w)
		_, err := a.executeFlow(r.Context(), spec, func(e Event) { sendEvent(w, e) })
		if err != nil {
			sendEvent(w, Event{Type: "workflow.error", Data: map[string]any{"error": err.Error()}})
		}
		return
	case "/api/artifacts":
		if r.Method != "POST" {
			break
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20+4096)
		file, header, e := r.FormFile("file")
		if e != nil {
			writeJSON(w, 400, map[string]any{"error": e.Error()})
			return
		}
		defer file.Close()
		x, e := a.artifacts.put(file, header)
		if e != nil {
			writeJSON(w, 400, map[string]any{"error": e.Error()})
			return
		}
		writeJSON(w, 201, x)
		return
	case "/v1/chat/completions":
		if r.Method != "POST" {
			break
		}
		a.openAIProxy(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/artifacts/") {
		id := strings.TrimPrefix(r.URL.Path, "/api/artifacts/")
		if r.Method != "GET" {
			writeJSON(w, 405, map[string]any{"error": "method not allowed"})
			return
		}
		v, ok := a.artifacts.get(id)
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return
		}
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment")
			http.ServeFile(w, r, a.artifacts.root+"/"+v.ID+".bin")
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/workflows/") {
		if r.Method != "GET" {
			writeJSON(w, 405, map[string]any{"error": "method not allowed"})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
		if v, ok := a.flows.get(id); ok {
			writeJSON(w, 200, v)
		} else {
			writeJSON(w, 404, map[string]any{"error": "not found"})
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/sessions/") {
		a.sessionAPI(w, r)
		return
	}
	writeJSON(w, 404, map[string]any{"error": "not found or method not allowed"})
}
func (a *App) sessionAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	id := parts[0]
	s, ok := a.store.get(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "session not found"})
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 2 {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	switch action {
	case "":
		if r.Method != "GET" {
			break
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, s)
		return
	case "events":
		if r.Method != "GET" {
			break
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		s.mu.Lock()
		ev := a.store.replay(s, after)
		s.mu.Unlock()
		writeJSON(w, 200, ev)
		return
	case "chat", "approval":
		if r.Method != "POST" {
			break
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		sseInit(w)
		if action == "chat" {
			var req ChatInput
			if e := decodeJSON(r, &req); e != nil {
				sendEvent(w, Event{Type: "turn.error", Data: map[string]any{"error": e.Error()}})
				return
			}
			if e := a.runTurn(r.Context(), s, &req, nil, func(e Event) { sendEvent(w, e) }); e != nil {
				sendEvent(w, Event{Type: "turn.error", Data: map[string]any{"error": e.Error()}})
			}
		} else {
			var req struct {
				Approve bool `json:"approve"`
			}
			if e := decodeJSON(r, &req); e != nil {
				sendEvent(w, Event{Type: "turn.error", Data: map[string]any{"error": e.Error()}})
				return
			}
			if e := a.runTurn(r.Context(), s, nil, &req.Approve, func(e Event) { sendEvent(w, e) }); e != nil {
				sendEvent(w, Event{Type: "turn.error", Data: map[string]any{"error": e.Error()}})
			}
		}
		return
	}
	writeJSON(w, 405, map[string]any{"error": "method not allowed"})
}
func (a *App) openAIProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model           string           `json:"model"`
		Messages        []Message        `json:"messages"`
		Stream          bool             `json:"stream"`
		Tools           []ToolDefinition `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
	}
	if e := decodeJSON(r, &req); e != nil {
		writeJSON(w, 400, map[string]any{"error": e.Error()})
		return
	}
	if req.Model == "" {
		req.Model = a.config.DefaultModel
	}
	profile, ok := a.config.model(req.Model)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "model profile not found"})
		return
	}
	id := "chatcmpl-" + newID()
	created := time.Now().Unix()
	if req.Stream {
		sseInit(w)
	}
	emit := func(kind, value string) {
		if !req.Stream {
			return
		}
		delta := map[string]any{}
		if kind == "content" {
			delta["content"] = value
		} else {
			delta["reasoning_content"] = value
		}
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	result, e := modelGenerate(r.Context(), profile, req.Messages, req.Tools, req.ReasoningEffort, emit)
	if e != nil {
		if req.Stream {
			b, _ := json.Marshal(map[string]any{"error": e.Error()})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
		} else {
			writeJSON(w, 502, map[string]any{"error": e.Error()})
		}
		return
	}
	finish := "stop"
	if len(result.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if req.Stream {
		delta := map[string]any{}
		if len(result.ToolCalls) > 0 {
			delta["tool_calls"] = result.ToolCalls
		}
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "message": Message{Role: "assistant", Content: result.Content, ToolCalls: result.ToolCalls}, "finish_reason": finish}}})
}
func main() {
	configPath := flag.String("config", "", "Path to JSON config; no file defaults to offline demo")
	flag.Parse()
	cfg, e := loadConfig(*configPath)
	if e != nil {
		log.Fatal(e)
	}
	token := os.Getenv("WW_TOKEN")
	if token == "" {
		var buf [24]byte
		if _, e = rand.Read(buf[:]); e != nil {
			log.Fatal(e)
		}
		token = hex.EncodeToString(buf[:])
		fmt.Fprintln(os.Stderr, "Temporary local API token:", token)
	}
	app, e := newApp(cfg, token)
	if e != nil {
		log.Fatal(e)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: app, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("Wizards & Wands listening at http://%s (loopback recommended)", cfg.Listen)
	log.Fatal(srv.ListenAndServe())
}
