package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	tests := []struct {
		method, header, origin string
		want                   int
	}{
		{"GET", "", "", 200},
		{"HEAD", "", "http://evil.example", 200},
		{"POST", "", "", 403},
		{"POST", "1", "", 200},
		{"POST", "1", "http://example.com", 200},
		{"POST", "1", "https://EXAMPLE.com", 200},
		{"POST", "1", "http://evil.example", 403},
		{"POST", "1", "null", 403},
		{"POST", "true", "", 403},
		{"DELETE", "", "", 403},
		{"OPTIONS", "", "http://evil.example", 403},
	}
	for _, tc := range tests {
		t.Run(tc.method+"/"+tc.header+"/"+tc.origin, func(t *testing.T) {
			called := false
			h := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tc.method, "/x", nil)
			if tc.header != "" {
				req.Header.Set("X-SD2NAS", tc.header)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if called != (tc.want == 200) {
				t.Errorf("inner handler called = %v", called)
			}
			if tc.want == 403 {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
					t.Errorf("403 body %q is not a JSON error: %v", rec.Body, err)
				}
			}
		})
	}
}
