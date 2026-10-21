package passthroughspec

import (
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

// Manager caches one compiled Spec per generic provider. A provider rebuild (any config update)
// hands the relay a new *GenericOpenAPIConfig pointer, so the cache keys invalidation on pointer
// identity: a hit is an RLock, a map read and a pointer compare; a miss compiles outside the lock
// and then stores. Entries for providers that no longer exist are swept on the next compile.
type Manager struct {
	mu         sync.RWMutex
	entries    map[schemas.ModelProvider]*entry
	configured func(schemas.ModelProvider) bool
}

type entry struct {
	cfg  *schemas.GenericOpenAPIConfig
	spec *Spec
	err  error
}

// NewManager creates a cache. configured reports whether a provider still exists; nil keeps every
// entry until it is replaced.
func NewManager(configured func(schemas.ModelProvider) bool) *Manager {
	return &Manager{entries: make(map[schemas.ModelProvider]*entry), configured: configured}
}

// Lookup returns the compiled spec for provider's current config, compiling it on first sight.
// A nil cfg means the provider has no document: nothing to enforce, nil spec, nil error.
func (m *Manager) Lookup(provider schemas.ModelProvider, cfg *schemas.GenericOpenAPIConfig) (*Spec, error) {
	if cfg == nil {
		return nil, nil
	}
	m.mu.RLock()
	e := m.entries[provider]
	m.mu.RUnlock()
	if e != nil && e.cfg == cfg {
		return e.spec, e.err
	}

	// Compile without holding the lock: a large document must not stall every other provider's
	// requests while it is parsed. Two concurrent first requests may both compile; the later
	// write wins and the results are equivalent.
	spec, err := Compile(cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	if e = m.entries[provider]; e != nil && e.cfg == cfg {
		return e.spec, e.err
	}
	m.entries[provider] = &entry{cfg: cfg, spec: spec, err: err}
	if m.configured != nil {
		for name := range m.entries {
			if name != provider && !m.configured(name) {
				delete(m.entries, name)
			}
		}
	}
	return spec, err
}

// Len reports how many providers currently hold a compiled spec.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}
