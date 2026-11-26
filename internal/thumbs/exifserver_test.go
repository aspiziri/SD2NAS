package thumbs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// captureProcs records each exiftool process c starts.
func captureProcs(c *Cache) func() []*exifProc {
	var mu sync.Mutex
	var procs []*exifProc
	c.exif.onStart = func(p *exifProc) {
		mu.Lock()
		procs = append(procs, p)
		mu.Unlock()
	}
	return func() []*exifProc {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(procs)
	}
}

// onlyProc returns the one process c has started.
func onlyProc(t *testing.T, procs func() []*exifProc) *exifProc {
	t.Helper()
	ps := procs()
	if len(ps) != 1 {
		t.Fatalf("%d exiftool processes started, want 1", len(ps))
	}
	return ps[0]
}

// wantGone fails unless p has exited and been waited for.
func wantGone(t *testing.T, p *exifProc) {
	t.Helper()
	select {
	case <-p.exited:
	default:
		t.Fatal("the exiftool process is still running")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("the exiftool process has no ProcessState")
	}
}

// locked runs f while holding e's lock, as a request would.
func (e *exifServer) locked(f func()) {
	e.lock <- struct{}{}
	defer func() { <-e.lock }()
	f()
}

// countLines counts the complete lines in file (0 when it doesn't exist).
func countLines(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte("\n"))
}

// wantPreview renders src and checks it is the fake's 64x32 preview turned
// by Orientation 6.
func wantPreview(t *testing.T, c *Cache, src Source) {
	t.Helper()
	dst, err := c.Get(context.Background(), src, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	img := decodeFile(t, dst)
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Fatalf("size = %v, want 32x64 (orientation 6 applied)", b)
	}
	if !reddish(img.At(16, 4)) {
		t.Errorf("top is not reddish: %v", img.At(16, 4))
	}
}

func TestExifServerReuses(t *testing.T) {
	c, argsFile := fakeCache(t, "preview", Config{})
	var want [][]string
	for i := range 3 {
		src := fakeSource(t, 400+i, media.KindRaw)
		wantPreview(t, c, src)
		want = append(want, exifArgs("PreviewImage", src.Path))
	}
	if n := fakeStarts(t); n != 1 {
		t.Fatalf("%d exiftool starts, want 1", n)
	}
	wantArgs(t, argsFile, want...)
}

func TestExifServerTimeout(t *testing.T) {
	c, _ := fakeCache(t, "hang", Config{Timeout: 200 * time.Millisecond})
	procs := captureProcs(c)
	src := fakeSource(t, 410, media.KindRaw)
	start := time.Now()
	_, err := c.Get(context.Background(), src, ThumbSize)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUndecodable) {
		t.Fatalf("err = %v, want DeadlineExceeded and not ErrUndecodable", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Get took %v", d)
	}
	wantGone(t, onlyProc(t, procs))
	t.Setenv("SD2NAS_FAKE_TOOL", "preview")
	wantPreview(t, c, src)
	if n := fakeStarts(t); n != 2 {
		t.Fatalf("%d exiftool starts, want 2", n)
	}
}

func TestExifServerDies(t *testing.T) {
	c, _ := fakeCache(t, "die", Config{})
	procs := captureProcs(c)
	src := fakeSource(t, 420, media.KindRaw)
	_, err := c.Get(context.Background(), src, ThumbSize)
	if err == nil || errors.Is(err, ErrUndecodable) {
		t.Fatalf("err = %v, want an error that is not ErrUndecodable", err)
	}
	// Wait (which closes the pipes) runs only after the readers are done,
	// so the dying tool's last stderr line is always there.
	if !strings.Contains(err.Error(), "fake tool: dying") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want one carrying exiftool's stderr and exit status", err)
	}
	wantGone(t, onlyProc(t, procs))
	t.Setenv("SD2NAS_FAKE_TOOL", "preview")
	wantPreview(t, c, src) // not remembered: rendered again by a new process
	if n := fakeStarts(t); n != 2 {
		t.Fatalf("%d exiftool starts, want 2", n)
	}
}

func TestExifServerHuge(t *testing.T) {
	old := maxToolOutput
	maxToolOutput = 1 << 20
	t.Cleanup(func() { maxToolOutput = old })
	c, _ := fakeCache(t, "huge", Config{})
	procs := captureProcs(c)
	_, err := c.Get(context.Background(), fakeSource(t, 430, media.KindRaw), ThumbSize)
	if !errors.Is(err, errTooLarge) || errors.Is(err, ErrUndecodable) {
		t.Fatalf("err = %v, want errTooLarge and not ErrUndecodable", err)
	}
	wantGone(t, onlyProc(t, procs))
}

func TestExifServerFileError(t *testing.T) {
	c, _ := fakeCache(t, "stderr", Config{})
	for i := range 2 {
		_, err := c.Get(context.Background(), fakeSource(t, 440+i, media.KindRaw), ThumbSize)
		if !errors.Is(err, ErrUndecodable) || !strings.Contains(err.Error(), "File format error") {
			t.Fatalf("file %d: err = %v, want ErrUndecodable carrying File format error", i, err)
		}
	}
	if n := fakeStarts(t); n != 1 {
		t.Fatalf("%d exiftool starts, want 1 (a file error leaves the process running)", n)
	}
}

func TestExifServerUnreadable(t *testing.T) {
	c, _ := fakeCache(t, "unreadable", Config{})
	src := fakeSource(t, 446, media.KindRaw)
	lock := writeSource(t, filepath.Dir(src.Path), filepath.Base(src.Path)+".locked", nil)
	_, err := c.Get(context.Background(), src, ThumbSize)
	if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrUndecodable) || !strings.Contains(err.Error(), "Error opening file") {
		t.Fatalf("err = %v, want fs.ErrPermission carrying Error opening file, not ErrUndecodable", err)
	}
	if err := os.Remove(lock); err != nil { // the owner fixes the permissions
		t.Fatal(err)
	}
	wantPreview(t, c, src) // not remembered: rendered again
	if n := fakeStarts(t); n != 1 {
		t.Fatalf("%d exiftool starts, want 1 (an unreadable file leaves the process running)", n)
	}
}

func TestExifServerFileGone(t *testing.T) {
	c, _ := fakeCache(t, "notfound", Config{})
	src := fakeSource(t, 445, media.KindRaw) // exists for generate's stat; the fake says it's gone
	_, err := c.Get(context.Background(), src, ThumbSize)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrUndecodable) {
		t.Fatalf("err = %v, want fs.ErrNotExist and not ErrUndecodable", err)
	}
	if _, err := c.Get(context.Background(), src, ThumbSize); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRemembered) {
		t.Fatalf("second Get err = %v, want a fresh fs.ErrNotExist (not remembered)", err)
	}
	if n := fakeStarts(t); n != 1 {
		t.Fatalf("%d exiftool starts, want 1", n)
	}
}

func TestExifServerForeignMarker(t *testing.T) {
	c, _ := fakeCache(t, "foreign", Config{})
	procs := captureProcs(c)
	src := fakeSource(t, 450, media.KindRaw)
	_, err := c.Get(context.Background(), src, ThumbSize)
	if err == nil || errors.Is(err, ErrUndecodable) || !strings.Contains(err.Error(), "{ready0}") {
		t.Fatalf("err = %v, want one naming {ready0} and not ErrUndecodable", err)
	}
	wantGone(t, onlyProc(t, procs))
	t.Setenv("SD2NAS_FAKE_TOOL", "preview")
	wantPreview(t, c, src)
}

func TestExifServerCancelWhileQueued(t *testing.T) {
	c, argsFile := fakeCache(t, "hang", Config{})
	e := c.exif
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := make(chan error, 1)
	go func() {
		_, _, err := e.run(ctx1, exifArgs("PreviewImage", "/a.ARW"))
		first <- err
	}()
	pollUntil(t, "the first request to reach exiftool", func() bool { return countLines(t, argsFile) == 1 })

	ctx2, cancel2 := context.WithCancel(context.Background())
	stop := time.AfterFunc(50*time.Millisecond, cancel2) // while it waits for the lock
	defer stop.Stop()
	if _, _, err := e.run(ctx2, exifArgs("PreviewImage", "/b.ARW")); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued request: err = %v, want context.Canceled", err)
	}
	if n := countLines(t, argsFile); n != 1 {
		t.Fatalf("exiftool got %d requests, want 1 (the queued one never reached it)", n)
	}
	cancel1()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first request: err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first request")
	}
	e.locked(func() {
		if e.seq != 1 || e.starts != 1 {
			t.Errorf("seq = %d, starts = %d, want 1 and 1", e.seq, e.starts)
		}
	})
}

func TestExifServerTimeoutWhileQueued(t *testing.T) {
	var logBuf bytes.Buffer
	e := newExifServer(os.Args[0], slog.New(slog.NewTextHandler(&logBuf, nil)))
	e.lock <- struct{}{} // another request holds exiftool
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := e.run(ctx, exifArgs("PreviewImage", "/a.ARW"))
	<-e.lock
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want DeadlineExceeded saying exiftool was busy", err)
	}
	if got := logBuf.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "timed out waiting for exiftool") {
		t.Fatalf("log = %q, want a WARN about the wait", got)
	}
	if e.starts != 0 {
		t.Fatalf("%d starts, want 0", e.starts)
	}
}

func TestExifServerPanicKills(t *testing.T) {
	c, _ := fakeCache(t, "preview", Config{Log: slog.New(slog.DiscardHandler)})
	procs := captureProcs(c)
	panicked := false // guarded by the exif lock: afterWrite runs under it
	c.exif.afterWrite = func() {
		if !panicked {
			panicked = true
			panic("boom")
		}
	}
	src := fakeSource(t, 460, media.KindRaw)
	_, err := c.Get(context.Background(), src, ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want one containing panicked", err)
	}
	wantGone(t, onlyProc(t, procs))
	wantPreview(t, c, src) // a new process, and the answer to this request
	// Counted here, not in the fake's log: the first may die before it logs.
	if n := len(procs()); n != 2 {
		t.Fatalf("%d exiftool starts, want 2", n)
	}
}

func TestExifServerClose(t *testing.T) {
	c, _ := fakeCache(t, "preview", Config{})
	procs := captureProcs(c)
	wantPreview(t, c, fakeSource(t, 470, media.KindRaw))
	start := time.Now()
	c.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	p := onlyProc(t, procs)
	wantGone(t, p)
	if p.waitErr != nil {
		t.Errorf("exiftool exited with %v, want a clean exit after -stay_open False", p.waitErr)
	}
	if _, err := c.Get(context.Background(), fakeSource(t, 471, media.KindRaw), ThumbSize); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close err = %v, want ErrClosed", err)
	}
}

func TestExifServerNewlinePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file names cannot contain a newline")
	}
	c, argsFile := fakeCache(t, "exif-preview", Config{})
	src := Source{shaN(480), writeSource(t, t.TempDir(), "a\nb.ARW", []byte("x")), media.KindRaw}
	wantPreview(t, c, src)
	if n := fakeStarts(t); n != 0 {
		t.Fatalf("%d exiftool starts, want 0 (a newline can't go in the argfile)", n)
	}
	wantArgs(t, argsFile, []string{"-json", "-b", "-n", "-Orientation", "-PreviewImage", "--", src.Path})
}

func TestExifServerRelativePath(t *testing.T) {
	c, argsFile := fakeCache(t, "exif-preview", Config{})
	// Without "--" a relative path could read as an option, so it goes to
	// a one-shot exiftool. (The fake doesn't open the file.)
	if _, err := c.rawPreview(context.Background(), "-x.ARW"); err != nil {
		t.Fatal(err)
	}
	if n := fakeStarts(t); n != 0 {
		t.Fatalf("%d exiftool starts, want 0", n)
	}
	wantArgs(t, argsFile, []string{"-json", "-b", "-n", "-Orientation", "-PreviewImage", "--", "-x.ARW"})
}

func TestExifServerMissingTool(t *testing.T) {
	c := newCache(t, Config{Exiftool: "sd2nas-no-such-tool"})
	for i := range 2 { // not remembered: each render tries again
		_, err := c.Get(context.Background(), fakeSource(t, 490, media.KindRaw), ThumbSize)
		if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, ErrUndecodable) {
			t.Fatalf("try %d: err = %v, want exec.ErrNotFound and not ErrUndecodable", i, err)
		}
	}
	c.exif.locked(func() {
		if c.exif.proc != nil {
			t.Error("a process is recorded although none started")
		}
	})
}

// TestRealExiftoolStayOpen drives the real exiftool's -stay_open protocol:
// several files (one with a name full of argfile pitfalls) and a corrupt
// one through one process, which then stops cleanly on Close.
func TestRealExiftoolStayOpen(t *testing.T) {
	needTool(t, "exiftool")
	jpg, err := encodeJPEG(redBlueImage(64, 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := newCache(t, Config{})
	procs := captureProcs(c)
	for i, name := range []string{"a.ARW", "-x #1 é = y.ARW", "b.ARW"} {
		path := filepath.Join(dir, name)
		writeARW(t, path, jpg, 6)
		wantPreview(t, c, Source{shaN(500 + i), path, media.KindRaw})
	}
	bad := writeSource(t, dir, "bad.ARW", []byte("II*\x00\xff\xff\xff\xffgarbage"))
	_, err = c.Get(context.Background(), Source{shaN(510), bad, media.KindRaw}, ThumbSize)
	if !errors.Is(err, ErrUndecodable) || !strings.Contains(err.Error(), "File format error") {
		t.Fatalf("corrupt ARW: err = %v, want ErrUndecodable carrying File format error", err)
	}
	// A file that went away after generate's stat is missing, not corrupt.
	if _, err := c.rawPreview(context.Background(), filepath.Join(dir, "gone.ARW")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("vanished ARW: err = %v, want fs.ErrNotExist", err)
	}
	p := onlyProc(t, procs)
	start := time.Now()
	c.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	wantGone(t, p)
	// It exits by itself after -stay_open False (status 1 here: a command
	// had an error) rather than being killed.
	if !p.cmd.ProcessState.Exited() {
		t.Errorf("exiftool ended with %v, want an exit after -stay_open False", p.cmd.ProcessState)
	}
}
