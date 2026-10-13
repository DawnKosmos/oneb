package oneb

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Operation describes a typed JSON-RPC method recorded during CaptureRoutes.
type Operation struct {
	Name             string
	Input            reflect.Type
	Output           reflect.Type
	PositionalParams bool
}

// Registry holds captured contracts. Normal routers do not allocate a registry.
type Registry struct {
	mu         sync.RWMutex
	operations []Operation
}

func (r *Registry) record(operation Operation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations = append(r.operations, operation)
}

// Operations returns a sorted snapshot of captured methods.
func (r *Registry) Operations() []Operation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	operations := slices.Clone(r.operations)
	slices.SortFunc(operations, func(a, b Operation) int { return strings.Compare(a.Name, b.Name) })
	return operations
}

// CaptureRoutes invokes register on a fresh router and records each Add's full
// method name and input/output types. Registration must finish synchronously.
// Handlers are never executed. Capture state is local to this router, so captures
// are independent and may run concurrently. The router is closed before return.
func CaptureRoutes(register func(*Router)) *Registry {
	if register == nil {
		panic("oneb: route registration callback must not be nil")
	}
	router := New()
	defer router.Close()
	registry := &Registry{}
	router.contracts = registry
	register(router)
	return registry
}
