package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/DawnKosmos/oneb"
)

func TestTypedStoreProfileAndSeparateRegistrations(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	record := testConnection()
	verified := true
	record.User.Name = "Test User"
	record.User.EmailVerified = &verified
	if err := a.store.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	other := record
	other.Key.ClientID = "workspace-two"
	other.Credentials.AccessToken = "other-access"
	if err := a.store.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	loaded, err := a.store.Load(ctx, record.Key)
	if err != nil || loaded.User.Email != record.User.Email || loaded.User.Name != "Test User" || loaded.User.EmailVerified == nil || !*loaded.User.EmailVerified || loaded.Credentials.AccessToken != "access" {
		t.Fatalf("typed record lost fields: %v", err)
	}
	otherLoaded, err := a.store.Load(ctx, other.Key)
	if err != nil || otherLoaded.Credentials.AccessToken != "other-access" {
		t.Fatal("registration collision")
	}
	loaded.Credentials.RefreshToken = "replacement"
	if err = a.store.Save(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = a.store.Load(ctx, record.Key)
	if err != nil || loaded.Credentials.RefreshToken != "replacement" {
		t.Fatal("credential replacement failed")
	}
	if err = a.store.Delete(ctx, record.Key); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Delete(ctx, record.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.Load(ctx, record.Key); !errors.Is(err, oneb.ErrNotFound) {
		t.Fatal("missing record error")
	}
	if _, err = a.store.Load(ctx, other.Key); err != nil {
		t.Fatal("delete affected another registration")
	}
}
func TestLegacyAccountMigrationPreservesSignInAndHistory(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE messages(id INTEGER PRIMARY KEY,account TEXT NOT NULL,role TEXT NOT NULL,content TEXT NOT NULL,model TEXT NOT NULL);
 INSERT INTO settings VALUES('host_id','urn:uuid:original-host');
 INSERT INTO messages(account,role,content,model) VALUES('legacy-user/legacy-client','user','original conversation','gpt-5.6-luna');`)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]any{"subject": "legacy-user", "client_id": "legacy-client", "email": "legacy@example.com", "access_token": "legacy-access", "refresh_token": "legacy-refresh", "id_token": "legacy-id", "scope": "openid chatgpt.tokens.use.direct", "expires_at": time.Now().Add(time.Hour), "earliest_refresh_at": time.Now().Unix()})
	if _, err = db.Exec(`INSERT INTO settings VALUES('account',?)`, string(legacy)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	a, err := openApp(dir, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	status, err := a.auth.Status(context.Background())
	if err != nil || !status.SignedIn || status.User.Email != "legacy@example.com" {
		t.Fatalf("migration lost sign-in: %+v %v", status, err)
	}
	runtime, _ := a.store.LoadRuntime(context.Background())
	if runtime.HostID != "urn:uuid:original-host" {
		t.Fatal("host changed")
	}
	record, err := a.store.Load(context.Background(), *runtime.ActiveConnection)
	if err != nil || record.Credentials.RefreshToken != "legacy-refresh" {
		t.Fatal("tokens lost")
	}
	history, err := a.history(context.Background(), empty{})
	if err != nil || len(history) != 1 || history[0].Content != "original conversation" {
		t.Fatal("history lost")
	}
	var count int
	a.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key='account'`).Scan(&count)
	if count != 0 {
		t.Fatal("duplicate legacy credentials retained")
	}
}
