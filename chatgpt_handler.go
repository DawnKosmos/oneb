package oneb

import (
	"context"
	"net/http"
	"strings"
)

type Empty struct{}

// Routes registers typed auth/status, auth/start and auth/logout methods. Their
// public DTOs contain no tokens and can be passed to oneb_gen.Generate.
func (c *ChatGPTClient) Routes(router *Router) {
	auth := router.Group("auth")
	auth.Add("status", func(ctx context.Context, _ Empty) (Status, error) { return c.Status(ctx) }, WithStrictParams())
	auth.Add("start", func(ctx context.Context, _ Empty) (SignIn, error) { return c.StartSignIn(ctx) }, WithStrictParams())
	auth.Add("logout", func(ctx context.Context, _ Empty) (SignOut, error) { return c.SignOut(ctx) }, WithStrictParams())
}

// Handler wraps the local app, owns /auth/callback and protects POST requests
// with an HttpOnly local-browser cookie plus exact origin and host checks.
// GET / bootstraps browser access before sign-in; it is not a signed-in identity.
// Provider requests still require a persisted signed-in connection via Do.
func (c *ChatGPTClient) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != strings.TrimPrefix(c.origin, "http://") {
			http.Error(w, "Forbidden", 403)
			return
		}
		if r.URL.Path == "/auth/callback" {
			if r.Method != "GET" {
				http.Error(w, "Method not allowed", 405)
				return
			}
			c.callback(w, r)
			return
		}
		if r.Method == "POST" {
			cookie, err := r.Cookie(c.cookieName)
			if err != nil || cookie.Value != c.cookie || r.Header.Get("Origin") != c.origin || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "Forbidden", 403)
				return
			}
		}
		if r.Method == "GET" && r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: c.cookieName, Value: c.cookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		next.ServeHTTP(w, r)
	})
}
