package main

import (
	"context"
	"encoding/json"
	"github.com/DawnKosmos/oneb"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(s string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(s))}
}
func testApp(t *testing.T) *app {
	t.Helper()
	a, err := openApp(t.TempDir(), "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	return a
}
func testConnection() oneb.Connection {
	identity := oneb.Identity{Issuer: "https://auth.openai.com", Subject: "user"}
	return oneb.Connection{Key: oneb.ConnectionKey{Identity: identity, ClientID: "client"}, User: oneb.User{Identity: identity, Email: "user@example.com"}, Credentials: oneb.Credentials{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer", Scopes: []string{"chatgpt.tokens.use.direct"}, ExpiresAt: time.Now().Add(time.Hour)}}
}
func connect(t *testing.T, a *app) {
	t.Helper()
	r := testConnection()
	if err := a.store.Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := a.auth.Select(context.Background(), r.Key); err != nil {
		t.Fatal(err)
	}
}
func TestChatModelSwitchAndFailedTurn(t *testing.T) {
	a := testApp(t)
	connect(t, a)
	call := 0
	a.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Fatal("missing bearer credential")
		}
		var body struct {
			Model     string              `json:"model"`
			Store     bool                `json:"store"`
			Stream    bool                `json:"stream"`
			Input     []map[string]string `json:"input"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		call++
		if body.Reasoning.Effort != "high" {
			t.Fatal("thinking selection ignored")
		}
		if body.Store || !body.Stream || len(body.Input) != 2*call-1 {
			t.Fatalf("invalid Responses contract: %+v", body)
		}
		if call == 1 && body.Model != "gpt-5.6-luna" || call > 1 && body.Model != "gpt-6.1-sol" {
			t.Fatal("model selection ignored")
		}
		if call == 3 {
			return response("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n"), nil
		}
		return response("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"), nil
	})
	for _, m := range []string{"gpt-5.6-luna", "gpt-6.1-sol"} {
		reply, err := a.send(context.Background(), sendInput{Message: "Hi", Model: m, ReasoningEffort: "high"})
		if err != nil || reply.Content != "Hello" {
			t.Fatalf("chat failed: %v", err)
		}
	}
	if _, err := a.send(context.Background(), sendInput{Message: "Failure", Model: "gpt-6.1-sol", ReasoningEffort: "high"}); err == nil {
		t.Fatal("failed response accepted")
	}
	history, err := a.history(context.Background(), empty{})
	if err != nil || len(history) != 4 {
		t.Fatalf("failed turn changed history: %v %+v", err, history)
	}
	record := testConnection()
	record.Key.Identity.Subject = "other"
	record.User.Identity = record.Key.Identity
	a.store.Save(context.Background(), record)
	a.auth.Select(context.Background(), record.Key)
	other, _ := a.history(context.Background(), empty{})
	if len(other) != 0 {
		t.Fatal("account history leaked")
	}
}

func TestStreamTerminalEvents(t *testing.T) {
	for _, s := range []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "data: [DONE]\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", "data: not-json\n\n", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n"} {
		if _, err := readReply(strings.NewReader(s)); err == nil {
			t.Fatalf("invalid stream accepted: %s", s)
		}
	}
	reply, err := readReply(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"refusal\",\"refusal\":\"Cannot help\"}]}]}}\n\n"))
	if err != nil || reply != "Cannot help" {
		t.Fatalf("refusal handling: %q %v", reply, err)
	}
}

func TestModelCatalog(t *testing.T) {
	a := testApp(t)
	connect(t, a)
	a.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/models" {
			t.Fatal(r.URL.Path)
		}
		return response(`{"models":[{"slug":"gpt-5.6-luna","display_name":"GPT 5.6 Luna","visibility":"list"},{"slug":"other","visibility":"list"},{"slug":"hidden","visibility":"hide"},{"slug":"gpt-6.1-sol","display_name":"GPT 6.1 Sol","visibility":"list"}]}`), nil
	})
	models, err := a.models(context.Background(), empty{})
	if err != nil || len(models) != 2 || models[1].Slug != "gpt-6.1-sol" || len(models[0].ReasoningEfforts) != 6 || len(models[1].ReasoningEfforts) != 5 || !models[0].CatalogListed || !models[1].CatalogListed {
		t.Fatalf("wrong catalog: %v %+v", err, models)
	}
}

func TestSQLiteRestart(t *testing.T) {
	dir := t.TempDir()
	a, err := openApp(dir, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	connect(t, a)
	runtime, _ := a.store.LoadRuntime(context.Background())
	host := runtime.HostID
	_, err = a.db.Exec(`INSERT INTO messages(account,role,content,model) VALUES(?,?,?,?)`, accountKey(testConnection().Key), "user", "saved", "a")
	if err != nil {
		t.Fatal(err)
	}
	a.close()
	b, err := openApp(dir, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	history, err := b.history(context.Background(), empty{})
	restored, _ := b.store.LoadRuntime(context.Background())
	record, _ := b.store.Load(context.Background(), *restored.ActiveConnection)
	if err != nil || len(history) != 1 || restored.HostID != host || record.Credentials.RefreshToken != "refresh" {
		t.Fatalf("restart lost state: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "chat.sqlite"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("database is not private")
	}
}

func TestThinkingSettings(t *testing.T) {
	a := testApp(t)
	connect(t, a)
	calls := 0
	a.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		expected := "medium"
		if calls == 2 {
			expected = "none"
		}
		if body.Reasoning.Effort != expected {
			t.Fatalf("thinking effort: got %s, want %s", body.Reasoning.Effort, expected)
		}
		return response("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"), nil
	})
	for _, in := range []sendInput{
		{Message: "Hi", Model: "gpt-6.1-sol"},
		{Message: "Hi", Model: "gpt-5.6-luna", ReasoningEffort: "none"},
	} {
		if _, err := a.send(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []sendInput{
		{Message: "Hi", Model: "other", ReasoningEffort: "medium"},
		{Message: "Hi", Model: "gpt-6.1-sol", ReasoningEffort: "none"},
		{Message: "Hi", Model: "gpt-5.6-luna", ReasoningEffort: "invalid"},
	} {
		if _, err := a.send(context.Background(), in); err == nil {
			t.Fatalf("invalid setting accepted: %+v", in)
		}
	}
	if calls != 2 {
		t.Fatal("invalid selection reached OpenAI")
	}
}

func TestRequestedModelMissingFromAccountCatalog(t *testing.T) {
	a := testApp(t)
	connect(t, a)
	a.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return response(`{"models":[{"slug":"gpt-5.6-luna","visibility":"list"},{"slug":"gpt-6-astra","visibility":"list"}]}`), nil
	})
	models, err := a.models(context.Background(), empty{})
	if err != nil || len(models) != 2 {
		t.Fatalf("missing requested choices: %+v %v", models, err)
	}
	if models[1].Slug != "gpt-6.1-sol" || models[1].CatalogListed || !models[0].CatalogListed {
		t.Fatalf("incorrect catalog status: %+v", models)
	}
}

// Test setup uses the same typed store and injected library client as main.
func openApp(dir, origin string) (*app, error) {
	store, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	auth, err := oneb.NewChatGPT(context.Background(), oneb.ChatGPTConfig{AppName: "oneb-chat", AppURL: origin, Store: store, HTTPClient: client})
	if err != nil {
		store.db.Close()
		return nil, err
	}
	return &app{db: store.db, auth: auth, http: client, store: store}, nil
}
