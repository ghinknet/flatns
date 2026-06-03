package provider

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a Provider from a Config. Each vendor package registers its
// factory from an init() function so that the main binary only needs to import
// the package for its side effects.
type Factory func(cfg Config) (Provider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a provider factory under the given name. It panics on duplicate
// registration because that always indicates a programming error. This is the
// single extension point new vendors hook into.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("provider: factory %q already registered", name))
	}
	registry[name] = factory
}

// New constructs a provider for cfg.Name using the registered factory.
func New(cfg Config) (Provider, error) {
	registryMu.RLock()
	factory, ok := registry[cfg.Name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: unknown provider %q (available: %v)", cfg.Name, Registered())
	}
	return factory(cfg)
}

// Registered returns the sorted list of registered provider names. Useful for
// error messages and CLI help.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
