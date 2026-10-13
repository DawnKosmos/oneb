package oneb

import (
	"context"
	"reflect"

	"github.com/creachadair/jrpc2/handler"
)

// Add registers a typed method on the router. The handler receives the JSON-RPC
// request context. The input is decoded from
// params and the output is encoded as result. Struct inputs use JSON objects by
// default; WithPositionalParams also permits arrays in field declaration order.
//
// I and O can usually be inferred from fn.
//
// Add panics for a nil handler, an invalid name, or a duplicate method name.
func (r *Router) Add[I, O any](name string, fn func(context.Context, I) (O, error), options ...MethodOption) {
	add(r, name, fn, options...)
}

// Add registers a typed method within the group's namespace. Parameter decoding
// and registration rules are the same as Router.Add.
func (g *Group) Add[I, O any](name string, fn func(context.Context, I) (O, error), options ...MethodOption) {
	add(g, name, fn, options...)
}

func add[I, O any](target registrar, name string, fn func(context.Context, I) (O, error), options ...MethodOption) {
	if fn == nil {
		panic("oneb: handler must not be nil")
	}
	var config methodOptions
	for _, option := range options {
		option(&config)
	}
	info, err := handler.Check(fn)
	if err != nil {
		panic("oneb: invalid handler: " + err.Error())
	}
	target.register(name, info.AllowArray(config.positional).SetStrict(config.strict).Wrap())
	if target.recording() {
		target.record(Operation{
			Name:             name,
			Input:            reflect.TypeFor[I](),
			Output:           reflect.TypeFor[O](),
			PositionalParams: config.positional,
		})
	}
}

// MethodOption configures parameter decoding for a registered method.
type MethodOption func(*methodOptions)

type methodOptions struct {
	positional bool
	strict     bool
}

// WithPositionalParams allows struct inputs to be provided as JSON arrays in
// field declaration order, as well as JSON objects. The array must contain one
// value per exported field (excluding fields tagged json:"-"). Slice and array
// inputs accept JSON arrays without this option.
func WithPositionalParams() MethodOption {
	return func(options *methodOptions) { options.positional = true }
}

// WithStrictParams rejects unknown JSON object fields in struct inputs. It does
// not enforce required fields; validate those inside the handler as needed.
func WithStrictParams() MethodOption {
	return func(options *methodOptions) { options.strict = true }
}
