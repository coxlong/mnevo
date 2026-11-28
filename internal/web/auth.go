package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "mnevo_session"
	sessionTTL    = 12 * time.Hour
)

type webSession struct {
	CSRF      string
	Flash     string
	ExpiresAt time.Time
}

type loginFailures struct {
	Count   int
	ResetAt time.Time
}

type sessionStore struct {
	adminKey  string
	loginCSRF string

	mu       sync.Mutex
	sessions map[string]webSession
	failures map[string]loginFailures
}

func newSessionStore(adminKey, origin string) *sessionStore {
	mac := hmac.New(sha256.New, []byte(adminKey))
	mac.Write([]byte("login:" + origin))
	return &sessionStore{
		adminKey:  adminKey,
		loginCSRF: hex.EncodeToString(mac.Sum(nil)),
		sessions:  make(map[string]webSession),
		failures:  make(map[string]loginFailures),
	}
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (s *sessionStore) get(r *http.Request) (string, webSession, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return "", webSession{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[cookie.Value]
	if !ok || now.After(session.ExpiresAt) {
		delete(s.sessions, cookie.Value)
		return "", webSession{}, false
	}
	return cookie.Value, session, true
}

func (s *sessionStore) create() (string, webSession, error) {
	id, err := randomToken()
	if err != nil {
		return "", webSession{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", webSession{}, err
	}
	session := webSession{CSRF: csrf, ExpiresAt: time.Now().Add(sessionTTL)}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, existing := range s.sessions {
		if now.After(existing.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	s.sessions[id] = session
	return id, session, nil
}

func (s *sessionStore) destroy(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// setFlash stores a one-shot status message on the session (read-once via popFlash).
func (s *sessionStore) setFlash(id, message string) {
	s.mu.Lock()
	if session, ok := s.sessions[id]; ok {
		session.Flash = message
		s.sessions[id] = session
	}
	s.mu.Unlock()
}

// popFlash returns and clears the pending flash message for the session.
func (s *sessionStore) popFlash(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || session.Flash == "" {
		return ""
	}
	flash := session.Flash
	session.Flash = ""
	s.sessions[id] = session
	return flash
}

func (s *sessionStore) loginAllowed(remote string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, failure := range s.failures {
		if now.After(failure.ResetAt) {
			delete(s.failures, ip)
		}
	}
	failure := s.failures[remote]
	return failure.Count < 10 || now.After(failure.ResetAt)
}

func (s *sessionStore) loginFailed(remote string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	failure := s.failures[remote]
	if now.After(failure.ResetAt) {
		failure = loginFailures{ResetAt: now.Add(5 * time.Minute)}
	}
	failure.Count++
	s.failures[remote] = failure
}

func (s *sessionStore) loginSucceeded(remote string) {
	s.mu.Lock()
	delete(s.failures, remote)
	s.mu.Unlock()
}

func setSessionCookie(w http.ResponseWriter, id string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func secureRequest(r *http.Request, publicOrigin string) bool {
	return r.TLS != nil || strings.HasPrefix(strings.ToLower(publicOrigin), "https://")
}

func safeNext(raw, base, publicPath string) string {
	if raw == "" {
		return base
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return base
	}
	route, ok := webRoute(u.Path, publicPath)
	if !ok || route != "root" {
		return base
	}
	return u.RequestURI()
}

func handleLogin(w http.ResponseWriter, r *http.Request, sessions *sessionStore, base, publicOrigin, publicPath string) {
	if r.Method == http.MethodGet {
		if _, _, ok := sessions.get(r); ok {
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next"), base, publicPath), http.StatusSeeOther)
			return
		}
		renderLogin(w, r, base, r.URL.Query().Get("next"), sessions.loginCSRF, "", http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if !allowedOrigin(r, publicOrigin) || r.ParseForm() != nil ||
		subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sessions.loginCSRF)) != 1 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	next := r.PostForm.Get("next")
	if !sessions.loginAllowed(r.RemoteAddr) {
		renderLogin(w, r, base, next, sessions.loginCSRF, "登录尝试过于频繁，请 5 分钟后再试。", http.StatusTooManyRequests)
		return
	}
	username, password := r.PostForm.Get("username"), r.PostForm.Get("password")
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte("mnevo")) == 1
	keyOK := subtle.ConstantTimeCompare([]byte(password), []byte(sessions.adminKey)) == 1
	if !userOK || !keyOK {
		sessions.loginFailed(r.RemoteAddr)
		renderLogin(w, r, base, next, sessions.loginCSRF, "用户名或管理密钥不正确，请重试。", http.StatusUnauthorized)
		return
	}
	id, _, err := sessions.create()
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	sessions.loginSucceeded(r.RemoteAddr)
	setSessionCookie(w, id, secureRequest(r, publicOrigin))
	http.Redirect(w, r, safeNext(next, base, publicPath), http.StatusSeeOther)
}

func renderLogin(w http.ResponseWriter, r *http.Request, base, next, csrf, message string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginPage(base, next, csrf, message).Render(r.Context(), w)
}

func handleLogout(w http.ResponseWriter, r *http.Request, sessions *sessionStore, id string, session webSession, base, publicOrigin string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if !allowedOrigin(r, publicOrigin) || r.ParseForm() != nil ||
		subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(session.CSRF)) != 1 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	sessions.destroy(id)
	clearSessionCookie(w)
	http.Redirect(w, r, base+"login", http.StatusSeeOther)
}
