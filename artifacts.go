package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Artifact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Bytes     int64     `json:"bytes"`
	Created   time.Time `json:"created"`
}
type Artifacts struct {
	root string
	mu   sync.Mutex
}

func newArtifacts(root string) (*Artifacts, error) {
	r := filepath.Join(root, "artifacts")
	return &Artifacts{root: r}, os.MkdirAll(r, 0700)
}
func (a *Artifacts) get(id string) (Artifact, bool) {
	var x Artifact
	if len(id) != 64 {
		return x, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, e := os.ReadFile(filepath.Join(a.root, id+".json"))
	if e != nil || json.Unmarshal(b, &x) != nil {
		return x, false
	}
	return x, x.ID == id
}
func (a *Artifacts) put(file multipart.File, header *multipart.FileHeader) (Artifact, error) {
	var v Artifact
	if header.Size > 16<<20 {
		return v, errors.New("max upload is 16 MiB")
	}
	b, e := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if e != nil {
		return v, e
	}
	if len(b) > 16<<20 {
		return v, errors.New("file too large")
	}
	name := filepath.Base(header.Filename)
	if name == "" || name == "." {
		name = "attachment"
	}
	media := http.DetectContentType(b)
	if len(b) == 0 {
		media = "application/octet-stream"
	}
	v = Artifact{ID: artifactID(b), Name: name, MediaType: media, Bytes: int64(len(b)), Created: time.Now().UTC()}
	a.mu.Lock()
	defer a.mu.Unlock()
	path := filepath.Join(a.root, v.ID+".bin")
	if e = os.WriteFile(path, b, 0600); e != nil {
		return Artifact{}, e
	}
	meta, _ := json.MarshalIndent(v, "", "  ")
	if e = os.WriteFile(filepath.Join(a.root, v.ID+".json"), meta, 0600); e != nil {
		return Artifact{}, fmt.Errorf("write artifact metadata: %w", e)
	}
	return v, nil
}
