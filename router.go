// Package oneb provides typed JSON-RPC 2.0 handlers and method groups over HTTP.
package oneb

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/jhttp"
)

type registrar interface {
	register(string, jrpc2.Handler)
	recording() bool
	record(Operation)
}

// Router serves JSON-RPC methods over HTTP. Create one with New and call Close
// when the HTTP server has stopped using it.
type Router struct {
	methods   *methodRegistry
	bridge    jhttp.Bridge
	contracts *Registry
}

// New creates a router accepting POST requests with Content-Type application/json.
// It supports JSON-RPC calls, notifications, and batches.
func New() *Router {
	r := &Router{methods: &methodRegistry{handlers: make(map[string]jrpc2.Handler)}}
	r.bridge = jhttp.NewBridge(r.methods, nil)
	return r
}

// ServeHTTP implements http.Handler. Mount the router at an HTTP path such as /rpc.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.bridge.ServeHTTP(w, req)
}

// Close stops the underlying JSON-RPC server and releases its resources.
func (r *Router) Close() error {
	return r.bridge.Close()
}

// Group creates a namespace whose methods are named name/method. Groups share
// their router's HTTP endpoint; they do not create additional HTTP routes.
func (r *Router) Group(name string) *Group {
	return &Group{router: r, prefix: methodName("", name)}
}

func (r *Router) register(name string, method jrpc2.Handler) {
	r.methods.add(methodName("", name), method)
}

func (r *Router) recording() bool { return r.contracts != nil }

func (r *Router) record(operation Operation) {
	operation.Name = methodName("", operation.Name)
	r.contracts.record(operation)
}

// Group registers methods within a namespace belonging to a Router.
type Group struct {
	router *Router
	prefix string
}

// Group creates a nested namespace, for example math/v1/method.
func (g *Group) Group(name string) *Group {
	return &Group{router: g.router, prefix: methodName(g.prefix, name)}
}

func (g *Group) register(name string, method jrpc2.Handler) {
	g.router.methods.add(methodName(g.prefix, name), method)
}

func (g *Group) recording() bool { return g.router.recording() }

func (g *Group) record(operation Operation) {
	operation.Name = methodName(g.prefix, operation.Name)
	g.router.contracts.record(operation)
}

func methodName(prefix, name string) string {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		panic("oneb: names must contain non-empty slash-separated segments")
	}
	if prefix != "" {
		name = prefix + "/" + name
	}
	if strings.HasPrefix(name, "rpc.") {
		panic("oneb: names beginning with rpc. are reserved by JSON-RPC")
	}
	return name
}

// The bridge starts before registration, so protect the registry during both
// registration and dispatch, including concurrent HTTP requests.
type methodRegistry struct {
	mu       sync.RWMutex
	handlers map[string]jrpc2.Handler
}

func (m *methodRegistry) add(name string, method jrpc2.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.handlers[name]; exists {
		panic("oneb: method already registered: " + name)
	}
	m.handlers[name] = method
}

func (m *methodRegistry) Assign(_ context.Context, name string) jrpc2.Handler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.handlers[name]
}

func (m *methodRegistry) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.handlers))
	for name := range m.handlers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
