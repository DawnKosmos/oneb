// A single-user local ChatGPT chat example. OAuth is owned by the library.
package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/DawnKosmos/oneb"
)

//go:embed web/index.html
var web embed.FS

type empty struct{}
type app struct {
	db    *sql.DB
	auth  *oneb.ChatGPTClient
	http  *http.Client
	store *sqliteStore
	turn  sync.Mutex
}

func (a *app) close() {
	a.auth.Close()
	a.db.Close()
}
func (a *app) routes(router *oneb.Router) {
	router.Add("models/list", a.models, oneb.WithStrictParams())
	router.Add("chat/history", a.history, oneb.WithStrictParams())
	router.Add("chat/send", a.send, oneb.WithStrictParams())
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Model   string `json:"model"`
}
type model struct {
	Slug             string   `json:"slug"`
	DisplayName      string   `json:"display_name"`
	Visibility       string   `json:"visibility"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
	CatalogListed    bool     `json:"catalog_listed"`
}

// Keep model selection and request validation on the same small allowlist.
func reasoningEfforts(slug string) []string {
	switch slug {
	case "gpt-5.6-luna":
		return []string{"none", "low", "medium", "high", "xhigh", "max"}
	case "gpt-6.1-sol":
		return []string{"low", "medium", "high", "xhigh", "max"}
	default:
		return nil
	}
}

func (a *app) models(ctx context.Context, _ empty) ([]model, error) {
	res, err := a.auth.Do(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var catalog struct {
		Models []model `json:"models"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&catalog); err != nil {
		return nil, err
	}
	// The requested choices remain visible even if the account catalog lags or
	// excludes one. Catalog presence is displayed separately from inference access.
	result := []model{
		{Slug: "gpt-5.6-luna", DisplayName: "GPT 5.6 Luna", ReasoningEfforts: reasoningEfforts("gpt-5.6-luna")},
		{Slug: "gpt-6.1-sol", DisplayName: "GPT 6.1 Sol", ReasoningEfforts: reasoningEfforts("gpt-6.1-sol")},
	}
	for i := range result {
		for _, m := range catalog.Models {
			if m.Slug == result[i].Slug && m.Visibility == "list" {
				result[i].CatalogListed = true
				break
			}
		}
	}
	return result, nil
}
func accountKey(key oneb.ConnectionKey) string { return key.Identity.Subject + "/" + key.ClientID }
func (a *app) loadHistory(ctx context.Context, key string) ([]message, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT role,content,model FROM messages WHERE account=? ORDER BY id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]message, 0)
	for rows.Next() {
		var m message
		if err = rows.Scan(&m.Role, &m.Content, &m.Model); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (a *app) history(ctx context.Context, _ empty) ([]message, error) {
	key, _, err := a.auth.Selected(ctx)
	if errors.Is(err, oneb.ErrSignInRequired) {
		return []message{}, nil
	}
	if err != nil {
		return nil, err
	}
	return a.loadHistory(ctx, accountKey(key))
}

type sendInput struct {
	Message         string `json:"message"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func (a *app) send(ctx context.Context, in sendInput) (message, error) {
	if !a.turn.TryLock() {
		return message{}, errors.New("A reply is already in progress")
	}
	defer a.turn.Unlock()
	if strings.TrimSpace(in.Message) == "" || len(in.Message) > 32000 || in.Model == "" {
		return message{}, errors.New("Choose a model and enter a message (up to 32,000 bytes)")
	}
	efforts := reasoningEfforts(in.Model)
	if efforts == nil {
		return message{}, errors.New("Choose GPT 5.6 Luna or GPT 6.1 Sol")
	}
	if in.ReasoningEffort == "" {
		in.ReasoningEffort = "medium"
	}
	validEffort := false
	for _, effort := range efforts {
		if effort == in.ReasoningEffort {
			validEffort = true
			break
		}
	}
	if !validEffort {
		return message{}, errors.New("Choose a supported thinking mode for this model")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	key, _, err := a.auth.Selected(ctx)
	if err != nil {
		return message{}, err
	}
	history, err := a.loadHistory(ctx, accountKey(key))
	if err != nil {
		return message{}, err
	}
	input := make([]map[string]string, 0, len(history)+1)
	size := len(in.Message)
	for _, m := range history {
		size += len(m.Content)
		input = append(input, map[string]string{"role": m.Role, "content": m.Content})
	}
	if size > 250000 {
		return message{}, errors.New("This conversation is too long for this MVP. Use a new data directory to start another")
	}
	input = append(input, map[string]string{"role": "user", "content": in.Message})
	body, err := json.Marshal(map[string]any{"model": in.Model, "input": input, "store": false, "stream": true, "reasoning": map[string]string{"effort": in.ReasoningEffort}})
	if err != nil {
		return message{}, err
	}
	res, err := a.auth.Do(ctx, "POST", "/responses", strings.NewReader(string(body)))
	if err != nil {
		return message{}, err
	}
	defer res.Body.Close()
	text, err := readReply(res.Body)
	if err != nil {
		return message{}, err
	}
	reply := message{Role: "assistant", Content: text, Model: in.Model}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return message{}, err
	}
	defer tx.Rollback()
	for _, m := range []message{{Role: "user", Content: in.Message, Model: in.Model}, reply} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(account,role,content,model) VALUES(?,?,?,?)`, accountKey(key), m.Role, m.Content, m.Model); err != nil {
			return message{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return message{}, err
	}
	return reply, nil
}

// frontend serves the embedded UI; RPC and authentication are wired in main.
func frontend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := web.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Write(data)
	})
}
func main() {
	defaultDir, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := flag.String("data", filepath.Join(defaultDir, "oneb-chat"), "private SQLite data directory")
	port := flag.Int("port", 8080, "loopback port (0 chooses an available port)")
	flag.Parse()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()

	store, err := openStore(*dir)
	if err != nil {
		listener.Close()
		log.Fatal(err)
	}
	auth, err := oneb.NewChatGPT(context.Background(), oneb.ChatGPTConfig{
		AppName: "oneb-chat",
		AppURL:  origin,
		Store:   store,
	})
	if err != nil {
		store.db.Close()
		listener.Close()
		log.Fatal(err)
	}
	a := &app{db: store.db, store: store, auth: auth}
	defer a.close()

	router := oneb.New()
	defer router.Close()
	auth.Routes(router)
	a.routes(router)

	mux := http.NewServeMux()
	mux.Handle("POST /rpc", http.MaxBytesHandler(router, 64<<10))
	mux.Handle("GET /", frontend())
	server := &http.Server{
		Handler:           auth.Handler(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("Open %s", origin)
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
