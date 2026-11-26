package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func getSettings(t *testing.T, f fullServer) map[string]string {
	t.Helper()
	rec := send(t, f.s, http.MethodGet, "/api/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Settings map[string]string `json:"settings"`
	}
	decodeInto(t, rec, &body)
	return body.Settings
}

func TestSettingsAPI(t *testing.T) {
	f := newFullServer(t)
	set := getSettings(t, f)
	if len(set) != len(catalog.SettingKeys()) {
		t.Errorf("want %d settings, got %v", len(catalog.SettingKeys()), set)
	}
	if set["archive_rescan_at"] != "03:00" || set["archive_skip"] != "Output/, CaptureOne/, *.cosessiondb" {
		t.Errorf("defaults: %v", set)
	}
	if !strings.HasSuffix(set["backup_dir"], filepath.Join("Management", "SD2NAS")) {
		t.Errorf("backup_dir %q", set["backup_dir"])
	}

	rec := send(t, f.s, http.MethodPut, "/api/settings", `{"archive_rescan_at":"04:45"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if get := send(t, f.s, http.MethodGet, "/api/settings", ""); rec.Body.String() != get.Body.String() {
		t.Errorf("PUT body %s differs from GET body %s", rec.Body, get.Body)
	}
	var put struct {
		Settings map[string]string `json:"settings"`
	}
	decodeInto(t, rec, &put)
	if put.Settings["archive_rescan_at"] != "04:45" || len(put.Settings) != len(catalog.SettingKeys()) {
		t.Errorf("PUT body %s", rec.Body)
	}
	if got := getSettings(t, f)["archive_rescan_at"]; got != "04:45" {
		t.Errorf("archive_rescan_at %q", got)
	}
	if got := archiveStats(t, f)["rescan_at"]; got != "04:45" {
		t.Errorf("stats rescan_at %v", got)
	}

	rec = send(t, f.s, http.MethodPut, "/api/settings", `{"archive_skip":"Output/, Exports/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put skip: %d %s", rec.Code, rec.Body)
	}
	skip, _ := json.Marshal(archiveStats(t, f)["skip_rules"])
	if string(skip) != `["Output/","Exports/"]` {
		t.Errorf("skip_rules %s", skip)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"archive_rescan_at":"05:00"}`))
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("put without X-SD2NAS: status %d, want 403", rec.Code)
	}
}

func TestSettingsAPIErrors(t *testing.T) {
	f := newFullServer(t)
	inArchive, err := json.Marshal(filepath.Join(f.photos, "Archive", "b"))
	if err != nil {
		t.Fatal(err)
	}
	before := getSettings(t, f)
	for _, c := range []struct{ body, msg string }{
		{`{}`, "Name at least one setting."},
		{`{"nope":"1"}`, "Unknown setting nope."},
		{`{"archive_rescan_at":5}`, "Setting archive_rescan_at must be a string."},
		{`{"archive_rescan_at":"25:00"}`, "Setting archive_rescan_at: "},
		{`{"archive_skip":"a/b"}`, "Setting archive_skip: "},
		{`{"backup_dir":` + string(inArchive) + `}`, "Setting backup_dir: pick a folder outside Archive/ and Staging/."},
		{`{"backup_dir":"relative"}`, "Setting backup_dir: want an absolute path"},
		// One good key and one bad: neither is stored.
		{`{"archive_rescan_at":"06:30","archive_skip":"a/b"}`, "Setting archive_skip: "},
	} {
		e := wantCode(t, send(t, f.s, http.MethodPut, "/api/settings", c.body), http.StatusBadRequest, "")
		if strings.HasSuffix(c.msg, ".") { // a whole sentence
			if e.Error != c.msg {
				t.Errorf("%s: error %q, want %q", c.body, e.Error, c.msg)
			}
		} else if !strings.HasPrefix(e.Error, c.msg) || !strings.HasSuffix(e.Error, ".") {
			t.Errorf("%s: error %q, want it to start with %q and end with a period", c.body, e.Error, c.msg)
		}
	}
	after := getSettings(t, f)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed from %q to %q after rejected PUTs", k, v, after[k])
		}
	}
}

func TestSettingsGetDefaults(t *testing.T) {
	f := newFullServer(t)
	rec := send(t, f.s, http.MethodGet, "/api/settings", "")
	var body struct {
		Defaults map[string]string `json:"defaults"`
	}
	decodeInto(t, rec, &body)
	if len(body.Defaults) != len(catalog.SettingKeys()) {
		t.Errorf("defaults has %d keys: %v", len(body.Defaults), body.Defaults)
	}
	if want := filepath.Join(f.photos, "Management", "SD2NAS"); body.Defaults["backup_dir"] != want {
		t.Errorf("defaults.backup_dir = %q, want %q", body.Defaults["backup_dir"], want)
	}
	if body.Defaults["staging_notice_at"] != "09:00" {
		t.Errorf("defaults.staging_notice_at = %q", body.Defaults["staging_notice_at"])
	}
}

func TestSettingsPutNoticeAt(t *testing.T) {
	f := newFullServer(t)
	rec := send(t, f.s, http.MethodPut, "/api/settings", `{"staging_notice_at":"18:30"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if got := getSettings(t, f)["staging_notice_at"]; got != "18:30" {
		t.Errorf("staging_notice_at = %q", got)
	}
	e := wantCode(t, send(t, f.s, http.MethodPut, "/api/settings", `{"staging_notice_at":"25:00"}`), http.StatusBadRequest, "")
	if want := "Setting staging_notice_at: want HH:MM, 00:00 to 23:59."; e.Error != want {
		t.Errorf("error %q, want %q", e.Error, want)
	}
}

func TestSettingsHooks(t *testing.T) {
	f := newFullServer(t)
	var n atomic.Int32
	f.s.onSettingsChanged(func() { n.Add(1) }) // after MountSettings: hooks are read per request
	if rec := send(t, f.s, http.MethodPut, "/api/settings", `{"staging_notice_at":"10:00"}`); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("hook ran %d times after a 200, want 1", got)
	}
	wantCode(t, send(t, f.s, http.MethodPut, "/api/settings", `{"staging_notice_at":"25:00"}`), http.StatusBadRequest, "")
	if got := n.Load(); got != 1 {
		t.Errorf("hook ran %d times after a 400, want 1", got)
	}
}
