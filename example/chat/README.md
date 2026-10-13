# Simple ChatGPT chat MVP

A local, single-user app: Continue with ChatGPT, chat, and choose a model.
The backend uses **oneb** typed JSON-RPC handlers; the frontend is plain HTML/CSS/JavaScript embedded in the Go binary. The library’s `oneb.ChatGPTClient` owns sign-in and authentication. The example injects a typed `oneb.Store` backed by SQLite, storing user identity/profile fields, credentials, selected registration, stable host ID, and conversation history. There is no frontend build step.

## Run

Requires Go **1.27.1+** and a C compiler for the SQLite driver (Xcode Command Line Tools on macOS, or GCC on Linux).

From the repository root:

```sh
cd example/chat
go run .
```

Open **http://127.0.0.1:8080**, click **Continue with ChatGPT**, authorize ChatGPT plan usage, choose a model, and send a message. Use `127.0.0.1` exactly; the loopback OAuth flow does not use `localhost`.

The model picker only offers **GPT 5.6 Luna** (`gpt-5.6-luna`) and **GPT 6.1 Sol** (`gpt-6.1-sol`) as explicit choices. Models missing from the signed-in account's live catalog are marked **not listed**, with a note that sending a message checks actual access. Selecting a model does not guarantee the account can use it. The **Thinking mode** selector sets reasoning effort: Low, Medium (default), High, Extra high, or Max. Luna also offers Off. Unsupported models and thinking settings are rejected by the backend. [Luna settings](https://developers.openai.com/api/docs/models/gpt-5.6-luna), [Sol settings](https://developers.openai.com/api/docs/models/gpt-6.1-sol). Changing it applies to the next message and retains conversation context. Completed exchanges are saved in SQLite and restored after restart. The frontend waits for the full reply; the backend consumes OpenAI's SSE stream and requires a successful terminal event before saving either message. Failed messages remain in the composer for retry.

```sh
go run . -port 8081 -data /path/to/private/oneb-chat
# Or build a single executable with the frontend embedded:
go build -o /tmp/oneb-chat .
```

By default data lives in `os.UserConfigDir()/oneb-chat/chat.sqlite` (on macOS: `~/Library/Application Support/oneb-chat/chat.sqlite`). The data directory is owner-only (`0700`) and the database is owner-only (`0600`). It contains OAuth credentials, so keep the directory private. Run only one app process per data directory. The app binds exclusively to `127.0.0.1` and protects RPC calls with a local HttpOnly cookie, origin and host checks.

Sign out attempts to revoke the renewable OAuth session and removes its local tokens. It keeps the account/client mapping, host ID, and chat history for subsequent sign-in. If remote revocation cannot be confirmed, the UI reports that and links to ChatGPT settings. This MVP has one account registration and one conversation; use another private data directory for a separate conversation.

This uses the documented [local open-source Sign in with ChatGPT flow](https://developers.openai.com/siwc/token-sharing-open-source/sign-in), not Codex's cached credentials. No API key or client secret is required. ChatGPT plan eligibility, permission, available models, and usage limits are controlled by OpenAI. Hosted deployment requires a different approved integration. [Models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)

## Verify

```sh
go test -race ./...
go vet ./...
```

Tests use a fake OpenAI transport and signed test ID tokens; they cover identity verification, nonce/audience/account mismatch, denied plan permission, PKCE, serialized refresh rotation, model selection, history isolation/persistence, failed streams, and HTTP origin protection. Real OAuth authorization and live inference require an eligible user's interactive sign-in.

The example is a separate Go module using `replace ../..`. Its SQLite dependency stays in the example; OIDC verification belongs to the root oneb package. The adapter automatically migrates the previous example’s account/host records while retaining tokens and chat history. See [the library auth interface](../../docs/CHATGPT_AUTH.md) and [SQLite adapter](store.go). From the repository root, `go test ./...` tests the library; run the commands above inside `example/chat` to test this MVP.
