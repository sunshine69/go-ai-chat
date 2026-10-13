package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// WorkdirContext holds the working directory information for a client session.
type WorkdirContext struct {
	Dir string // the current working directory for this session (BaseDir/Workdir or BaseDir)
}

// Session represents an active MCP server session for a specific workdir.
type Session struct {
	Workdir   string
	Fullpath  string
	MCPServer *server.MCPServer
	Active    bool
	Cleanup   func() // optional cleanup function
}

// WorkdirManager manages multiple workdir sessions with concurrency control.
type WorkdirManager struct {
	baseDir  string
	sessions map[string]*Session
	mutex    sync.RWMutex
}

// NewWorkdirManager creates a new WorkdirManager with the given base directory.
func NewWorkdirManager(baseDir string) *WorkdirManager {
	return &WorkdirManager{
		baseDir:  baseDir,
		sessions: make(map[string]*Session),
	}
}

// GetOrCreateSession returns an existing session for the workdir, or creates a new one.
// If the workdir is already occupied by another session, it returns an error.
func (wm *WorkdirManager) GetOrCreateSession(workdir string) (*Session, error) {
	// First, check if session exists (read lock)
	wm.mutex.RLock()
	if session, ok := wm.sessions[workdir]; ok && session.Active {
		wm.mutex.RUnlock()
		return session, nil
	}
	wm.mutex.RUnlock()

	// Need to create new session (write lock)
	wm.mutex.Lock()
	defer wm.mutex.Unlock()

	// Double-check after acquiring write lock
	if session, ok := wm.sessions[workdir]; ok && session.Active {
		return session, fmt.Errorf("workdir '%s' is already occupied by another client", workdir)
	}

	// Create the full path
	var fullpath string
	if workdir == "" {
		fullpath = wm.baseDir
	} else {
		fullpath = filepath.Join(wm.baseDir, workdir)
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(fullpath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workdir '%s': %w", workdir, err)
	}

	// Create a session - we don't create a new MCPServer here
	// Instead, we use the base server and pass workdir context via request context
	session := &Session{
		Workdir:  workdir,
		Fullpath: fullpath,
		Active:   true,
	}

	wm.sessions[workdir] = session
	return session, nil
}

// ReleaseSession marks a session as inactive, allowing other clients to use the workdir.
func (wm *WorkdirManager) ReleaseSession(workdir string) {
	wm.mutex.Lock()
	defer wm.mutex.Unlock()
	if session, ok := wm.sessions[workdir]; ok {
		session.Active = false
		// Optionally, we could remove the session from the map after a timeout
		// or when cleanup is needed
	}
}

// GetSession returns an existing active session, or error if not found.
func (wm *WorkdirManager) GetSession(workdir string) (*Session, error) {
	wm.mutex.RLock()
	defer wm.mutex.RUnlock()
	session, ok := wm.sessions[workdir]
	if !ok || !session.Active {
		return nil, fmt.Errorf("workdir '%s' is not active", workdir)
	}
	return session, nil
}

// ListActiveWorkdirs returns all currently active workdirs.
func (wm *WorkdirManager) ListActiveWorkdirs() []string {
	wm.mutex.RLock()
	defer wm.mutex.RUnlock()
	active := make([]string, 0, len(wm.sessions))
	for w, session := range wm.sessions {
		if session.Active {
			active = append(active, w)
		}
	}
	return active
}