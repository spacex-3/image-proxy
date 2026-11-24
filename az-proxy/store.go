package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type taskStatus string

const (
	statusSubmitting taskStatus = "submitting"
	statusProcessing taskStatus = "processing"
	statusCompleted  taskStatus = "completed"
	statusFailed     taskStatus = "failed"
)

type imageOutput struct {
	URL string `json:"url"`
}

type proxyTask struct {
	ID              string        `json:"id"`
	UpstreamID      string        `json:"upstream_id,omitempty"`
	Action          string        `json:"action"`
	Model           string        `json:"model"`
	Status          taskStatus    `json:"status"`
	ErrorCode       string        `json:"error_code,omitempty"`
	ErrorType       string        `json:"error_type,omitempty"`
	ErrorMessage    string        `json:"error_message,omitempty"`
	CreatedAt       int64         `json:"created_at"`
	UpdatedAt       int64         `json:"updated_at"`
	NextPollAt      int64         `json:"next_poll_at,omitempty"`
	InputKeys       []string      `json:"input_keys,omitempty"`
	OutputKeys      []string      `json:"output_keys,omitempty"`
	Outputs         []imageOutput `json:"outputs,omitempty"`
	FailureNotified bool          `json:"failure_notified,omitempty"`
}

type taskStore struct {
	mu    sync.RWMutex
	path  string
	tasks map[string]proxyTask
}

func openTaskStore(path string) (*taskStore, error) {
	store := &taskStore{path: path, tasks: make(map[string]proxyTask)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &store.tasks); err != nil {
		return nil, fmt.Errorf("decode task store: %w", err)
	}
	return store, nil
}

func (s *taskStore) put(task proxyTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = cloneTask(task)
	return s.saveLocked()
}

func (s *taskStore) get(id string) (proxyTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	return cloneTask(task), ok
}

func (s *taskStore) update(id string, apply func(*proxyTask)) (proxyTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return proxyTask{}, os.ErrNotExist
	}
	apply(&task)
	task.UpdatedAt = time.Now().Unix()
	s.tasks[id] = cloneTask(task)
	return cloneTask(task), s.saveLocked()
}

func (s *taskStore) due(now time.Time) []proxyTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]proxyTask, 0)
	for _, task := range s.tasks {
		if task.Status == statusProcessing && task.UpstreamID != "" && task.NextPollAt <= now.Unix() {
			result = append(result, cloneTask(task))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NextPollAt < result[j].NextPollAt })
	return result
}

func (s *taskStore) submitting() []proxyTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]proxyTask, 0)
	for _, task := range s.tasks {
		if task.Status == statusSubmitting {
			result = append(result, cloneTask(task))
		}
	}
	return result
}

func (s *taskStore) expired(cutoff int64) []proxyTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]proxyTask, 0)
	for _, task := range s.tasks {
		if task.UpdatedAt < cutoff && (task.Status == statusCompleted || task.Status == statusFailed) {
			result = append(result, cloneTask(task))
		}
	}
	return result
}

func (s *taskStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return s.saveLocked()
}

func (s *taskStore) saveLocked() error {
	data, err := json.Marshal(s.tasks)
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err = os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

func cloneTask(task proxyTask) proxyTask {
	task.InputKeys = append([]string(nil), task.InputKeys...)
	task.OutputKeys = append([]string(nil), task.OutputKeys...)
	task.Outputs = append([]imageOutput(nil), task.Outputs...)
	return task
}
