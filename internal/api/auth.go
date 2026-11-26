package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
)

const (
	sessionCookie    = "sd2nas_session"
	sessionMaxAge    = 400 * 24 * 60 * 60 // seconds; browsers cap cookie lifetimes at 400 days
	sessionMessage   = "sd2nas-session-v1"
	tokenTextRefusal = "SD2NAS refused the request: the token is missing or wrong. Set SD2NAS_TOKEN in sd2nas.cfg to the container's token."
)

// auth is the optional shared token (SD2NAS_TOKEN). The zero value means none.
type auth struct {
	digest  []byte // sha256 of the token; nil = no token
	session string // the cookie value: hex HMAC-SHA256(token, sessionMessage)
}

func newAuth(token string) auth {
	if token == "" {
		return auth{}
	}
	sum := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(sessionMessage))
	return auth{digest: sum[:], session: hex.EncodeToString(mac.Sum(nil))}
}

func (a auth) required() bool { return a.digest != nil }

// tokenOK compares digests, so the time taken doesn't depend on the token's
// length or on how much of it was right.
func (a auth) tokenOK(given string) bool {
	sum := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(sum[:], a.digest) == 1
}

// authorized reports whether r carries the token as a Bearer header or the
// session cookie. Always true when no token is set.
func (a auth) authorized(r *http.Request) bool {
	if !a.required() {
		return true
	}
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && a.tokenOK(strings.TrimSpace(tok)) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.session)) == 1
}

// openPath: what answers without the token. The path is judged as the mux
// will route it, after cleaning.
func openPath(r *http.Request) bool {
	p := path.Clean("/" + r.URL.Path)
	switch {
	case p == "/api/auth" && (r.Method == http.MethodGet || r.Method == http.MethodHead),
		(p == "/api/login" || p == "/api/logout") && r.Method == http.MethodPost:
		return true
	case p == "/api" || strings.HasPrefix(p, "/api/"):
		return false
	}
	return true // /healthz and the web UI's files
}

// requireToken refuses every /api/ request but the open ones unless it
// carries the token. It runs before the mux, so no route can be missed.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPath(r) || s.auth.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="SD2NAS"`)
		if wantsText(r) {
			writeFail(w, r, http.StatusUnauthorized, tokenTextRefusal)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "This SD2NAS needs its password.", "code": "token_required"})
	})
}

// SetToken sets the shared token every /api/ request but GET /api/auth,
// POST /api/login and POST /api/logout must carry; "" means none. Call it
// before serving.
func (s *Server) SetToken(token string) { s.auth = newAuth(token) }

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Required   bool `json:"required"`
		Authorized bool `json:"authorized"`
	}{s.auth.required(), s.auth.authorized(r)})
}

// sessionCookieOf is the session cookie; maxAge -1 deletes it (Max-Age=0).
func sessionCookieOf(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode} // no Secure: the LAN is plain HTTP
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.required() {
		writeError(w, http.StatusBadRequest, "SD2NAS has no token set.")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "Enter the password.")
		return
	}
	if !s.auth.tokenOK(body.Token) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That password is not right.", "code": "bad_token"})
		return
	}
	http.SetCookie(w, sessionCookieOf(s.auth.session, sessionMaxAge))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookieOf("", -1))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
