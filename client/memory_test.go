package client

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func mustParseTestURL(t *testing.T, raw string) url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return *u
}

func TestDecisionMemoryLookupUnknown(t *testing.T) {
	var m decisionMemory
	if _, known := m.Lookup(mustParseTestURL(t, "https://example.com/a")); known {
		t.Error("Lookup() known = true on an empty memory, want false")
	}
}

func TestDecisionMemoryURLScope(t *testing.T) {
	var m decisionMemory
	u := mustParseTestURL(t, "https://example.com/a")
	m.Remember(u, decisionURL, true)

	if allowed, known := m.Lookup(u); !known || !allowed {
		t.Errorf("Lookup(%v) = (%v, %v), want (true, true)", u, allowed, known)
	}

	other := mustParseTestURL(t, "https://example.com/b")
	if _, known := m.Lookup(other); known {
		t.Errorf("Lookup(%v) known = true, want false -- a different path must not match a URL-scoped rule", other)
	}
}

func TestDecisionMemoryDomainScope(t *testing.T) {
	var m decisionMemory
	m.Remember(mustParseTestURL(t, "https://example.com/a"), decisionDomain, true)

	for _, raw := range []string{"https://example.com/a", "https://example.com/b", "https://example.com/"} {
		u := mustParseTestURL(t, raw)
		if allowed, known := m.Lookup(u); !known || !allowed {
			t.Errorf("Lookup(%v) = (%v, %v), want (true, true)", u, allowed, known)
		}
	}

	other := mustParseTestURL(t, "https://other.example/a")
	if _, known := m.Lookup(other); known {
		t.Errorf("Lookup(%v) known = true, want false -- a different host must not match", other)
	}
}

func TestDecisionMemoryDenyBeatsDomainAllow(t *testing.T) {
	var m decisionMemory
	allowed := mustParseTestURL(t, "https://example.com/allowed")
	denied := mustParseTestURL(t, "https://example.com/denied")

	m.Remember(allowed, decisionDomain, true)
	m.Remember(denied, decisionURL, false)

	if got, known := m.Lookup(denied); !known || got {
		t.Errorf("Lookup(%v) = (%v, %v), want (false, true) -- a URL-level deny must beat a domain-level allow", denied, got, known)
	}
	if got, known := m.Lookup(allowed); !known || !got {
		t.Errorf("Lookup(%v) = (%v, %v), want (true, true) -- unrelated URLs on the domain stay allowed", allowed, got, known)
	}
}

func TestDecisionMemoryDenyBeatsMoreSpecificURLAllow(t *testing.T) {
	var m decisionMemory
	u := mustParseTestURL(t, "https://example.com/sensitive")

	// The domain is denied as a whole, but this one URL was separately
	// (and more specifically) remembered as allowed. Denial still wins,
	// regardless of which rule is more specific.
	m.Remember(u, decisionDomain, false)
	m.Remember(u, decisionURL, true)

	if got, known := m.Lookup(u); !known || got {
		t.Errorf("Lookup(%v) = (%v, %v), want (false, true) -- domain-level deny must beat a more specific URL-level allow", u, got, known)
	}
}

func TestDecisionMemoryOnceIsNotRemembered(t *testing.T) {
	var m decisionMemory
	u := mustParseTestURL(t, "https://example.com/a")

	m.Remember(u, decisionOnce, true)

	if _, known := m.Lookup(u); known {
		t.Error("Lookup() known = true after remembering decisionOnce, want false")
	}
}

func TestDecisionMemoryPersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")

	var m decisionMemory
	m.LoadFromFile(path)
	m.Remember(mustParseTestURL(t, "https://example.com/a"), decisionURL, true)
	m.Remember(mustParseTestURL(t, "https://denied.example/a"), decisionDomain, false)

	var reloaded decisionMemory
	reloaded.LoadFromFile(path)

	if allowed, known := reloaded.Lookup(mustParseTestURL(t, "https://example.com/a")); !known || !allowed {
		t.Errorf("Lookup(url-scoped) after reload = (%v, %v), want (true, true)", allowed, known)
	}
	if allowed, known := reloaded.Lookup(mustParseTestURL(t, "https://denied.example/b")); !known || allowed {
		t.Errorf("Lookup(domain-scoped) after reload = (%v, %v), want (false, true)", allowed, known)
	}
}

func TestDecisionMemoryRememberOnceDoesNotWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")

	var m decisionMemory
	m.LoadFromFile(path)
	m.Remember(mustParseTestURL(t, "https://example.com/a"), decisionOnce, true)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) error = %v, want a not-exist error -- decisionOnce must never be persisted", path, err)
	}
}

func TestDecisionMemoryLoadFromFileMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	var m decisionMemory
	m.LoadFromFile(path)

	if _, known := m.Lookup(mustParseTestURL(t, "https://example.com/a")); known {
		t.Error("Lookup() known = true after loading a nonexistent file, want false")
	}
}

func TestDecisionMemoryLoadFromFileDeletesMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var m decisionMemory
	m.LoadFromFile(path)

	if _, known := m.Lookup(mustParseTestURL(t, "https://example.com/a")); known {
		t.Error("Lookup() known = true after loading a malformed file, want false")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) error = %v, want a not-exist error -- a malformed file must be deleted", path, err)
	}

	// The malformed file being gone must not have disabled persistence
	// going forward.
	m.Remember(mustParseTestURL(t, "https://example.com/a"), decisionURL, true)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("os.Stat(%q) error = %v, want the file to exist again after a fresh Remember", path, err)
	}
}

func TestParseAnswer(t *testing.T) {
	tests := []struct {
		input     string
		wantAllow bool
		wantScope decisionScope
	}{
		{"y", true, decisionOnce},
		{"Y", true, decisionOnce},
		{"  y  ", true, decisionOnce},
		{"yu", true, decisionURL},
		{"YU", true, decisionURL},
		{"yd", true, decisionDomain},
		{"n", false, decisionOnce},
		{"nu", false, decisionURL},
		{"nd", false, decisionDomain},
		{"whatever", false, decisionOnce},
		{"", false, decisionOnce},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			allowed, scope := parseAnswer(tt.input)
			if allowed != tt.wantAllow || scope != tt.wantScope {
				t.Errorf("parseAnswer(%q) = (%v, %v), want (%v, %v)", tt.input, allowed, scope, tt.wantAllow, tt.wantScope)
			}
		})
	}
}
