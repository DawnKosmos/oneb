# oneb

Typed JSON-RPC 2.0 methods and groups over HTTP, backed by `creachadair/jrpc2`.
Requires Go 1.27 or later for generic methods.

```go
type AddInput struct {
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
}

type AddOutput struct {
	Sum float64 `json:"sum"`
}

router := oneb.New()
defer router.Close()

math := router.Group("math")
math.Add[AddInput, AddOutput]("add", func(ctx context.Context, input AddInput) (AddOutput, error) {
	return AddOutput{Sum: input.Left + input.Right}, nil
}, oneb.WithStrictParams())

mux := http.NewServeMux()
mux.Handle("/rpc", router)
```

Import `github.com/DawnKosmos/oneb`. The router implements `http.Handler` and
accepts `POST` requests with `Content-Type: application/json`, including batch
requests and notifications. Call `Close` after the HTTP server stops.

Methods registered on a group have names such as `math/add`. Nested groups
produce names such as `math/v1/add`. Groups share the same HTTP endpoint.
Register on the router itself for an unprefixed method name.

`router.Add("name", handler, options...)` and `group.Add("name", handler, options...)`
infer input and output types from `func(context.Context, Input) (Output, error)`. You can also
specify them explicitly with `group.Add[Input, Output]("name", handler, options...)`.
Every handler receives the JSON-RPC request context through `Add`. The current
HTTP bridge does not forward values from the HTTP request context, so user
identity set by HTTP middleware is not yet available to handlers automatically.

Struct inputs use object parameters by default. `WithPositionalParams()` also
accepts arrays in exported field declaration order. Slice inputs already accept
arrays. `WithStrictParams()` rejects unknown object fields; handlers remain
responsible for validating required fields and business rules. Registration
panics on invalid names, duplicate names, and nil handlers. Registration and
HTTP dispatch may run concurrently.

Run the example:

```sh
go run ./example
```

```sh
curl http://localhost:8080/rpc \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"math/add","params":{"left":2,"right":3},"id":1}'
```

```json
{"jsonrpc":"2.0","id":1,"result":{"sum":5}}
```

## TypeScript generation

Keep registration in a function or handler method that accepts `*oneb.Router`.
Pass that function and the output folder to `oneb_gen.Generate`:

```go
import "github.com/DawnKosmos/oneb/oneb_gen"

func (h *Handler) Routes(router *oneb.Router) {
	math := router.Group("math")
	math.Add("add", h.Add)
}

err := oneb_gen.Generate(h.Routes, "./generated")
```

Generation writes `models.ts` and `client.ts`, overwriting those two files.
It records each `Add`'s full method name and input/output types on a temporary
router; application handlers are never executed. Registration must complete
synchronously. Normal routers do not collect generation metadata, and separate
captures can run concurrently.

```ts
import { createClient } from "./generated/client";

const rpc = createClient("http://localhost:8080/rpc", {
  headers: { Authorization: "Bearer token" },
});
const result = await rpc["math/add"]({ left: 2, right: 3 });
console.log(result.sum);
```

The client uses `fetch` and POST JSON-RPC requests. RPC errors throw `RpcError`
with `code`, `message`, and optional `data`. Fetch options also support an
`AbortSignal`. Opting into `WithPositionalParams()` adds the corresponding tuple
to the generated parameter type.

Models follow JSON field names, omitted fields, optional tags, embedded fields,
and nested types. Reflection does not recover enum constant values or source
directives such as `gogen:omit`. Unsupported custom JSON marshalers and conflicting
model names return errors before generation writes files. `time.Time` is a
string, `json.RawMessage` is `unknown`, and byte slices are base64 strings.

Generate from the example without starting the server:

```sh
go run ./example -generate ./example/generated
```

## ChatGPT chat MVP

For a minimal local app with ChatGPT sign-in, chat, a model selector, and SQLite storage, see [example/chat](example/chat/README.md):

```sh
cd example/chat
go run .
```

Open `http://127.0.0.1:8080`. The example has its own Go module and embedded frontend.

## ChatGPT authentication

Use [oneb’s ChatGPT client](docs/CHATGPT_AUTH.md) to inject typed credential storage into a local ChatGPT client:

```go
client, err := oneb.NewChatGPT(ctx, oneb.ChatGPTConfig{
    AppName: "my-app",
    AppURL:  "http://127.0.0.1:8080",
    Store:   myStore,
})
// Handle err, then defer client.Close().
client.Routes(router)
server.Handler = client.Handler(mux)
```

The library handles OAuth, identity validation, refresh, and sign-out. `oneb.Store` exposes typed user/connection records and runtime metadata. The chat example supplies a SQLite implementation; application handlers make provider requests through `client.Do` without handling access tokens.
