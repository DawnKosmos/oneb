# Auth flow in oneb

These diagrams describe the current local chat example and `oneb.ChatGPTClient` implementation. “Sign up” means connecting/registering this application with a ChatGPT account for the first time; the OpenAI account sign-in/sign-up and consent UI belongs to OpenAI.

## 1. First sign-up / sign-in

At startup, oneb calls `Store.LoadRuntime`. If no runtime exists, it generates a host ID and calls `Store.SaveRuntime`. It reuses the saved host ID on subsequent launches.

```mermaid
flowchart TD
    A["1. Browser opens app: GET /"] --> B["oneb sets local HttpOnly browser cookie"]
    B --> C["2. Click Continue with ChatGPT: POST /rpc, auth/start"]
    C --> D["oneb generates state, nonce and PKCE; returns authorization URL"]
    D --> E["3. Browser opens OpenAI authorization page; user signs in and consents"]
    E --> F["4. OpenAI redirects browser to GET /auth/callback with code and state"]
    F --> G["oneb checks state, expiry, errors and issued client ID"]
    G --> H["5. oneb exchanges code for tokens at OpenAI token endpoint"]
    H --> I["6. oneb fetches discovery and signing keys; validates ID token and nonce"]
    I --> J["7. Store.Save: verified user, registration and credentials"]
    J --> K["Store.SaveRuntime: host ID and selected connection"]
    K --> L["8. Redirect to app; auth/status returns public user information"]
```

Verification checks signature, issuer, audience, token expiration, nonce, and verified subject. For a returning registration, its identity must match the saved connection. Invalid authorization is rejected before any connection is saved. Pending authorization attempts live in memory and expire after ten minutes.

`Connection` stores issuer/subject identity, email, optional name/email-verification claims, client ID, tokens, scopes and renewal timestamps. Access, refresh and ID tokens stay server-side. If identity verification succeeds without ChatGPT plan permission, the connection is saved with `ready=false`; model requests are blocked.

### HTTP requests

`APP` below means `http://127.0.0.1:<port>`.

| Step | Caller → receiver | Request | Important data |
| --- | --- | --- | --- |
| 1 | Browser → app | `GET APP/` | Response sets the local HttpOnly cookie. |
| 2 | Browser → app | `POST APP/rpc` | JSON-RPC method `auth/start`, params `{}`; cookie and matching Origin. |
| 3 | Browser → OpenAI | `GET https://auth.openai.com/api/accounts/authorize` | Authorization query below. |
| 4 | Browser → app | `GET APP/auth/callback` | `code`, `state`, issued `client_id`; or OAuth `error`. |
| 5 | oneb → OpenAI | `POST https://auth.openai.com/api/accounts/oauth/token` | Form-encoded authorization-code grant below. |
| 6 | oneb → OpenAI | `GET https://auth.openai.com/.well-known/openid-configuration` | Discovers issuer and JWKS URL. |
| 6 | oneb → OpenAI | `GET <jwks_uri from discovery>` | Fetches public signing keys when needed for verification. |
| 8 | Browser → app | `GET APP/`, then `POST APP/rpc` | JSON-RPC `auth/status`, params `{}`. |

Authorization query for a new registration:

```text
client_id=dynamic_agent_client
agent_name_hint=<configured app name>
ext_agent_host_id=<persisted host ID>
response_type=code
redirect_uri=APP/auth/callback
scope=openid profile email offline_access resource.invoke chatgpt.tokens.use.direct
resource=https://api.openai.com/v1
state=<random attempt state>
nonce=<random OIDC nonce>
code_challenge_method=S256
code_challenge=<SHA-256 digest of PKCE verifier, base64url encoded>
```

For returning sign-in, oneb uses the saved issued `client_id`, omits `agent_name_hint`, and adds a retained `id_token_hint` when present. The callback can omit `client_id` for that returning registration.

Token exchange form:

```text
grant_type=authorization_code
client_id=<issued client ID>
code=<callback authorization code>
code_verifier=<saved PKCE verifier>
redirect_uri=APP/auth/callback
resource=https://api.openai.com/v1
```

## 2. Request to OpenAI

```mermaid
flowchart TD
    A["1. Browser sends POST /rpc, chat/send"] --> B["oneb checks cookie and Origin; dispatches typed chat handler"]
    B --> C["2. Chat handler loads history and calls ChatGPTClient.Do"]
    C --> D["3. Store.Load: selected connection and credentials"]
    D --> E{"Signed in and plan permission granted?"}
    E -- No --> X["Return error; no inference request"]
    E -- Yes --> F{"Token needs refresh and refresh is allowed?"}
    F -- Yes --> G["4. POST OpenAI token endpoint with refresh_token grant"]
    G --> H["Store.Save: atomically replace renewed credentials"]
    H --> I["5. POST /v1/responses with bearer access token"]
    F -- No --> I
    I --> J["6. OpenAI streams SSE response"]
    J --> K{"response.completed received successfully?"}
    K -- Yes --> L["7. Example saves user message and reply to SQLite; returns JSON-RPC result"]
    K -- No --> M["Return error; example does not save the failed exchange"]
```

Refresh is attempted within one minute of expiration, provided `EarliestRefreshAt` has passed. After any refresh, oneb checks plan permission and expiration again before sending the inference request. An expired token, refresh failure, or storage failure stops the request. Concurrent renewals are serialized by the client.

The library owns credentials and authentication; the example owns conversation history, the model/thinking selection, Responses input, SSE parsing and successful-turn persistence.

### HTTP requests

| Step | Caller → receiver | Request | Important data |
| --- | --- | --- | --- |
| 1 | Browser → app | `POST APP/rpc` | JSON-RPC `chat/send`, params `{message, model, reasoning_effort}`; local cookie and matching Origin. |
| 4, if needed | oneb → OpenAI | `POST https://auth.openai.com/api/accounts/oauth/token` | Form: `grant_type=refresh_token`, issued `client_id`, current `refresh_token`, `resource=https://api.openai.com/v1`. |
| 5 | oneb → OpenAI | `POST https://api.openai.com/v1/responses` | `Authorization: Bearer <access_token>` and JSON body below. |

Inference request body:

```json
{
  "model": "gpt-6.1-sol",
  "input": [
    {"role": "user", "content": "Hello"}
  ],
  "reasoning": {"effort": "medium"},
  "store": false,
  "stream": true
}
```

The actual input includes the example's saved history plus the new message. Token refresh and inference run in the backend; the browser never receives provider credentials.

Additional requests used by the example:

| Purpose | Browser → app | Backend → OpenAI |
| --- | --- | --- |
| Populate model picker | `POST APP/rpc`, method `models/list` | `GET https://api.openai.com/v1/models` with bearer token; automatic renewal if necessary. |
| Restore conversation | `POST APP/rpc`, method `chat/history` | None; reads local SQLite. |
| Sign out | `POST APP/rpc`, method `auth/logout` | Discovery GET, then POST to its `revocation_endpoint` with `token=<refresh_token>`, `token_type_hint=refresh_token`, and issued `client_id`. |

`Store` calls are local persistence operations, not HTTP requests in the SQLite example. No user-info request is made: profile fields come from the verified ID token.
