package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const scriptName = "unassigned-devices.sh"

// call is one stubbed host command: its name and arguments.
type call struct {
	name string
	args []string
}

func (c call) has(sub string) bool {
	return slices.ContainsFunc(c.args, func(a string) bool { return strings.Contains(a, sub) })
}

// request is one request the fake SD2NAS received.
type request struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// fake is the fake SD2NAS. ingest and take answer the two endpoints; a nil
// handler answers 200 with an empty body.
type fake struct {
	srv    *httptest.Server
	ingest http.HandlerFunc
	take   http.HandlerFunc

	mu   sync.Mutex
	reqs []request
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		h := f.take
		if r.URL.Path == "/api/ingest" {
			h = f.ingest
		}
		if h == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

// needTools skips (or fails, with SD2NAS_REQUIRE_TOOLS=1) when bash or curl is
// missing.
func needTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the card script runs on Linux")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("SD2NAS_REQUIRE_TOOLS") == "1" {
				t.Fatalf("%s is required: %v", tool, err)
			}
			t.Skipf("%s is not installed", tool)
		}
	}
}

const stubRecord = `{ printf '%s' "$(basename "$0")"; for a in "$@"; do printf '\037%s' "$a"; done; printf '\036'; } >> "$SD2NAS_TEST_CALLS"
`

func writeStubs(t *testing.T, dir string) {
	t.Helper()
	stubs := map[string]string{
		"blkid": stubRecord + `if [ "$*" = "-s UUID -o value /dev/sdz1" ]; then echo 1234-ABCD; exit 0; fi
exit 2
`,
		"rc.unassigned": stubRecord + `exit "${STUB_UMOUNT_EXIT:-0}"
`,
		"logger": stubRecord + "exit 0\n",
		"notify": stubRecord + "exit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// runScript runs the card script against f with stubbed host tools and returns
// the commands it called. An empty value in env removes that variable.
func runScript(t *testing.T, f *fake, env map[string]string) []call {
	t.Helper()
	needTools(t)
	script, err := filepath.Abs(scriptName)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	stubs := filepath.Join(tmp, "stubs")
	if err := os.Mkdir(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStubs(t, stubs)
	callsFile := filepath.Join(tmp, "calls")
	vars := map[string]string{
		"PATH":              stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SD2NAS_CONF":       filepath.Join(tmp, "none.cfg"),
		"SD2NAS_NOTIFY":     filepath.Join(stubs, "notify"),
		"SD2NAS_TEST_CALLS": callsFile,
		"ACTION":            "ADD",
		"DEVICE":            "/dev/sdz1",
		"LABEL":             "SONY_A7IV",
		"SERIAL":            "reader",
		"MOUNTPOINT":        "/mnt/disks/SONY_A7IV",
		"SD2NAS_URL":        f.srv.URL,
	}
	for k, v := range env {
		vars[k] = v
	}
	var environ []string
	for k, v := range vars {
		if v != "" {
			environ = append(environ, k+"="+v)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = environ
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out.String())
	}
	raw, err := os.ReadFile(callsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	for _, rec := range strings.Split(string(raw), "\x1e") {
		if rec == "" {
			continue
		}
		parts := strings.Split(rec, "\x1f")
		calls = append(calls, call{name: parts[0], args: parts[1:]})
	}
	return calls
}

func named(calls []call, name string) []call {
	var out []call
	for _, c := range calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func except(calls []call, name string) []call {
	var out []call
	for _, c := range calls {
		if c.name != name {
			out = append(out, c)
		}
	}
	return out
}

func notifyArgs(msg, level string) []string {
	return []string{"-e", "SD2NAS", "-s", "SD2NAS", "-d", msg, "-i", level}
}

func wantCall(t *testing.T, got call, name string, args []string) {
	t.Helper()
	if got.name != name || !slices.Equal(got.args, args) {
		t.Errorf("call = %s %q, want %s %q", got.name, got.args, name, args)
	}
}

// wantOnlyNotify checks that, logger aside, the script made exactly one call,
// a notify with msg and level.
func wantOnlyNotify(t *testing.T, calls []call, msg, level string) {
	t.Helper()
	rest := except(calls, "logger")
	if len(rest) != 1 {
		t.Fatalf("calls = %q, want one notify", rest)
	}
	wantCall(t, rest[0], "notify", notifyArgs(msg, level))
}

type ingestBody struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	UUID  string `json:"uuid"`
}

func decodeIngest(t *testing.T, body []byte) ingestBody {
	t.Helper()
	var b ingestBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("body is not the expected JSON: %v\n%q", err, body)
	}
	return b
}

func TestScriptSyntax(t *testing.T) {
	needTools(t)
	if out, err := exec.Command("bash", "-n", scriptName).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestScriptImportsAndUnmounts(t *testing.T) {
	f := newFake(t)
	const safe = "SONY_A7IV: 2 new, 0 already known. Card safe to format."
	const stale = "2 folders have been in Staging for 30 days or more: B and A."
	const backup = "Nightly catalog backup failed: x. Older backups were kept."
	f.ingest = reply(200, "normal\n"+safe+"\n")
	f.take = reply(200, "warning\n"+stale+"\nalert\n"+backup+"\n")
	calls := runScript(t, f, nil)

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	in := reqs[0]
	if in.method != "POST" || in.path != "/api/ingest" || in.query != "wait=1&format=text" {
		t.Errorf("ingest request = %s %s?%s", in.method, in.path, in.query)
	}
	if in.header.Get("X-SD2NAS") != "1" || in.header.Get("Content-Type") != "application/json" || in.header.Get("Authorization") != "" {
		t.Errorf("ingest headers = %v", in.header)
	}
	if got, want := decodeIngest(t, in.body), (ingestBody{"/cards/SONY_A7IV", "SONY_A7IV", "1234-ABCD"}); got != want {
		t.Errorf("ingest body = %+v, want %+v", got, want)
	}
	tk := reqs[1]
	if tk.method != "POST" || tk.path != "/api/notifications/take" || tk.query != "format=text" || tk.header.Get("X-SD2NAS") != "1" {
		t.Errorf("take request = %s %s?%s %v", tk.method, tk.path, tk.query, tk.header)
	}

	rest := except(calls, "logger")
	if len(rest) != 5 {
		t.Fatalf("calls = %q, want 5", rest)
	}
	wantCall(t, rest[0], "blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdz1"})
	wantCall(t, rest[1], "rc.unassigned", []string{"umount", "/dev/sdz1"})
	wantCall(t, rest[2], "notify", notifyArgs(safe, "normal"))
	wantCall(t, rest[3], "notify", notifyArgs(stale, "warning"))
	wantCall(t, rest[4], "notify", notifyArgs(backup, "alert"))
}

func TestScriptEscapesLabel(t *testing.T) {
	f := newFake(t)
	f.ingest = reply(200, "normal\nok\n")
	runScript(t, f, map[string]string{
		"LABEL":      "a\"b\\c&d\te\nf\\",
		"MOUNTPOINT": "/mnt/disks/we\"ird&\\",
	})
	reqs := f.requests()
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	got := decodeIngest(t, reqs[0].body)
	want := ingestBody{Path: "/cards/we\"ird&\\", Label: "a\"b\\c&d e f\\", UUID: "1234-ABCD"}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestScriptRefused(t *testing.T) {
	f := newFake(t)
	const msg = "SONY_A7IV: not imported, staging would drop below the 50.0 GB free-space reserve (1.0 GB free). Card left mounted."
	f.ingest = reply(507, "alert\n"+msg+"\n")
	calls := runScript(t, f, nil)
	if len(named(calls, "rc.unassigned")) != 0 {
		t.Errorf("unmounted a refused card: %q", calls)
	}
	notifies := named(calls, "notify")
	if len(notifies) != 1 {
		t.Fatalf("notifies = %q", notifies)
	}
	wantCall(t, notifies[0], "notify", notifyArgs(msg, "alert"))
}

func TestScriptNotSD2NASReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		code string
	}{
		{"404", reply(404, `{"error":"not found"}`), "404"},
		{"hello", reply(200, "hello"), "200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.ingest = tc.h
			calls := runScript(t, f, nil)
			if len(named(calls, "rc.unassigned")) != 0 {
				t.Errorf("unmounted: %q", calls)
			}
			notifies := named(calls, "notify")
			if len(notifies) != 1 {
				t.Fatalf("notifies = %q", notifies)
			}
			wantCall(t, notifies[0], "notify", notifyArgs("SD2NAS could not import SONY_A7IV (HTTP "+tc.code+"). Card left mounted. Check SD2NAS.", "alert"))
		})
	}
}

func TestScriptContainerDown(t *testing.T) {
	f := newFake(t)
	f.srv.Close()
	calls := runScript(t, f, nil)
	if len(named(calls, "rc.unassigned")) != 0 {
		t.Errorf("unmounted: %q", calls)
	}
	notifies := named(calls, "notify")
	if len(notifies) != 1 {
		t.Fatalf("notifies = %q, want exactly one", notifies)
	}
	wantCall(t, notifies[0], "notify", notifyArgs("SD2NAS did not answer, so SONY_A7IV was not imported. Card left mounted. Is the container running?", "alert"))
	if !slices.ContainsFunc(named(calls, "logger"), func(c call) bool { return c.has("could not fetch notices from SD2NAS") }) {
		t.Errorf("no logger call about the notices: %q", calls)
	}
}

func TestScriptTimeout(t *testing.T) {
	f := newFake(t)
	f.ingest = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	calls := runScript(t, f, map[string]string{"SD2NAS_MAX_TIME": "1"})
	if len(named(calls, "rc.unassigned")) != 0 {
		t.Errorf("unmounted: %q", calls)
	}
	notifies := named(calls, "notify")
	if len(notifies) != 1 {
		t.Fatalf("notifies = %q", notifies)
	}
	wantCall(t, notifies[0], "notify", notifyArgs("SD2NAS did not finish importing SONY_A7IV in time. Card left mounted. Check SD2NAS.", "alert"))
}

func TestScriptUnmountFails(t *testing.T) {
	f := newFake(t)
	const msg = "SONY_A7IV: 2 new, 0 already known. Card safe to format."
	f.ingest = reply(200, "normal\n"+msg+"\n")
	calls := runScript(t, f, map[string]string{"STUB_UMOUNT_EXIT": "1"})
	notifies := named(calls, "notify")
	if len(notifies) != 1 {
		t.Fatalf("notifies = %q", notifies)
	}
	wantCall(t, notifies[0], "notify", notifyArgs(msg+" Unmounting failed: unmount it in Unassigned Devices before removing the card.", "warning"))
}

func TestScriptNoMountPoint(t *testing.T) {
	f := newFake(t)
	calls := runScript(t, f, map[string]string{"MOUNTPOINT": ""})
	for _, r := range f.requests() {
		if r.path == "/api/ingest" {
			t.Errorf("ingest requested without a mount point")
		}
	}
	notifies := named(calls, "notify")
	if len(notifies) != 1 {
		t.Fatalf("notifies = %q", notifies)
	}
	wantCall(t, notifies[0], "notify", notifyArgs("Unassigned Devices gave no mount point for /dev/sdz1, so the card was not imported.", "alert"))
}

func TestScriptOtherActions(t *testing.T) {
	for _, action := range []string{"UNMOUNT", "REMOVE", "ERROR_UNMOUNT", ""} {
		t.Run("action="+action, func(t *testing.T) {
			f := newFake(t)
			calls := runScript(t, f, map[string]string{"ACTION": action})
			if len(f.requests()) != 0 || len(calls) != 0 {
				t.Errorf("requests = %d, calls = %q, want none", len(f.requests()), calls)
			}
		})
	}
	t.Run("ERROR_MOUNT", func(t *testing.T) {
		f := newFake(t)
		calls := runScript(t, f, map[string]string{"ACTION": "ERROR_MOUNT"})
		if len(f.requests()) != 0 {
			t.Errorf("requests = %d, want none", len(f.requests()))
		}
		wantOnlyNotify(t, calls, "Unassigned Devices could not mount the card in /dev/sdz1, so it was not imported.", "alert")
	})
}

func TestScriptNotifyAction(t *testing.T) {
	onlyTake := func(t *testing.T, f *fake) {
		t.Helper()
		reqs := f.requests()
		if len(reqs) != 1 || reqs[0].path != "/api/notifications/take" {
			t.Fatalf("requests = %+v, want only the take", reqs)
		}
	}
	t.Run("empty", func(t *testing.T) {
		f := newFake(t)
		f.take = reply(200, "")
		calls := runScript(t, f, map[string]string{"ACTION": "NOTIFY"})
		onlyTake(t, f)
		if len(named(calls, "notify")) != 0 {
			t.Errorf("notify called: %q", calls)
		}
	})
	t.Run("unknown level", func(t *testing.T) {
		f := newFake(t)
		f.take = reply(200, "loud\nx\n")
		calls := runScript(t, f, map[string]string{"ACTION": "NOTIFY"})
		onlyTake(t, f)
		if len(named(calls, "notify")) != 0 {
			t.Errorf("notify called: %q", calls)
		}
		if !slices.ContainsFunc(named(calls, "logger"), func(c call) bool { return c.has("ignored a notice") }) {
			t.Errorf("no logger call about the ignored notice: %q", calls)
		}
	})
	t.Run("503", func(t *testing.T) {
		f := newFake(t)
		f.take = reply(503, "alert\nSD2NAS is shutting down\n")
		calls := runScript(t, f, map[string]string{"ACTION": "NOTIFY"})
		onlyTake(t, f)
		if len(calls) != 1 || calls[0].name != "logger" || !calls[0].has("HTTP 503") {
			t.Errorf("calls = %q, want one logger call with HTTP 503", calls)
		}
	})
}

func TestScriptConfigFileAndToken(t *testing.T) {
	f := newFake(t)
	f.ingest = reply(200, "normal\nok\n")
	conf := filepath.Join(t.TempDir(), "sd2nas.cfg")
	if err := os.WriteFile(conf, []byte("SD2NAS_URL="+f.srv.URL+"\nSD2NAS_TOKEN=abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runScript(t, f, map[string]string{"SD2NAS_CONF": conf, "SD2NAS_URL": ""})
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for _, r := range reqs {
		if got := r.header.Get("Authorization"); got != "Bearer abc" {
			t.Errorf("%s Authorization = %q, want Bearer abc", r.path, got)
		}
	}
}

func TestScriptCRLFConfig(t *testing.T) {
	f := newFake(t)
	f.ingest = reply(200, "normal\nok\n")
	cfg := filepath.Join(t.TempDir(), "sd2nas.cfg")
	if err := os.WriteFile(cfg, []byte("SD2NAS_URL="+f.srv.URL+"\r\nSD2NAS_TOKEN=secret\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An empty SD2NAS_URL removes the variable, so only the cfg supplies it.
	calls := runScript(t, f, map[string]string{"SD2NAS_CONF": cfg, "SD2NAS_URL": ""})
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (the cfg's CR must not break the URL)", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret")
	}
	if len(named(calls, "rc.unassigned")) != 1 {
		t.Errorf("calls = %q, want the card unmounted after the import", calls)
	}
}
