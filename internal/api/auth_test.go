package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef"

// sessionFor is the cookie value for token, computed independently of auth.go.
func sessionFor(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("sd2nas-session-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// authServer is a full server with the UI mounted and, unless token is
// empty, the token set.
func authServer(t *testing.T, token string) fullServer {
	t.Helper()
	f := newFullServer(t)
	if token != "" {
		f.s.SetToken(token)
	}
	if err := f.s.MountUI(builtUI()); err != nil {
		t.Fatal(err)
	}
	return f
}

// do sends one request; header pairs are name, value.
func do(t *testing.T, s *Server, method, url, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// wantTokenRequired checks the binding JSON refusal.
func wantTokenRequired(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	e := wantCode(t, rec, http.StatusUnauthorized, "token_required")
	if e.Error != "This SD2NAS needs its password." {
		t.Errorf("error %q", e.Error)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="SD2NAS"` {
		t.Errorf("WWW-Authenticate %q", got)
	}
}

func wantAuth(t *testing.T, s *Server, want string, header ...string) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/auth", "", header...)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("GET /api/auth: %d %s, want 200 %s", rec.Code, rec.Body, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET /api/auth Cache-Control %q", cc)
	}
}

func TestNoTokenUnchanged(t *testing.T) {
	f := authServer(t, "")
	if rec := do(t, f.s, http.MethodGet, "/api/staging", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /api/staging: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.s, http.MethodPost, "/api/staging/rescan", "", "X-SD2NAS", "1"); rec.Code != http.StatusAccepted {
		t.Errorf("POST /api/staging/rescan: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.s, http.MethodPost, "/api/staging/rescan", ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/staging/rescan without X-SD2NAS: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.s, http.MethodGet, "/api/settings", "", "Authorization", "Bearer whatever"); rec.Code != http.StatusOK {
		t.Errorf("GET /api/settings with a stray Bearer: %d %s", rec.Code, rec.Body)
	}
	wantAuth(t, f.s, `{"required":false,"authorized":true}`)
	e := wantCode(t, do(t, f.s, http.MethodPost, "/api/login", `{"token":"x"}`, "X-SD2NAS", "1"), http.StatusBadRequest, "")
	if e.Error != "SD2NAS has no token set." {
		t.Errorf("login error %q", e.Error)
	}
}

func TestTokenRequired(t *testing.T) {
	f := authServer(t, testToken)
	sha := strings.Repeat("ab", 32)
	for _, c := range []struct{ method, url string }{
		{http.MethodGet, "/api/settings"},
		{http.MethodGet, "/api/thumbs/" + sha},
		{http.MethodGet, "/api/events"},
		{http.MethodHead, "/api/events"},
		{http.MethodGet, "/api/nope"},
		{http.MethodGet, "/api"},
		{http.MethodPost, "/api/staging/rescan"},
		{http.MethodPost, "/api/auth"},
	} {
		t.Run(c.method+" "+c.url, func(t *testing.T) {
			rec := do(t, f.s, c.method, c.url, "", "X-SD2NAS", "1")
			if c.method == http.MethodHead {
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status %d", rec.Code)
				}
				return
			}
			wantTokenRequired(t, rec)
		})
	}
	for _, p := range []string{"/healthz", "/", "/staging/3"} {
		if rec := do(t, f.s, http.MethodGet, p, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	wantAuth(t, f.s, `{"required":true,"authorized":false}`)
	wantAuth(t, f.s, `{"required":true,"authorized":true}`, "Authorization", "Bearer "+testToken)
}

func TestTokenBearer(t *testing.T) {
	f := authServer(t, testToken)
	for _, c := range []struct {
		auth string
		want int
	}{
		{"Bearer " + testToken, 200},
		{"bearer " + testToken, 200},
		{"BEARER  " + testToken + " ", 200},
		{"Bearer wrong", 401},
		{"Bearer " + testToken + "x", 401},
		{"Basic " + testToken, 401},
		{"Bearer", 401},
		{"Bearer ", 401},
		{testToken, 401},
	} {
		rec := do(t, f.s, http.MethodGet, "/api/settings", "", "Authorization", c.auth)
		if rec.Code != c.want {
			t.Errorf("Authorization %q: status %d, want %d", c.auth, rec.Code, c.want)
		}
	}
	rec := do(t, f.s, http.MethodPost, "/api/staging/rescan", "", "X-SD2NAS", "1", "Authorization", "Bearer "+testToken)
	if rec.Code != http.StatusAccepted {
		t.Errorf("POST with the Bearer: %d %s", rec.Code, rec.Body)
	}
}

func TestTokenTextRefusal(t *testing.T) {
	f := authServer(t, testToken)
	for _, auth := range []string{"Bearer wrong", ""} {
		rec := do(t, f.s, http.MethodPost, "/api/ingest?wait=1&format=text", cardBody(f.card), "X-SD2NAS", "1", "Authorization", auth)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status %d", auth, rec.Code)
		}
		const want = "alert\nSD2NAS refused the request: the token is missing or wrong. Set SD2NAS_TOKEN in sd2nas.cfg to the container's token.\n"
		if rec.Body.String() != want {
			t.Errorf("%q: body %q, want %q", auth, rec.Body, want)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="SD2NAS"` {
			t.Errorf("WWW-Authenticate %q", got)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q", cc)
		}
	}
	// The card script's other request.
	rec := do(t, f.s, http.MethodPost, "/api/notifications/take?format=text", "", "X-SD2NAS", "1")
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Body.String(), "alert\nSD2NAS refused the request") {
		t.Errorf("notifications/take: %d %q", rec.Code, rec.Body)
	}
}

func TestGuardBeforeToken(t *testing.T) {
	f := authServer(t, testToken)
	rec := do(t, f.s, http.MethodPost, "/api/staging/rescan", "", "Authorization", "Bearer "+testToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	rec = do(t, f.s, http.MethodPost, "/api/staging/rescan", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("no credentials, no X-SD2NAS: status %d, want 403", rec.Code)
	}
}

func TestLogin(t *testing.T) {
	f := authServer(t, testToken)
	rec := do(t, f.s, http.MethodPost, "/api/login", `{"token":"`+testToken+`"}`, "X-SD2NAS", "1")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body %q", rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	session := sessionFor(testToken)
	wantCookie := "sd2nas_session=" + session + "; Path=/; Max-Age=34560000; HttpOnly; SameSite=Strict"
	if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != wantCookie {
		t.Fatalf("Set-Cookie %q, want %q", got, wantCookie)
	}
	cookie := "sd2nas_session=" + session
	if rec := do(t, f.s, http.MethodGet, "/api/settings", "", "Cookie", cookie); rec.Code != http.StatusOK {
		t.Errorf("GET /api/settings with the cookie: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.s, http.MethodPost, "/api/staging/rescan", "", "X-SD2NAS", "1", "Cookie", cookie); rec.Code != http.StatusAccepted {
		t.Errorf("POST with the cookie: %d %s", rec.Code, rec.Body)
	}
	wantAuth(t, f.s, `{"required":true,"authorized":true}`, "Cookie", cookie)

	e := wantCode(t, do(t, f.s, http.MethodPost, "/api/login", `{"token":"wrong"}`, "X-SD2NAS", "1"), http.StatusUnauthorized, "bad_token")
	if e.Error != "That password is not right." {
		t.Errorf("wrong token: error %q", e.Error)
	}
	for _, body := range []string{`{}`, `{"token":""}`} {
		e := wantCode(t, do(t, f.s, http.MethodPost, "/api/login", body, "X-SD2NAS", "1"), http.StatusBadRequest, "")
		if e.Error != "Enter the password." {
			t.Errorf("%s: error %q", body, e.Error)
		}
	}
	if rec := do(t, f.s, http.MethodPost, "/api/login", `{"token":"`+testToken+`"}`); rec.Code != http.StatusForbidden {
		t.Errorf("without X-SD2NAS: status %d, want 403", rec.Code)
	}
	e = wantCode(t, do(t, f.s, http.MethodPost, "/api/login", `{"token":"`+testToken+`","x":1}`, "X-SD2NAS", "1"), http.StatusBadRequest, "")
	if e.Error != "The request has a field this server does not know." {
		t.Errorf("unknown field: error %q", e.Error)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		do(t, f.s, http.MethodPost, "/api/login", `{"token":"wrong"}`, "X-SD2NAS", "1"),
		do(t, f.s, http.MethodPost, "/api/login", `{}`, "X-SD2NAS", "1"),
	} {
		if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Errorf("a failed login set a cookie: %q", got)
		}
	}
}

func TestLogout(t *testing.T) {
	for _, token := range []string{testToken, ""} {
		f := authServer(t, token)
		rec := do(t, f.s, http.MethodPost, "/api/logout", "", "X-SD2NAS", "1")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("token %q: status %d, want 204", token, rec.Code)
		}
		const want = "sd2nas_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict"
		if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != want || !strings.Contains(got[0], "Max-Age=0") {
			t.Errorf("token %q: Set-Cookie %q, want %q", token, got, want)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q", cc)
		}
		if rec := do(t, f.s, http.MethodPost, "/api/logout", ""); rec.Code != http.StatusForbidden {
			t.Errorf("without X-SD2NAS: status %d, want 403", rec.Code)
		}
	}
}

func TestCookieOfOtherToken(t *testing.T) {
	f := authServer(t, testToken)
	for _, v := range []string{sessionFor("another-token-123"), "", "x", strings.ToUpper(sessionFor(testToken))} {
		wantTokenRequired(t, do(t, f.s, http.MethodGet, "/api/settings", "", "Cookie", "sd2nas_session="+v))
	}
}

func TestTokenCleanedPath(t *testing.T) {
	f := authServer(t, testToken)
	for _, p := range []string{"/x/../api/settings", "/api/./settings", "/api/auth/../settings"} {
		t.Run(p, func(t *testing.T) {
			wantTokenRequired(t, do(t, f.s, http.MethodGet, p, ""))
		})
	}
}

func TestHeadEvents(t *testing.T) {
	s := New("test", nil)
	t.Cleanup(s.Close)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/events", nil))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		s.Close() // ends the stream so the handler goroutine finishes
		<-done
		t.Fatal("HEAD /api/events did not return within 1 s")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	wantHeader(t, rec, "Content-Type", "text/event-stream")
	wantHeader(t, rec, "Cache-Control", "no-store")
	if rec.Body.Len() != 0 {
		t.Errorf("body %q, want empty", rec.Body)
	}
	if n := s.hub.Len(); n != 0 {
		t.Errorf("%d subscribers left", n)
	}
}
