# ChatGPT authentication for oneb

The root `github.com/DawnKosmos/oneb` package owns local ChatGPT registration, OAuth, verified user profiles, token renewal, revocation, and the loopback callback. The application supplies a typed `Store` and its HTTP server. This client is for a single-user local app at `http://127.0.0.1:<port>`.

```go
client, err := oneb.NewChatGPT(ctx, oneb.ChatGPTConfig{
    AppName: "my-app",
    AppURL:  "http://127.0.0.1:8080",
    Store:   myStore,
})
if err != nil {
    return err
}
defer client.Close()

router := oneb.New()
defer router.Close()
client.Routes(router)
registerApplicationRoutes(router)

mux := http.NewServeMux()
mux.Handle("/rpc", router)
mux.Handle("/", frontend)
server := &http.Server{
    Addr:    "127.0.0.1:8080",
    Handler: client.Handler(mux),
}
```

Start the HTTP server before calling sign-in. The library derives `/auth/callback` from `AppURL`; the application does not implement the callback. `Handler` also issues an HttpOnly browser-access cookie on `GET /` and checks host and origin on POST requests. This cookie permits local frontend RPC access before sign-in; provider access still requires signed-in credentials. It is a single-user local boundary, not hosted multi-user application authentication. Application GET routes must not return private information without their own access checks.

`Routes` registers typed `auth/start`, `auth/status`, and `auth/logout` methods. The existing `oneb_gen` generator produces their frontend contracts. `auth/start` returns a browser authorization URL; the frontend navigates there. `auth/status` exposes `User`, connection identity, `signed_in`, and `ready`, never tokens. `auth/logout` clears all local tokens and attempts revocation; a warning reports unconfirmed remote revocation. Registration and profile metadata are retained for subsequent sign-in.

## Typed persistence

```go
type Store interface {
    Load(ctx context.Context, key ConnectionKey) (Connection, error)
    Save(ctx context.Context, connection Connection) error
    Delete(ctx context.Context, key ConnectionKey) error
    LoadRuntime(ctx context.Context) (Runtime, error)
    SaveRuntime(ctx context.Context, runtime Runtime) error
}
```

See [store.go](../store.go) for the complete DTOs:

- `User`: verified issuer/subject identity, email, optional email verification and name.
- `ConnectionKey`: issuer + subject + issued client ID. Email is not a key. Two workspace registrations for the same identity remain separate.
- `Connection`: public user information and server-only `Credentials` (tokens, scopes and renewal timestamps).
- `Runtime`: stable host ID and selected connection. The library generates the host ID and saves/restores it through these two methods.

Store implementations return `ErrNotFound` for absent records, atomically replace complete records, and make `Delete` idempotent. All credential storage must be private. One client/process owns a runtime; sharing a rotating session across processes needs storage-level coordination. SQL columns, encrypted records, or other persistence layouts are application choices; no opaque library blobs need decoding.

The SQLite adapter in [example/chat/store.go](../example/chat/store.go) demonstrates typed user columns, complete credential replacement, runtime persistence and migration of the example's original account record.

## Provider requests

```go
response, err := client.Do(ctx, http.MethodGet, "/models", nil)
if err != nil {
    return err
}
defer response.Body.Close()
```

`Do` loads the selected connection and renews tokens automatically. Requests are restricted to the public OpenAI `/v1` API; no access-token retrieval is needed in application handlers. Always close response bodies. Sign-out, account switching, and `Close` wait until active response bodies are closed. No inference request is automatically retried.

`Selected(ctx)` returns the public key and user for application history ownership. `Select(ctx, key)` lets trusted backend code switch to a saved registration. `Close` releases this client without signing the user out. `ErrSignInRequired`, `ErrPermissionRequired`, and `ErrClosed` are available through `errors.Is`.

Identity-only sign-in is retained with `ready=false`; it does not authorize model requests. The library follows the documented [local sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in) and [session renewal](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions) flows. Model entitlement and usage limits are enforced by OpenAI.
