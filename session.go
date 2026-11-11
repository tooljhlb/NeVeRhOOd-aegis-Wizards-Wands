package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}
type Event struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	SessionID string    `json:"session_id"`
	Time      time.Time `json:"time"`
	Data      any       `json:"data"`
}
type Pending struct {
	Calls    []ToolCall `json:"calls"`
	Index    int        `json:"index"`
	Step     int        `json:"step"`
	Mode     string     `json:"mode"`
	Model    string     `json:"model"`
	Thinking string     `json:"thinking"`
}
type Session struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Mode     string    `json:"mode"`
	Model    string    `json:"model"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages []Message `json:"messages"`
	Events   []Event   `json:"events"`
	Pending  *Pending  `json:"pending,omitempty"`
	mu       sync.Mutex
}

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

type Store struct {
	dir      string
	mu       sync.Mutex
	sessions map[string]*Session
}

func newStore(dir string) (*Store, error) {
	if e := os.MkdirAll(filepath.Join(dir, "sessions"), 0700); e != nil {
		return nil, e
	}
	s := &Store{dir: dir, sessions: map[string]*Session{}}
	entries, e := os.ReadDir(filepath.Join(dir, "sessions"))
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "sessions", entry.Name()))
		if err != nil {
			return nil, err
		}
		var session Session
		if err = json.Unmarshal(b, &session); err != nil {
			return nil, fmt.Errorf("corrupt session %s: %w", entry.Name(), err)
		}
		if session.ID == "" || session.ID+".json" != entry.Name() {
			return nil, errors.New("invalid stored session identity")
		}
		s.sessions[session.ID] = &session
	}
	return s, nil
}
func (s *Store) create(model, mode string) *Session {
	now := time.Now().UTC()
	v := &Session{ID: newID(), Name: "New conversation", Mode: mode, Model: model, Created: now, Updated: now, Messages: []Message{}, Events: []Event{}}
	s.mu.Lock()
	s.sessions[v.ID] = v
	s.mu.Unlock()
	return v
}
func (s *Store) get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	return v, ok
}
func (s *Store) list() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}
func (s *Store) save(sess *Session) error {
	sess.Updated = time.Now().UTC()
	data, e := json.MarshalIndent(sess, "", "  ")
	if e != nil {
		return e
	}
	dir := filepath.Join(s.dir, "sessions")
	tmp, e := os.CreateTemp(dir, ".session-*")
	if e != nil {
		return e
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(data)
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
	return os.Rename(tmpName, filepath.Join(dir, sess.ID+".json"))
}
func (s *Store) emit(sess *Session, typ string, data any, send func(Event)) {
	ev := Event{ID: int64(len(sess.Events) + 1), Type: typ, SessionID: sess.ID, Time: time.Now().UTC(), Data: data}
	sess.Events = append(sess.Events, ev)
	if send != nil {
		send(ev)
	}
}
func (s *Store) replay(sess *Session, after int64) []Event {
	if after < 0 {
		after = 0
	}
	if after >= int64(len(sess.Events)) {
		return []Event{}
	}
	out := make([]Event, len(sess.Events[int(after):]))
	copy(out, sess.Events[int(after):])
	return out
}
