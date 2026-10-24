package client

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"sync"
)

// decisionScope controls how long a human's answer to a prompt is
// remembered.
type decisionScope int

const (
	// decisionOnce isn't remembered at all; the human is asked again next
	// time, even for the identical URL.
	decisionOnce decisionScope = iota
	// decisionURL is remembered for exactly this URL (scheme, host, path,
	// and query all included) -- a different path or query on the same
	// host is asked about separately.
	decisionURL
	// decisionDomain is remembered for every URL on this host, regardless
	// of path or query.
	decisionDomain
)

// decisionMemory records "remember this" answers so a repeated request for
// the same URL or domain can be resolved without prompting again. The zero
// value is ready to use, with persistence disabled until LoadFromFile is
// called.
type decisionMemory struct {
	mu      sync.RWMutex
	urls    map[string]bool
	domains map[string]bool
	// path is where Remember persists every update, and where LoadFromFile
	// reads from on startup. Empty disables persistence entirely.
	path string
}

// memorySnapshot is decisionMemory's on-disk JSON representation.
type memorySnapshot struct {
	URLs    map[string]bool `json:"urls"`
	Domains map[string]bool `json:"domains"`
}

// Lookup reports whether a remembered decision covers u, and if so,
// whether it was allowed. A denial always wins over an allowance: if
// either the exact URL or its domain was remembered as denied, the result
// is deny even when the other scope was separately remembered as allowed
// -- a denial at either scope is a standing exception that an allowance at
// the other scope, recorded before or after it, can never override.
func (m *decisionMemory) Lookup(u url.URL) (allowed, known bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	urlAllowed, urlKnown := m.urls[u.String()]
	domainAllowed, domainKnown := m.domains[u.Hostname()]

	if !urlKnown && !domainKnown {
		return false, false
	}
	if (urlKnown && !urlAllowed) || (domainKnown && !domainAllowed) {
		return false, true
	}
	return true, true
}

// Remember records allowed for u at the given scope, and persists the
// update to disk if LoadFromFile has set a path. decisionOnce is a no-op,
// since by definition it's never looked up again.
func (m *decisionMemory) Remember(u url.URL, scope decisionScope, allowed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch scope {
	case decisionURL:
		if m.urls == nil {
			m.urls = make(map[string]bool)
		}
		m.urls[u.String()] = allowed
	case decisionDomain:
		if m.domains == nil {
			m.domains = make(map[string]bool)
		}
		m.domains[u.Hostname()] = allowed
	default:
		return
	}
	m.save()
}

// save writes the current contents of m to m.path as JSON. Must be called
// with mu held. A failure is only logged, not returned or panicked on --
// persistence is a convenience, and losing it shouldn't take down a
// client that's mid-session with a human waiting on a prompt.
func (m *decisionMemory) save() {
	if m.path == "" {
		return
	}
	data, err := json.MarshalIndent(memorySnapshot{URLs: m.urls, Domains: m.domains}, "", "  ")
	if err != nil {
		slog.Error("Failed to marshal remembered decisions", "error", err)
		return
	}
	if err := os.WriteFile(m.path, data, 0o600); err != nil {
		slog.Error("Failed to save remembered decisions", "path", m.path, "error", err)
	}
}

// LoadFromFile points m at path for all future saves, and immediately
// loads whatever's already there, if anything. A missing file just means
// this is the first run. A file that exists but can't be parsed as JSON
// is treated as corrupt: it's deleted (so it doesn't keep failing to load
// forever) and a warning is logged, and m starts out empty rather than
// crashing the client over a file that only exists as a convenience.
func (m *decisionMemory) LoadFromFile(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.path = path

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Warn("Failed to read remembered decisions, starting empty", "path", path, "error", err)
		return
	}

	var snap memorySnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Warn("Remembered-decisions file is malformed, deleting and starting empty", "path", path, "error", err)
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("Failed to delete malformed remembered-decisions file", "path", path, "error", rmErr)
		}
		return
	}
	m.urls = snap.URLs
	m.domains = snap.Domains
}
