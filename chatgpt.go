package oneb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const issuer = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"
const scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

var (
	ErrSignInRequired     = errors.New("chatgpt: sign in required")
	ErrPermissionRequired = errors.New("chatgpt: ChatGPT plan usage permission required")
	ErrClosed             = errors.New("chatgpt: client closed")
)

// ChatGPTConfig injects persistence and local application settings. HTTPClient is
// optional and can supply a custom transport for testing or network settings.
type ChatGPTConfig struct {
	AppName    string
	AppURL     string // http://127.0.0.1:<port>; also the post-login return location
	Store      Store
	HTTPClient *http.Client
}

// ChatGPTClient owns OAuth attempts, verified user records and rotating refresh tokens.
// It is scoped to one local application's selected account, not a hosted tenant.
// Register Routes on a oneb router and wrap the application in Handler.
type ChatGPTClient struct {
	store                                       Store
	http                                        *http.Client
	appName, origin, hostID, cookie, cookieName string
	mu                                          sync.Mutex
	requests                                    sync.RWMutex
	active                                      *ConnectionKey
	pending                                     *attempt
	closed                                      bool
}
type attempt struct {
	State, Nonce, Verifier string
	Key                    *ConnectionKey
	Expires                time.Time
}
type tokenReply struct {
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	IDToken           string `json:"id_token"`
	TokenType         string `json:"token_type"`
	Scope             string `json:"scope"`
	ExpiresIn         int64  `json:"expires_in"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at"`
}

// NewChatGPT restores the host and selected registration using the injected Store.
func NewChatGPT(ctx context.Context, config ChatGPTConfig) (*ChatGPTClient, error) {
	if config.Store == nil || strings.TrimSpace(config.AppName) == "" {
		return nil, errors.New("chatgpt: Store and AppName are required")
	}
	u, err := url.Parse(config.AppURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("chatgpt: AppURL must be http://127.0.0.1:<port>")
	}
	if _, _, err = net.SplitHostPort(u.Host); err != nil {
		return nil, errors.New("chatgpt: invalid loopback address")
	}
	origin := "http://" + u.Host
	c := &ChatGPTClient{store: config.Store, http: config.HTTPClient, appName: config.AppName, origin: origin, cookie: random()}
	if c.http == nil {
		c.http = &http.Client{Timeout: 3 * time.Minute}
	}
	hash := sha256.Sum256([]byte(config.AppName + origin))
	c.cookieName = fmt.Sprintf("oneb-chatgpt-%x", hash[:6])
	runtime, err := c.store.LoadRuntime(ctx)
	if errors.Is(err, ErrNotFound) {
		runtime = Runtime{HostID: newHostID()}
		err = c.store.SaveRuntime(ctx, runtime)
	}
	if err != nil {
		return nil, fmt.Errorf("chatgpt: load runtime: %w", err)
	}
	if runtime.HostID == "" {
		return nil, errors.New("chatgpt: stored host ID is empty")
	}
	c.hostID = runtime.HostID
	c.active = runtime.ActiveConnection
	if c.active != nil {
		if _, err = c.load(ctx); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func newHostID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
func (c *ChatGPTClient) load(ctx context.Context) (Connection, error) {
	if c.closed {
		return Connection{}, ErrClosed
	}
	if c.active == nil {
		return Connection{}, ErrSignInRequired
	}
	record, err := c.store.Load(ctx, *c.active)
	if err != nil {
		return Connection{}, fmt.Errorf("chatgpt: load connection: %w", err)
	}
	if record.Key != *c.active || record.User.Identity != record.Key.Identity {
		return Connection{}, errors.New("chatgpt: stored connection identity mismatch")
	}
	return record, nil
}

// Status returns public user information; no credentials are exposed.
type Status struct {
	SignedIn bool           `json:"signed_in"`
	Ready    bool           `json:"ready"`
	Email    string         `json:"email"` // convenience for simple frontends
	User     *User          `json:"user,omitempty"`
	Key      *ConnectionKey `json:"connection,omitempty"`
}

func (c *ChatGPTClient) Status(ctx context.Context) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err := c.load(ctx)
	if errors.Is(err, ErrSignInRequired) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	signedIn := record.Credentials.AccessToken != ""
	result := Status{SignedIn: signedIn}
	if signedIn {
		result.Email = record.User.Email
		result.User = &record.User
		result.Key = &record.Key
		result.Ready = contains(record.Credentials.Scopes, "chatgpt.tokens.use.direct")
	}
	return result, nil
}

// Selected returns the active public identity and registration for application
// history ownership. It does not return tokens or trigger token renewal.
func (c *ChatGPTClient) Selected(ctx context.Context) (ConnectionKey, User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load(ctx)
	if err != nil {
		return ConnectionKey{}, User{}, err
	}
	if r.Credentials.AccessToken == "" {
		return ConnectionKey{}, User{}, ErrSignInRequired
	}
	return r.Key, r.User, nil
}

// Select switches to an already persisted registration. Intended for trusted
// backend account-selection code in this single-user local client.
func (c *ChatGPTClient) Select(ctx context.Context, key ConnectionKey) error {
	c.requests.Lock()
	defer c.requests.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	r, err := c.store.Load(ctx, key)
	if err != nil {
		return err
	}
	if r.Key != key || r.User.Identity != key.Identity {
		return errors.New("chatgpt: stored connection identity mismatch")
	}
	if err = c.store.SaveRuntime(ctx, Runtime{HostID: c.hostID, ActiveConnection: &key}); err != nil {
		return err
	}
	c.active = &key
	c.pending = nil
	return nil
}

type SignIn struct {
	URL string `json:"url"`
}

// StartSignIn generates an authorization attempt. The callback is owned by Handler.
func (c *ChatGPTClient) StartSignIn(ctx context.Context) (SignIn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return SignIn{}, ErrClosed
	}
	p := &attempt{State: random(), Nonce: random(), Verifier: random(), Expires: time.Now().Add(10 * time.Minute)}
	id := "dynamic_agent_client"
	var record Connection
	if c.active != nil {
		var err error
		record, err = c.load(ctx)
		if err != nil {
			return SignIn{}, err
		}
		key := record.Key
		p.Key = &key
		id = key.ClientID
	}
	digest := sha256.Sum256([]byte(p.Verifier))
	q := url.Values{"client_id": {id}, "ext_agent_host_id": {c.hostID}, "response_type": {"code"}, "redirect_uri": {c.origin + "/auth/callback"}, "scope": {scopes}, "resource": {resource}, "state": {p.State}, "nonce": {p.Nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}}
	if p.Key == nil {
		q.Set("agent_name_hint", c.appName)
	} else if record.Credentials.IDToken != "" {
		q.Set("id_token_hint", record.Credentials.IDToken)
	}
	c.pending = p
	return SignIn{URL: issuer + "/api/accounts/authorize?" + q.Encode()}, nil
}
func (c *ChatGPTClient) callback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c.requests.Lock()
	defer c.requests.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func(msg string) { http.Error(w, msg+" Return to the app and try again.", 400) }
	if c.closed {
		fail("Client closed.")
		return
	}
	p := c.pending
	if p == nil || p.State != r.URL.Query().Get("state") || time.Now().After(p.Expires) {
		fail("Invalid or expired sign-in.")
		return
	}
	c.pending = nil
	if r.URL.Query().Get("error") != "" {
		fail("ChatGPT authorization was declined.")
		return
	}
	id := r.URL.Query().Get("client_id")
	if p.Key != nil {
		if id != "" && id != p.Key.ClientID {
			fail("Client identity mismatch.")
			return
		}
		id = p.Key.ClientID
	}
	if id == "" || id == "dynamic_agent_client" || r.URL.Query().Get("code") == "" {
		fail("Incomplete registration.")
		return
	}
	t, err := c.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {r.URL.Query().Get("code")}, "code_verifier": {p.Verifier}, "redirect_uri": {c.origin + "/auth/callback"}, "resource": {resource}})
	if err != nil {
		fail("Could not exchange ChatGPT authorization.")
		return
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, c.http), issuer)
	if err != nil {
		fail("Could not load OpenAI identity verification.")
		return
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: id}).Verify(ctx, t.IDToken)
	if err != nil {
		fail("Could not verify ChatGPT identity.")
		return
	}
	identity := Identity{Issuer: verified.Issuer, Subject: verified.Subject}
	if verified.Nonce != p.Nonce || identity.Subject == "" || p.Key != nil && p.Key.Identity != identity {
		fail("Account identity mismatch.")
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err = verified.Claims(&claims); err != nil {
		fail("Invalid identity claims.")
		return
	}
	record := Connection{Key: ConnectionKey{Identity: identity, ClientID: id}, User: User{Identity: identity, Email: claims.Email, EmailVerified: claims.EmailVerified, Name: claims.Name}, Credentials: applyTokens(Credentials{}, t)}
	if err = c.store.Save(ctx, record); err != nil {
		fail("Could not save sign-in.")
		return
	}
	if err = c.store.SaveRuntime(ctx, Runtime{HostID: c.hostID, ActiveConnection: &record.Key}); err != nil {
		fail("Could not save selected account.")
		return
	}
	c.active = &record.Key
	http.Redirect(w, r, c.origin+"/", http.StatusSeeOther)
}
func (c *ChatGPTClient) exchange(ctx context.Context, form url.Values) (tokenReply, error) {
	var t tokenReply
	req, err := http.NewRequestWithContext(ctx, "POST", issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return t, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		return t, errors.New("chatgpt: could not reach token endpoint")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return t, fmt.Errorf("chatgpt: token endpoint returned HTTP %d; sign in again", res.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); err != nil {
		return t, errors.New("chatgpt: invalid token response")
	}
	if t.AccessToken == "" || !strings.EqualFold(t.TokenType, "Bearer") || t.ExpiresIn <= 0 {
		return t, errors.New("chatgpt: incomplete token response")
	}
	if form.Get("grant_type") == "refresh_token" && t.RefreshToken == "" {
		return t, errors.New("chatgpt: refresh response omitted replacement refresh token")
	}
	return t, nil
}
func applyTokens(previous Credentials, t tokenReply) Credentials {
	previous.AccessToken = t.AccessToken
	previous.RefreshToken = t.RefreshToken
	previous.TokenType = t.TokenType
	if t.IDToken != "" {
		previous.IDToken = t.IDToken
	}
	if t.Scope != "" {
		previous.Scopes = strings.Fields(t.Scope)
	}
	previous.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	previous.EarliestRefreshAt = time.Time{}
	if t.EarliestRefreshAt > 0 {
		previous.EarliestRefreshAt = time.Unix(t.EarliestRefreshAt, 0)
	}
	return previous
}
func (c *ChatGPTClient) credentials(ctx context.Context) (Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err := c.load(ctx)
	if err != nil {
		return Connection{}, err
	}
	if record.Credentials.AccessToken == "" {
		return Connection{}, ErrSignInRequired
	}
	if !contains(record.Credentials.Scopes, "chatgpt.tokens.use.direct") {
		return Connection{}, ErrPermissionRequired
	}
	if time.Now().Add(time.Minute).After(record.Credentials.ExpiresAt) && !time.Now().Before(record.Credentials.EarliestRefreshAt) {
		if record.Credentials.RefreshToken == "" {
			return Connection{}, ErrSignInRequired
		}
		t, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {record.Key.ClientID}, "refresh_token": {record.Credentials.RefreshToken}, "resource": {resource}})
		if err != nil {
			return Connection{}, err
		}
		record.Credentials = applyTokens(record.Credentials, t)
		if err = c.store.Save(ctx, record); err != nil {
			return Connection{}, fmt.Errorf("chatgpt: save renewed credentials: %w", err)
		}
		if !contains(record.Credentials.Scopes, "chatgpt.tokens.use.direct") {
			return Connection{}, ErrPermissionRequired
		}
	}
	if !time.Now().Before(record.Credentials.ExpiresAt) {
		return Connection{}, ErrSignInRequired
	}
	return record, nil
}

// Do sends an authenticated request to the public OpenAI /v1 API, automatically
// loading and refreshing credentials. path must be relative, e.g. "/models" or
// "/responses"; credentials cannot be forwarded to another host. Close Body.
func (c *ChatGPTClient) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || u.Fragment != "" {
		return nil, errors.New("chatgpt: API path must be relative to /v1")
	}
	c.requests.RLock()
	unlock := true
	defer func() {
		if unlock {
			c.requests.RUnlock()
		}
	}()
	record, err := c.credentials(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, resource+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+record.Credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("chatgpt: could not reach OpenAI")
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("OpenAI returned HTTP %d. Check your ChatGPT connection and plan usage", res.StatusCode)
	}
	res.Body = &lockedBody{ReadCloser: res.Body, release: c.requests.RUnlock}
	unlock = false
	return res, nil
}

type lockedBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *lockedBody) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.release); return err }

type SignOut struct {
	Warning string `json:"warning"`
}

// SignOut revokes the renewable session, clears credentials and retains identity
// and registration metadata. It waits for outstanding Do response bodies to close.
func (c *ChatGPTClient) SignOut(ctx context.Context) (SignOut, error) {
	c.requests.Lock()
	defer c.requests.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	result := SignOut{}
	record, err := c.load(ctx)
	if errors.Is(err, ErrSignInRequired) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if record.Credentials.RefreshToken != "" {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, c.http), issuer)
		var config struct {
			RevocationEndpoint string `json:"revocation_endpoint"`
		}
		if err == nil {
			err = provider.Claims(&config)
		}
		if err == nil && config.RevocationEndpoint != "" {
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, "POST", config.RevocationEndpoint, strings.NewReader(url.Values{"token": {record.Credentials.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {record.Key.ClientID}}.Encode()))
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				var res *http.Response
				res, err = c.http.Do(req)
				if err == nil {
					res.Body.Close()
					if res.StatusCode != 200 {
						err = errors.New("revocation failed")
					}
				}
			}
		} else if err == nil {
			err = errors.New("revocation unavailable")
		}
		if err != nil {
			result.Warning = "Signed out locally. Remote revocation was not confirmed; disconnect the app in ChatGPT Settings."
		}
	}
	record.Credentials = Credentials{}
	c.pending = nil
	if err = c.store.Save(ctx, record); err != nil {
		return result, err
	}
	return result, nil
}

// Close invalidates attempts and stops this client. Persisted credentials are
// retained; use SignOut to revoke and clear them.
func (c *ChatGPTClient) Close() error {
	c.requests.Lock()
	defer c.requests.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.pending = nil
	return nil
}
