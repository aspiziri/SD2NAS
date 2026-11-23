package api

import (
	"net/http"
	"net/url"
	"strings"
)

// guard refuses every request that could change state unless it carries
// "X-SD2NAS: 1" (a custom header a cross-site form or simple fetch cannot
// send) and, when the browser says where it came from, comes from this host.
// No CORS preflight is ever answered, so OPTIONS is refused too.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-SD2NAS") != "1" {
			writeError(w, http.StatusForbidden, "missing X-SD2NAS: 1 header")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
				writeError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
