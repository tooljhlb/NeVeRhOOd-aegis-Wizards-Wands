package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type FlowStep struct {
	ID        string   `json:"id"`
	Model     string   `json:"model"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
}
type FlowSpec struct {
	Name  string     `json:"name"`
	Steps []FlowStep `json:"steps"`
}
type FlowState struct {
	ID      string            `json:"id"`
	Spec    FlowSpec          `json:"spec"`
	Status  string            `json:"status"`
	Outputs map[string]string `json:"outputs"`
	Steps   map[string]string `json:"steps"`
	Events  []Event           `json:"events"`
	Created time.Time         `json:"created"`
	Updated time.Time         `json:"updated"`
	Error   string            `json:"error,omitempty"`
}
type FlowStore struct {
	root  string
	mu    sync.Mutex
	items map[string]*FlowState
}

func newFlowStore(root string) (*FlowStore, error) {
	dir := filepath.Join(root, "workflows")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	st := &FlowStore{root: dir, items: map[string]*FlowState{}}
	files, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, f.Name()))
		if e != nil {
			return nil, e
		}
		var v FlowState
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		if v.ID+".json" != f.Name() {
			return nil, errors.New("workflow file identity invalid")
		}
		if v.Status == "running" {
			v.Status = "interrupted"
			v.Error = "Worker restarted; completed outputs retained, re-run requires a new explicit request"
		}
		st.items[v.ID] = &v
	}
	return st, nil
}
func (s *FlowStore) save(v *FlowState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.Updated = time.Now().UTC()
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(s.root, ".flow-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(b)
	}
	if e == nil {
		e = tmp.Sync()
	}
	if closeErr := tmp.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(tmp.Name(), filepath.Join(s.root, v.ID+".json")); e != nil {
		return e
	}
	s.items[v.ID] = v
	return nil
}
func (s *FlowStore) list() []*FlowState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*FlowState, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}
func (s *FlowStore) get(id string) (*FlowState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	return v, ok
}
func validateFlow(spec FlowSpec, c Config) error {
	if len(spec.Steps) < 1 || len(spec.Steps) > 12 {
		return errors.New("workflow must contain 1..12 steps")
	}
	seen := map[string]FlowStep{}
	for _, step := range spec.Steps {
		if !isToolIdentifier(step.ID) || seen[step.ID].ID != "" {
			return fmt.Errorf("invalid/duplicate step ID %q", step.ID)
		}
		if len(step.Task) == 0 || len(step.Task) > 8000 {
			return fmt.Errorf("task too long/empty for %s", step.ID)
		}
		if _, ok := c.model(step.Model); !ok {
			return fmt.Errorf("unknown model for step %s", step.ID)
		}
		seen[step.ID] = step
	}
	for _, step := range spec.Steps {
		for _, dep := range step.DependsOn {
			if _, ok := seen[dep]; !ok || dep == step.ID {
				return fmt.Errorf("invalid dependency %s for %s", dep, step.ID)
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return errors.New("workflow dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, d := range seen[id].DependsOn {
			if e := visit(d); e != nil {
				return e
			}
		}
		state[id] = 2
		return nil
	}
	for id := range seen {
		if e := visit(id); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) executeFlow(ctx context.Context, spec FlowSpec, send func(Event)) (*FlowState, error) {
	if e := validateFlow(spec, a.config); e != nil {
		return nil, e
	}
	now := time.Now().UTC()
	v := &FlowState{ID: newID(), Spec: spec, Status: "running", Created: now, Updated: now, Outputs: map[string]string{}, Steps: map[string]string{}, Events: []Event{}}
	for _, s := range spec.Steps {
		v.Steps[s.ID] = "planned"
	}
	emit := func(typ string, data any) {
		e := Event{ID: int64(len(v.Events) + 1), Type: typ, SessionID: v.ID, Time: time.Now().UTC(), Data: data}
		v.Events = append(v.Events, e)
		if send != nil {
			send(e)
		}
	}
	emit("workflow.started", map[string]any{"name": spec.Name, "steps": len(spec.Steps), "id": v.ID})
	if e := a.flows.save(v); e != nil {
		return nil, e
	}
	completed := map[string]bool{}
	for len(completed) < len(spec.Steps) {
		select {
		case <-ctx.Done():
			v.Status = "interrupted"
			v.Error = "Client disconnected"
			emit("workflow.interrupted", map[string]any{"error": v.Error})
			_ = a.flows.save(v)
			return v, ctx.Err()
		default:
		}
		progressed := false
		for _, step := range spec.Steps {
			if completed[step.ID] {
				continue
			}
			ready := true
			for _, d := range step.DependsOn {
				if !completed[d] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			progressed = true
			v.Steps[step.ID] = "running"
			emit("workflow.step.started", map[string]any{"step": step.ID, "model": step.Model})
			if e := a.flows.save(v); e != nil {
				return v, e
			}
			prompt := "You are one specialist in a bounded AI workflow. Complete exactly your task. Do not claim to have run tools; this workflow step has no tool access."
			if len(step.DependsOn) > 0 {
				prompt += "\nVerified prior step outputs:"
				for _, d := range step.DependsOn {
					prompt += "\n[" + d + "] " + v.Outputs[d]
				}
			}
			profile, _ := a.config.model(step.Model)
			result, e := modelGenerate(ctx, profile, []Message{{Role: "system", Content: prompt}, {Role: "user", Content: step.Task}}, nil, "auto", func(kind, delta string) {
				if kind == "content" {
					emit("workflow.step.delta", map[string]any{"step": step.ID, "text": delta})
				}
			})
			if e != nil || len(result.ToolCalls) > 0 {
				if e == nil {
					e = errors.New("workflow model requested unavailable tools")
				}
				v.Steps[step.ID] = "failed"
				v.Status = "failed"
				v.Error = e.Error()
				emit("workflow.step.failed", map[string]any{"step": step.ID, "error": e.Error()})
				_ = a.flows.save(v)
				return v, e
			}
			v.Steps[step.ID] = "completed"
			v.Outputs[step.ID] = result.Content
			completed[step.ID] = true
			emit("workflow.step.completed", map[string]any{"step": step.ID, "text": result.Content})
			if e := a.flows.save(v); e != nil {
				return v, e
			}
		}
		if !progressed {
			return v, errors.New("no executable step found")
		}
	}
	v.Status = "completed"
	emit("workflow.completed", map[string]any{"id": v.ID})
	return v, a.flows.save(v)
}
