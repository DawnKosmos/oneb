package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DawnKosmos/oneb"
	_ "github.com/mattn/go-sqlite3"
)

// sqliteStore handles persistence only; it contains no OAuth or token logic.
type sqliteStore struct{ db *sql.DB }

var _ oneb.Store = (*sqliteStore)(nil)

func openStore(dir string) (*sqliteStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "chat.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &sqliteStore{db: db}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY,account TEXT NOT NULL,role TEXT NOT NULL,content TEXT NOT NULL,model TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS connections (
 issuer TEXT NOT NULL,subject TEXT NOT NULL,client_id TEXT NOT NULL,
 email TEXT NOT NULL,name TEXT NOT NULL,email_verified INTEGER,credentials TEXT NOT NULL,
 PRIMARY KEY(issuer,subject,client_id));`)
	if err == nil {
		err = store.migrateLegacy()
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}
func (s *sqliteStore) Load(ctx context.Context, key oneb.ConnectionKey) (oneb.Connection, error) {
	r := oneb.Connection{Key: key, User: oneb.User{Identity: key.Identity}}
	var credentials string
	var verified sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT email,name,email_verified,credentials FROM connections WHERE issuer=? AND subject=? AND client_id=?`, key.Identity.Issuer, key.Identity.Subject, key.ClientID).Scan(&r.User.Email, &r.User.Name, &verified, &credentials)
	if errors.Is(err, sql.ErrNoRows) {
		return r, oneb.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if verified.Valid {
		r.User.EmailVerified = &verified.Bool
	}
	err = json.Unmarshal([]byte(credentials), &r.Credentials)
	return r, err
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveConnection(ctx context.Context, db executor, r oneb.Connection) error {
	if r.Key.Identity != r.User.Identity || r.Key.Identity.Subject == "" || r.Key.ClientID == "" {
		return errors.New("invalid connection identity")
	}
	b, err := json.Marshal(r.Credentials)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO connections(issuer,subject,client_id,email,name,email_verified,credentials) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(issuer,subject,client_id) DO UPDATE SET email=excluded.email,name=excluded.name,email_verified=excluded.email_verified,credentials=excluded.credentials`, r.Key.Identity.Issuer, r.Key.Identity.Subject, r.Key.ClientID, r.User.Email, r.User.Name, r.User.EmailVerified, string(b))
	return err
}
func (s *sqliteStore) Save(ctx context.Context, r oneb.Connection) error {
	return saveConnection(ctx, s.db, r)
}
func (s *sqliteStore) Delete(ctx context.Context, key oneb.ConnectionKey) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE issuer=? AND subject=? AND client_id=?`, key.Identity.Issuer, key.Identity.Subject, key.ClientID)
	return err
}
func (s *sqliteStore) LoadRuntime(ctx context.Context) (oneb.Runtime, error) {
	var r oneb.Runtime
	var b string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='auth_runtime'`).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return r, oneb.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(b), &r)
	return r, err
}
func saveRuntime(ctx context.Context, db executor, r oneb.Runtime) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings VALUES('auth_runtime',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	return err
}
func (s *sqliteStore) SaveRuntime(ctx context.Context, r oneb.Runtime) error {
	return saveRuntime(ctx, s.db, r)
}

// Preserve the original example's registration, tokens, host and history. All
// conversion is transactional and the duplicate legacy token record is removed.
func (s *sqliteStore) migrateLegacy() error {
	ctx := context.Background()
	if _, err := s.LoadRuntime(ctx); err == nil {
		return nil
	} else if !errors.Is(err, oneb.ErrNotFound) {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var host, raw string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='host_id'`).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	runtime := oneb.Runtime{HostID: host}
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='account'`).Scan(&raw)
	if err == nil {
		var old struct {
			ClientID          string    `json:"client_id"`
			Subject           string    `json:"subject"`
			Email             string    `json:"email"`
			IDToken           string    `json:"id_token"`
			AccessToken       string    `json:"access_token"`
			RefreshToken      string    `json:"refresh_token"`
			Scope             string    `json:"scope"`
			ExpiresAt         time.Time `json:"expires_at"`
			EarliestRefreshAt int64     `json:"earliest_refresh_at"`
		}
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			return err
		}
		if old.ClientID != "" && old.Subject != "" {
			identity := oneb.Identity{Issuer: "https://auth.openai.com", Subject: old.Subject}
			record := oneb.Connection{Key: oneb.ConnectionKey{Identity: identity, ClientID: old.ClientID}, User: oneb.User{Identity: identity, Email: old.Email}, Credentials: oneb.Credentials{AccessToken: old.AccessToken, RefreshToken: old.RefreshToken, IDToken: old.IDToken, TokenType: "Bearer", Scopes: strings.Fields(old.Scope), ExpiresAt: old.ExpiresAt}}
			if old.EarliestRefreshAt > 0 {
				record.Credentials.EarliestRefreshAt = time.Unix(old.EarliestRefreshAt, 0)
			}
			if err = saveConnection(ctx, tx, record); err != nil {
				return err
			}
			runtime.ActiveConnection = &record.Key
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = saveRuntime(ctx, tx, runtime); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM settings WHERE key IN ('account','host_id')`); err != nil {
		return err
	}
	return tx.Commit()
}
