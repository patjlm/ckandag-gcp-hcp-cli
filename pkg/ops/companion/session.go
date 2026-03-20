package companion

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/cloudrun"
)

// SessionEvent represents a single logged event in a session.
type SessionEvent struct {
	Time       time.Time              `json:"time"`
	Type       string                 `json:"type"` // "user", "assistant", "tool_call", "tool_confirm", "tool_result", "error"
	Content    string                 `json:"content,omitempty"`
	Tool       string                 `json:"tool,omitempty"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
	Result     json.RawMessage        `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

// SessionLogger writes session events to an NDJSON file.
type SessionLogger struct {
	file *os.File
	enc  *json.Encoder
}

const sessionMaxAge = 14 * 24 * time.Hour

func sessionsBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".gcphcp", "sre-companion", "sessions")
}

func projectSessionDir(project string) string {
	return filepath.Join(sessionsBaseDir(), project)
}

// NewSessionLogger creates a session log file under ~/.gcphcp/sre-companion/sessions/<project>/
// and removes stale session files across all projects.
func NewSessionLogger(project string) (*SessionLogger, error) {
	cleanupStaleSessions(sessionsBaseDir())

	dir := projectSessionDir(project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}

	filename := fmt.Sprintf("%s.jsonl", time.Now().Format("2006-01-02T15-04-05"))
	path := filepath.Join(dir, filename)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating session file: %w", err)
	}

	return &SessionLogger{file: f, enc: json.NewEncoder(f)}, nil
}

// cleanupStaleSessions removes session files not modified in the last 2 weeks
// across all project subdirectories, and removes empty project dirs.
func cleanupStaleSessions(baseDir string) {
	projectDirs, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sessionMaxAge)
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, pd.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		// Remove empty project dirs
		remaining, _ := os.ReadDir(dir)
		if len(remaining) == 0 {
			os.Remove(dir)
		}
	}
}

// Path returns the session log file path.
func (s *SessionLogger) Path() string {
	if s == nil || s.file == nil {
		return ""
	}
	return s.file.Name()
}

// Log writes a session event.
func (s *SessionLogger) Log(event SessionEvent) {
	if s == nil {
		return
	}
	event.Time = time.Now()
	s.enc.Encode(event) //nolint:errcheck
}

// Close closes the session log file.
func (s *SessionLogger) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

// SessionSummary describes a past session for the /resume picker.
type SessionSummary struct {
	Path      string
	LastInput string
	UpdatedAt time.Time
}

// ListSessions returns past sessions for a project, sorted by last update (newest first).
func ListSessions(project string) ([]SessionSummary, error) {
	dir := projectSessionDir(project)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []SessionSummary
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		lastInput := lastUserInput(path)
		if lastInput == "" {
			continue // empty session
		}
		sessions = append(sessions, SessionSummary{
			Path:      path,
			LastInput: lastInput,
			UpdatedAt: info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

// lastUserInput scans a session file and returns the last user message content.
func lastUserInput(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var last string
	dec := json.NewDecoder(f)
	for dec.More() {
		var event SessionEvent
		if err := dec.Decode(&event); err != nil {
			continue
		}
		if event.Type == "user" {
			last = event.Content
		}
	}
	return last
}

// LoadHistory reads a session log and reconstructs the conversation history.
func LoadHistory(path string) ([]cloudrun.ChatMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var history []cloudrun.ChatMessage
	dec := json.NewDecoder(f)
	for dec.More() {
		var event SessionEvent
		if err := dec.Decode(&event); err != nil {
			continue
		}
		switch event.Type {
		case "user":
			history = append(history, cloudrun.ChatMessage{Role: "user", Content: event.Content})
		case "assistant":
			history = append(history, cloudrun.ChatMessage{Role: "assistant", Content: event.Content})
		}
	}
	return history, nil
}
