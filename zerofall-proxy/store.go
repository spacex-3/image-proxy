package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type imageOutput struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
}
type taskError struct {
	Code       string `json:"code"`
	Type       string `json:"type"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}
type task struct {
	ID                string           `json:"id"`
	Model             string           `json:"model"`
	Action            string           `json:"action"`
	Status            string           `json:"status"`
	Stage             string           `json:"stage"`
	CreatedAt         int64            `json:"created_at"`
	UpdatedAt         int64            `json:"updated_at"`
	Size              string           `json:"size"`
	Quality           string           `json:"quality"`
	ReferenceCount    int              `json:"reference_count"`
	ReferenceBytes    int64            `json:"reference_bytes"`
	UpstreamRequestID string           `json:"upstream_request_id,omitempty"`
	Timings           map[string]int64 `json:"timings_ms"`
	Outputs           []imageOutput    `json:"data,omitempty"`
	OutputKeys        []string         `json:"output_keys,omitempty"`
	Error             *taskError       `json:"error,omitempty"`
}

type taskStore struct {
	mu    sync.Mutex
	root  string
	tasks map[string]task
}

var errQueueFull = errors.New("task queue is full")

func openTaskStore(root string) (*taskStore, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	s := &taskStore{root: root, tasks: map[string]task{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !taskIDPattern.MatchString(entry.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "task.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var t task
		if err = json.Unmarshal(data, &t); err != nil {
			return nil, err
		}
		if t.ID != entry.Name() {
			return nil, errors.New("invalid persisted task ID")
		}
		// A synchronous POST cannot be resumed safely after a restart. Never
		// replay it: the upstream may already have generated and charged.
		if t.Status == "in_progress" {
			t.Status = "failed"
			t.Error = &taskError{Code: "upstream_interrupted", Type: "upstream_error", Message: "Relay restarted during processing; upstream completion is unknown"}
			t.UpdatedAt = time.Now().Unix()
			if err = writeAtomic(filepath.Join(root, t.ID, "task.json"), t); err != nil {
				return nil, err
			}
			_ = os.Remove(filepath.Join(root, t.ID, "request.json"))
		}
		s.tasks[t.ID] = t
	}
	return s, nil
}

func writeAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *taskStore) create(t task, body imageRequest, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := 0
	for _, t := range s.tasks {
		if t.Status == "queued" || t.Status == "in_progress" {
			active++
		}
	}
	if active >= limit {
		return errQueueFull
	}
	dir := filepath.Join(s.root, t.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "request.json"), body); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "task.json"), t); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	s.tasks[t.ID] = t
	return nil
}

func (s *taskStore) save(t task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(filepath.Join(s.root, t.ID, "task.json"), t); err != nil {
		return err
	}
	s.tasks[t.ID] = t
	if t.Status == "completed" || t.Status == "failed" {
		_ = os.Remove(filepath.Join(s.root, t.ID, "request.json"))
	}
	return nil
}

func (s *taskStore) get(id string) (task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	// Workers mutate their own timing map, never the published snapshot.
	t.Timings = cloneTimings(t.Timings)
	return t, ok
}
func cloneTimings(src map[string]int64) map[string]int64 {
	dst := map[string]int64{}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
func (s *taskStore) list() []task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]task, 0, len(s.tasks))
	for _, t := range s.tasks {
		t.Timings = cloneTimings(t.Timings)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}
