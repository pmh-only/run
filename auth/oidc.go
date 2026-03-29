package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"regexp"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"run.pmh.codes/run/config"
	"run.pmh.codes/run/session"
)

var nonUsernameChar = regexp.MustCompile(`[^a-z0-9_]`)

// sanitizeUsername converts an OIDC preferred_username into a valid Linux username.
func sanitizeUsername(raw string) string {
	s := strings.ToLower(raw)
	s = nonUsernameChar.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "user_" + s
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

type Handler struct {
	cfg      *config.Config
	sessions *session.Store
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	oauth2   oauth2.Config
}

func New(ctx context.Context, cfg *config.Config, sess *session.Store) (*Handler, error) {
	provider, err := gooidc.NewProvider(ctx, cfg.OIDCIssuerURL)
	if err != nil {
		return nil, err
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		RedirectURL:  cfg.BaseURL + "/auth/callback",
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{gooidc.ScopeOpenID, "email", "profile"},
	}

	verifier := provider.Verifier(&gooidc.Config{ClientID: cfg.OIDCClientID})

	return &Handler{
		cfg:      cfg,
		sessions: sess,
		provider: provider,
		verifier: verifier,
		oauth2:   oauth2Cfg,
	}, nil
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomString(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sess, err := h.sessions.Get(r)
	if err != nil {
		log.Printf("session get error: %v", err)
	}
	sess.Values[session.KeyOAuthState] = state
	sess.Values[session.KeyOAuthNonce] = nonce
	if err := h.sessions.Save(r, w, sess); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	url := h.oauth2.AuthCodeURL(state, gooidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	sess, err := h.sessions.Get(r)
	if err != nil {
		http.Error(w, "session error", http.StatusBadRequest)
		return
	}

	expectedState := session.GetString(sess, session.KeyOAuthState)
	if r.URL.Query().Get("state") != expectedState || expectedState == "" {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	token, err := h.oauth2.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("oauth2 exchange error: %v", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusInternalServerError)
		return
	}

	idToken, err := h.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		log.Printf("id_token verification error: %v", err)
		http.Error(w, "token verification failed", http.StatusUnauthorized)
		return
	}

	expectedNonce := session.GetString(sess, session.KeyOAuthNonce)
	if idToken.Nonce != expectedNonce || expectedNonce == "" {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}

	var claims struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims error", http.StatusInternalServerError)
		return
	}

	raw := claims.PreferredUsername
	if raw == "" {
		raw = claims.Name
	}
	if raw == "" {
		raw = claims.Email
	}
	username := sanitizeUsername(raw)

	// Clear temporary OAuth state, store user identity
	delete(sess.Values, session.KeyOAuthState)
	delete(sess.Values, session.KeyOAuthNonce)
	sess.Values[session.KeyUserSub] = claims.Sub
	sess.Values[session.KeyUserEmail] = claims.Email
	sess.Values[session.KeyUsername] = username

	if err := h.sessions.Save(r, w, sess); err != nil {
		http.Error(w, "session save error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	sess, err := h.sessions.Get(r)
	if err == nil {
		sess.Options.MaxAge = -1
		_ = h.sessions.Save(r, w, sess)
	}
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}

// RequireAuth is middleware that redirects unauthenticated requests to /auth/login.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := h.sessions.Get(r)
		if err != nil || session.GetString(sess, session.KeyUserSub) == "" {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}


func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
