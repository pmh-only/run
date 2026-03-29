package session

import (
	"crypto/sha256"
	"net/http"

	"github.com/gorilla/sessions"
)

const sessionName = "run_session"

const (
	KeyUserSub    = "user_sub"
	KeyUserEmail  = "user_email"
	KeyUsername   = "username"
	KeyOAuthState = "oauth_state"
	KeyOAuthNonce = "oauth_nonce"
)

type Store struct {
	store *sessions.CookieStore
}

func New(secret string) *Store {
	h := sha256.Sum256([]byte(secret))
	e := sha256.Sum256(h[:])
	s := sessions.NewCookieStore(h[:], e[:])
	s.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	return &Store{store: s}
}

func (s *Store) Get(r *http.Request) (*sessions.Session, error) {
	return s.store.Get(r, sessionName)
}

func (s *Store) Save(r *http.Request, w http.ResponseWriter, sess *sessions.Session) error {
	return s.store.Save(r, w, sess)
}

func GetString(sess *sessions.Session, key string) string {
	v, _ := sess.Values[key].(string)
	return v
}
