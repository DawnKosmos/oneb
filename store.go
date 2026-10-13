// Typed persistence for ChatGPT connections and local installation metadata.
package oneb

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by a Store when the requested record does not exist.
var ErrNotFound = errors.New("chatgpt: record not found")

// Identity identifies a verified OpenAI user. Email is not an identity key.
type Identity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// User contains public claims from a verified OpenID Connect ID token.
// Profile claims can be absent; nil EmailVerified means it was not supplied.
type User struct {
	Identity      Identity `json:"identity"`
	Email         string   `json:"email"`
	EmailVerified *bool    `json:"email_verified,omitempty"`
	Name          string   `json:"name,omitempty"`
}

// Credentials is sensitive server-side data. Never return it to a frontend.
type Credentials struct {
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	IDToken           string    `json:"id_token"`
	TokenType         string    `json:"token_type"`
	Scopes            []string  `json:"scopes"`
	ExpiresAt         time.Time `json:"expires_at"`
	EarliestRefreshAt time.Time `json:"earliest_refresh_at"`
}

// ConnectionKey keeps registrations separate even for the same user or email.
type ConnectionKey struct {
	Identity Identity `json:"identity"`
	ClientID string   `json:"client_id"`
}

// Connection is the complete typed record saved after sign-in and token renewal.
type Connection struct {
	Key         ConnectionKey `json:"key"`
	User        User          `json:"user"`
	Credentials Credentials   `json:"credentials"`
}

// Runtime is installation metadata, not an authentication credential.
// It is retained across sign-out and restart to reuse the host and registration.
type Runtime struct {
	HostID           string         `json:"host_id"`
	ActiveConnection *ConnectionKey `json:"active_connection,omitempty"`
}

// Store is the application's persistence boundary. Load and LoadRuntime return
// ErrNotFound for absent records. Save replaces the entire connection atomically,
// including rotating refresh tokens. Delete is idempotent. SaveRuntime is atomic.
// Implementations must protect credentials and honor context cancellation.
// Use one Client/process per runtime; shared processes need external coordination.
type Store interface {
	// Load retrieves the saved user profile and credentials for a specific connection.
	Load(ctx context.Context, key ConnectionKey) (Connection, error)
	// Save atomically saves or replaces a connection's user profile and credentials.
	Save(ctx context.Context, connection Connection) error
	// Delete removes a connection, succeeding even if it does not exist.
	Delete(ctx context.Context, key ConnectionKey) error
	// LoadRuntime retrieves the installation's stable host ID and selected connection.
	LoadRuntime(ctx context.Context) (Runtime, error)
	// SaveRuntime atomically saves or replaces the installation's host ID and selected connection.
	SaveRuntime(ctx context.Context, runtime Runtime) error
}
