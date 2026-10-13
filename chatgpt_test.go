package oneb

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(s string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(s))}
}

type memoryStore struct {
	records  map[ConnectionKey]Connection
	runtime  Runtime
	exists   bool
	failSave bool
}

func (s *memoryStore) Load(_ context.Context, key ConnectionKey) (Connection, error) {
	r, ok := s.records[key]
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}
func (s *memoryStore) Save(_ context.Context, r Connection) error {
	if s.failSave {
		return errors.New("save failed")
	}
	s.records[r.Key] = r
	return nil
}
func (s *memoryStore) Delete(_ context.Context, key ConnectionKey) error {
	delete(s.records, key)
	return nil
}
func (s *memoryStore) LoadRuntime(context.Context) (Runtime, error) {
	if !s.exists {
		return Runtime{}, ErrNotFound
	}
	return s.runtime, nil
}
func (s *memoryStore) SaveRuntime(_ context.Context, r Runtime) error {
	s.runtime = r
	s.exists = true
	return nil
}
func testClient(t *testing.T) *ChatGPTClient {
	t.Helper()
	s := &memoryStore{records: make(map[ConnectionKey]Connection)}
	c, err := NewChatGPT(context.Background(), ChatGPTConfig{AppName: "oneb-chat", AppURL: "http://127.0.0.1:8080", Store: s})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func connect(t *testing.T, c *ChatGPTClient) {
	t.Helper()
	identity := Identity{Issuer: issuer, Subject: "user"}
	record := Connection{Key: ConnectionKey{Identity: identity, ClientID: "client"}, User: User{Identity: identity, Email: "user@example.com", Name: "User"}, Credentials: Credentials{AccessToken: "access", RefreshToken: "refresh", IDToken: "id-secret", TokenType: "Bearer", Scopes: strings.Fields(scopes), ExpiresAt: time.Now().Add(time.Hour)}}
	if err := c.store.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := c.Select(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
}
func TestOAuthRegistrationAndIdentityValidation(t *testing.T) {
	for _, scenario := range []string{"success", "wrong nonce", "wrong audience", "wrong subject", "no plan permission", "wrong state"} {
		t.Run(scenario, func(t *testing.T) {
			a := testClient(t)
			if scenario == "wrong subject" {
				key := ConnectionKey{Identity: Identity{Issuer: issuer, Subject: "original"}, ClientID: "issued"}
				a.store.Save(context.Background(), Connection{Key: key, User: User{Identity: key.Identity}})
				if err := a.Select(context.Background(), key); err != nil {
					t.Fatal(err)
				}
			}
			started, err := a.StartSignIn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			verifier := a.pending.Verifier
			u, _ := url.Parse(started.URL)
			q := u.Query()
			digest := sha256.Sum256([]byte(a.pending.Verifier))
			if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) || q.Get("redirect_uri") != a.origin+"/auth/callback" || q.Get("ext_agent_host_id") != a.hostID {
				t.Fatal("invalid OAuth request")
			}
			if scenario != "wrong subject" && (q.Get("client_id") != "dynamic_agent_client" || q.Get("agent_name_hint") != "oneb-chat") {
				t.Fatal("dynamic registration missing")
			}
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			nonce := a.pending.Nonce
			aud := "issued"
			if scenario == "wrong nonce" {
				nonce = "bad"
			}
			if scenario == "wrong audience" {
				aud = "other"
			}
			claims := map[string]any{"iss": issuer, "sub": "verified-user", "aud": aud, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "email": "verified@example.com", "email_verified": true, "name": "Verified User"}
			payload, _ := json.Marshal(claims)
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test"))
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			jwt, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			var exchanged bool
			a.http = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/accounts/oauth/token":
					exchanged = true
					r.ParseForm()
					if r.Form.Get("client_id") != "issued" || r.Form.Get("code_verifier") != verifier || r.Form.Get("redirect_uri") != a.origin+"/auth/callback" {
						t.Error("incorrect code exchange")
					}
					scope := scopes
					if scenario == "no plan permission" {
						scope = "openid email"
					}
					b, _ := json.Marshal(tokenReply{AccessToken: "access", RefreshToken: "refresh", IDToken: jwt, TokenType: "Bearer", ExpiresIn: 3600, Scope: scope})
					return response(string(b)), nil
				case "/.well-known/openid-configuration":
					return response(`{"issuer":"https://auth.openai.com","jwks_uri":"https://auth.openai.com/jwks","id_token_signing_alg_values_supported":["RS256"]}`), nil
				case "/jwks":
					b, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
					return response(string(b)), nil
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
					return nil, nil
				}
			})}
			state := q.Get("state")
			if scenario == "wrong state" {
				state = "bad"
			}
			callback := a.origin + "/auth/callback?" + url.Values{"code": {"code"}, "state": {state}, "client_id": {"issued"}}.Encode()
			w := httptest.NewRecorder()
			a.callback(w, httptest.NewRequest("GET", callback, nil))
			if scenario == "success" || scenario == "no plan permission" {
				if w.Code != 303 || a.active == nil || a.active.Identity.Subject != "verified-user" || a.active.ClientID != "issued" {
					t.Fatalf("sign-in failed: %d %s", w.Code, w.Body.String())
				}
				saved, err := a.store.Load(context.Background(), *a.active)
				if err != nil || saved.User.Email != "verified@example.com" || saved.User.Name != "Verified User" || saved.User.EmailVerified == nil || !*saved.User.EmailVerified {
					t.Fatal("verified profile was not saved")
				}
				if scenario == "no plan permission" {
					status, _ := a.Status(context.Background())
					if !status.SignedIn || status.Ready {
						t.Fatal("identity-only state was lost")
					}
					if _, err := a.Do(context.Background(), "GET", "/models", nil); !errors.Is(err, ErrPermissionRequired) {
						t.Fatal("identity-only session invoked provider")
					}
				}
			} else if w.Code != 400 {
				t.Fatalf("invalid authorization accepted: %d", w.Code)
			}
			if scenario == "wrong state" && exchanged {
				t.Fatal("exchanged code with invalid state")
			}
		})
	}
}

func TestRefreshIsSerializedAndPersists(t *testing.T) {
	a := testClient(t)
	connect(t, a)
	record, err := a.store.Load(context.Background(), *a.active)
	if err != nil {
		t.Fatal(err)
	}
	record.Credentials.ExpiresAt = time.Now().Add(-time.Minute)
	a.store.Save(context.Background(), record)
	var calls atomic.Int32
	a.http = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != "client" || r.Form.Has("scope") {
			t.Error("wrong refresh grant")
		}
		return response(`{"access_token":"new-access","refresh_token":"rotated","token_type":"Bearer","scope":"chatgpt.tokens.use.direct","expires_in":3600}`), nil
	})}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ac, err := a.credentials(context.Background())
			if err != nil || ac.Credentials.RefreshToken != "rotated" {
				t.Errorf("refresh result: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh raced: %d calls", calls.Load())
	}
	record, err = a.store.Load(context.Background(), *a.active)
	if err != nil || record.Credentials.RefreshToken != "rotated" {
		t.Fatal("rotated token not persisted")
	}
}

func TestTypedProfileStatusAndRestart(t *testing.T) {
	c := testClient(t)
	connect(t, c)
	key := *c.active
	record, _ := c.store.Load(context.Background(), key)
	verified := true
	record.User.EmailVerified = &verified
	c.store.Save(context.Background(), record)
	status, err := c.Status(context.Background())
	if err != nil || status.User == nil || status.User.Identity.Subject != "user" || status.User.Name != "User" || !*status.User.EmailVerified || !status.Ready {
		t.Fatalf("missing public profile: %+v %v", status, err)
	}
	data, _ := json.Marshal(status)
	for _, secret := range []string{"access_token", "refresh_token", "id-secret", "credentials"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("status leaked credentials")
		}
	}
	restarted, err := NewChatGPT(context.Background(), ChatGPTConfig{AppName: "oneb-chat", AppURL: c.origin, Store: c.store})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.hostID != c.hostID || restarted.active == nil || *restarted.active != key {
		t.Fatal("restart lost runtime")
	}
}
func TestHandlerAndOnebRoutes(t *testing.T) {
	c := testClient(t)
	router := New()
	defer router.Close()
	c.Routes(router)
	mux := http.NewServeMux()
	mux.Handle("/rpc", router)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("frontend")) })
	h := c.Handler(mux)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", c.origin+"/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(w, req)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal("bootstrap failed")
	}
	cookie := w.Result().Cookies()[0]
	for _, scenario := range []string{"valid", "no cookie", "wrong origin", "wrong host"} {
		req = httptest.NewRequest("POST", c.origin+"/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"auth/status","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", c.origin)
		if scenario != "no cookie" {
			req.AddCookie(cookie)
		}
		if scenario == "wrong origin" {
			req.Header.Set("Origin", "http://evil.example")
		}
		if scenario == "wrong host" {
			req.Host = "evil.example"
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if scenario == "valid" {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"signed_in":false`) {
				t.Fatalf("bad RPC: %s", w.Body.String())
			}
		} else if w.Code != 403 {
			t.Fatalf("%s accepted", scenario)
		}
	}
}
func TestPermissionAndSaveFailure(t *testing.T) {
	c := testClient(t)
	connect(t, c)
	record, _ := c.store.Load(context.Background(), *c.active)
	record.Credentials.Scopes = []string{"openid"}
	c.store.Save(context.Background(), record)
	if _, err := c.Do(context.Background(), "GET", "/models", nil); !errors.Is(err, ErrPermissionRequired) {
		t.Fatal(err)
	}
	record.Credentials.Scopes = strings.Fields(scopes)
	record.Credentials.ExpiresAt = time.Now().Add(-time.Minute)
	c.store.Save(context.Background(), record)
	c.http = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		return response(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`), nil
	})}
	c.store.(*memoryStore).failSave = true
	if _, err := c.Do(context.Background(), "GET", "/models", nil); err == nil {
		t.Fatal("used rotated token without saving")
	}
}
func TestSignOutRevocationAndRegistrationReuse(t *testing.T) {
	c := testClient(t)
	connect(t, c)
	key := *c.active
	c.http = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			return response(`{"issuer":"https://auth.openai.com","revocation_endpoint":"https://auth.openai.com/revoke"}`), nil
		}
		if r.URL.Path != "/revoke" {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		r.ParseForm()
		if r.Form.Get("token") != "refresh" || r.Form.Get("client_id") != "client" {
			t.Fatal("wrong revocation")
		}
		return response(""), nil
	})}
	result, err := c.SignOut(context.Background())
	if err != nil || result.Warning != "" {
		t.Fatalf("sign-out: %v %+v", err, result)
	}
	record, _ := c.store.Load(context.Background(), key)
	if record.Credentials.AccessToken != "" || record.User.Email == "" {
		t.Fatal("sign-out did not clear tokens and retain profile")
	}
	start, err := c.StartSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.URL)
	if u.Query().Get("client_id") != "client" || u.Query().Has("agent_name_hint") || u.Query().Has("id_token_hint") {
		t.Fatal("registration not reused correctly")
	}
}
func TestDoRestrictsDestinationAndAddsBearer(t *testing.T) {
	c := testClient(t)
	connect(t, c)
	c.http = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer access" {
			t.Fatal("incorrect authenticated request")
		}
		return response("{}"), nil
	})}
	for _, path := range []string{"https://evil.example", "//evil.example/path", "models"} {
		if _, err := c.Do(context.Background(), "GET", path, nil); err == nil {
			t.Fatal("unsafe destination accepted")
		}
	}
	res, err := c.Do(context.Background(), "GET", "/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}
