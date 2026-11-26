package thumbs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disintegration/imaging"

	"github.com/aspiziri/SD2NAS/internal/media"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("SD2NAS_FAKE_TOOL"); mode != "" {
		os.Exit(fakeTool(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeTool stands in for exiftool or ffmpeg when a test sets
// SD2NAS_FAKE_TOOL and points Config.Exiftool/FFmpeg at os.Args[0]. Run
// once per call, it appends its arguments as one JSON line to
// $SD2NAS_FAKE_ARGS and answers as the mode says. Started with -stay_open,
// it serves exiftool's argfile protocol instead (fakeStayOpen).
func fakeTool(mode string, args []string) int {
	if slices.Contains(args, "-stay_open") {
		return fakeStayOpen(mode)
	}
	if err := appendJSONLine(os.Getenv("SD2NAS_FAKE_ARGS"), args); err != nil {
		fmt.Fprintln(os.Stderr, "fake tool: recording args:", err)
		return 99
	}
	code, _ := fakeAnswer(mode, args, os.Stdout, os.Stderr)
	return code
}

// appendJSONLine appends v as one JSON line to file.
func appendJSONLine(file string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// fakeStayOpen serves "exiftool -stay_open True -@ -": it logs its pid to
// $SD2NAS_FAKE_TOOL_LOG, then reads argfile lines from stdin. At each
// -execute<N> it records the request's arguments (without the -echo4
// pair) to $SD2NAS_FAKE_ARGS, answers as the mode says, and ends the
// answer with {ready<N>} on stdout and the -echo4 text on stderr. A mode
// that makes a one-shot tool exit makes it exit (it dies). "-stay_open",
// "False" or the end of stdin makes it exit 0.
func fakeStayOpen(mode string) int {
	if err := appendJSONLine(os.Getenv("SD2NAS_FAKE_TOOL_LOG"), os.Getpid()); err != nil {
		fmt.Fprintln(os.Stderr, "fake tool: logging the start:", err)
		return 99
	}
	sc := bufio.NewScanner(os.Stdin)
	var req []string
	for sc.Scan() {
		line := sc.Text()
		if line == "False" && len(req) > 0 && req[len(req)-1] == "-stay_open" {
			return 0
		}
		n, ok := strings.CutPrefix(line, "-execute")
		if !ok {
			req = append(req, line)
			continue
		}
		args, echo := req, ""
		if i := slices.Index(args, "-echo4"); i >= 0 && i+1 < len(args) {
			echo = args[i+1]
			args = slices.Delete(slices.Clone(args), i, i+2)
		}
		req = nil
		if err := appendJSONLine(os.Getenv("SD2NAS_FAKE_ARGS"), args); err != nil {
			fmt.Fprintln(os.Stderr, "fake tool: recording args:", err)
			return 99
		}
		var out bytes.Buffer
		code, stop := fakeAnswer(mode, args, &out, os.Stderr)
		if _, err := os.Stdout.Write(out.Bytes()); err != nil || stop {
			return code
		}
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			fmt.Println() // exiftool's JSON ends with a newline
		}
		fmt.Printf("{ready%s}\n", n)
		fmt.Fprintln(os.Stderr, echo)
	}
	return 0
}

// fakeAnswer writes mode's answer to one call with args. stop says that
// the tool exits with code (a failure, or a stay_open tool that dies).
func fakeAnswer(mode string, args []string, stdout, stderr io.Writer) (code int, stop bool) {
	jpg, err := encodeJPEG(redBlueImage(64, 32), 0)
	if err != nil {
		fmt.Fprintln(stderr, "fake tool:", err)
		return 99, true
	}
	b64 := func(b []byte) string { return "base64:" + base64.StdEncoding.EncodeToString(b) }
	write := func(b []byte) (int, bool) {
		if _, err := stdout.Write(b); err != nil {
			return 99, true
		}
		return 0, false
	}
	switch mode {
	case "exif-preview", "preview":
		fmt.Fprintf(stdout, `[{"SourceFile":"x","Orientation":6,"PreviewImage":"%s"}]`, b64(jpg))
	case "exif-jpgfromraw":
		if slices.Contains(args, "-JpgFromRaw") {
			fmt.Fprintf(stdout, `[{"SourceFile":"x","JpgFromRaw":"%s"}]`, b64(jpg))
		} else {
			fmt.Fprint(stdout, `[{"SourceFile":"x","Orientation":1}]`)
		}
	case "exif-none":
		fmt.Fprint(stdout, `[{"SourceFile":"x"}]`)
	case "exif-error":
		fmt.Fprint(stdout, `[{"SourceFile":"x","Error":"File is empty"}]`)
	case "exif-huge":
		fmt.Fprintf(stdout, `[{"SourceFile":"x","PreviewImage":"%s"}]`, b64(hugePNG(20000, 20000)))
	case "huge-frame":
		return write(hugePNG(20000, 20000))
	case "garbage":
		fmt.Fprint(stdout, "not json")
	case "fail":
		fmt.Fprint(stderr, "boom: bad file")
		return 3, true
	case "die":
		fmt.Fprintln(stderr, "fake tool: dying")
		return 3, true
	case "stderr":
		fmt.Fprintln(stderr, "Error: File format error - x")
	case "notfound":
		fmt.Fprintln(stderr, "Error: File not found - x")
	case "unreadable":
		// Unreadable while <path>.locked exists (a permission fault the
		// owner can fix), then the normal preview.
		if _, err := os.Stat(args[len(args)-1] + ".locked"); err == nil {
			fmt.Fprint(stdout, `[{"SourceFile":"x"}]`)
			fmt.Fprintln(stderr, "Error: Error opening file - x")
			break
		}
		fmt.Fprintf(stdout, `[{"SourceFile":"x","Orientation":6,"PreviewImage":"%s"}]`, b64(jpg))
	case "foreign":
		fmt.Fprint(stdout, "{ready0}\n")
		fmt.Fprintf(stdout, `[{"SourceFile":"x","Orientation":6,"PreviewImage":"%s"}]`, b64(jpg))
	case "hang":
		time.Sleep(time.Minute)
	case "ffmpeg-short", "ffmpeg-short-old":
		for i, a := range args {
			if a == "-ss" && i+1 < len(args) && args[i+1] == "0" {
				return write(jpg)
			}
		}
		if mode == "ffmpeg-short" {
			fmt.Fprint(stderr, "[out#0/image2pipe @ 0x0] Nothing was written into output file, because at least one of its streams received no packets.")
			return 234, true
		}
	case "big", "huge":
		return write(make([]byte, 2<<20)) // tests lower maxToolOutput to 1 MiB
	default:
		fmt.Fprintln(stderr, "fake tool: unknown mode", mode)
		return 98, true
	}
	return 0, false
}

// fakeCache returns a Cache whose exiftool and ffmpeg are the fake tool in
// the given mode, and the file that records the arguments of each call
// (each request, for exiftool). Each start of a stay_open fake is logged,
// see fakeStarts.
func fakeCache(t *testing.T, mode string, cfg Config) (*Cache, string) {
	t.Helper()
	cfg.Exiftool = os.Args[0]
	cfg.FFmpeg = os.Args[0]
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.jsonl")
	t.Setenv("SD2NAS_FAKE_TOOL", mode)
	t.Setenv("SD2NAS_FAKE_ARGS", argsFile)
	t.Setenv("SD2NAS_FAKE_TOOL_LOG", filepath.Join(dir, "starts.jsonl"))
	// A -race build sleeps 1 s before exiting, which would make every
	// Close of a stay_open fake time out and kill it.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return newCache(t, cfg), argsFile
}

// fakeStarts returns how many stay_open fakes have started in this test.
func fakeStarts(t *testing.T) int {
	t.Helper()
	return countLines(t, os.Getenv("SD2NAS_FAKE_TOOL_LOG"))
}

// recordedArgs reads back the calls the fake tool recorded.
func recordedArgs(t *testing.T, file string) [][]string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var calls [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var a []string
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("recorded args %q: %v", l, err)
		}
		calls = append(calls, a)
	}
	return calls
}

// needTool returns the path of a real tool, skipping the test when it is
// not installed, or failing it when SD2NAS_REQUIRE_TOOLS=1 (CI and the
// Linux Docker run set it, so these tests can't silently stop running).
func needTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("SD2NAS_REQUIRE_TOOLS") == "1" {
			t.Fatalf("%s is required (SD2NAS_REQUIRE_TOOLS=1): %v", name, err)
		}
		t.Skipf("%s is not installed: %v", name, err)
	}
	return p
}

// writeARW writes a minimal little-endian TIFF that exiftool reads as a
// Sony ARW whose PreviewImage is preview.
func writeARW(t *testing.T, path string, preview []byte, orientation uint16) {
	t.Helper()
	const (
		makeOff    = 74
		modelOff   = 80
		previewOff = 90
	)
	var b []byte
	b = append(b, 'I', 'I', 42, 0, 8, 0, 0, 0)
	b = binary.LittleEndian.AppendUint16(b, 5)
	entry := func(tag, typ uint16, count, value uint32) {
		b = binary.LittleEndian.AppendUint16(b, tag)
		b = binary.LittleEndian.AppendUint16(b, typ)
		b = binary.LittleEndian.AppendUint32(b, count)
		b = binary.LittleEndian.AppendUint32(b, value)
	}
	entry(0x010F, 2, 5, makeOff)
	entry(0x0110, 2, 9, modelOff)
	entry(0x0112, 3, 1, uint32(orientation))
	entry(0x0201, 4, 1, previewOff)
	entry(0x0202, 4, 1, uint32(len(preview)))
	b = append(b, 0, 0, 0, 0) // no next IFD
	b = append(b, "SONY\x00\x00"...)
	b = append(b, "ILCE-7M4\x00\x00"...)
	if len(b) != previewOff {
		t.Fatalf("ARW header is %d bytes, want %d", len(b), previewOff)
	}
	b = append(b, preview...)
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
}

func fakeSource(t *testing.T, n int, kind media.Kind) Source {
	t.Helper()
	return Source{shaN(n), writeSource(t, t.TempDir(), "file.bin", []byte("x")), kind}
}

func wantArgs(t *testing.T, file string, want ...[]string) {
	t.Helper()
	if got := recordedArgs(t, file); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded calls = %q\nwant %q", got, want)
	}
}

func TestRawPreviewArgs(t *testing.T) {
	c, argsFile := fakeCache(t, "exif-preview", Config{})
	src := fakeSource(t, 1, media.KindRaw)
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
	wantArgs(t, argsFile, exifArgs("PreviewImage", src.Path))
	if n := fakeStarts(t); n != 1 {
		t.Errorf("%d exiftool starts, want 1", n)
	}
}

// exifArgs is one request to the stay_open exiftool for tag of path. The
// path is absolute and goes without "--": exiftool ignores -execute after
// "--" and would never answer.
func exifArgs(tag, path string) []string {
	return []string{"-charset", "filename=utf8", "-json", "-b", "-n", "-Orientation", "-" + tag, path}
}

func TestRawFallsBackToJpgFromRaw(t *testing.T) {
	c, argsFile := fakeCache(t, "exif-jpgfromraw", Config{})
	src := fakeSource(t, 2, media.KindRaw)
	dst, err := c.Get(context.Background(), src, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, dst); w != 64 || h != 32 {
		t.Fatalf("size = %dx%d, want 64x32", w, h)
	}
	wantArgs(t, argsFile, exifArgs("PreviewImage", src.Path), exifArgs("JpgFromRaw", src.Path))
	if n := fakeStarts(t); n != 1 {
		t.Errorf("%d exiftool starts, want 1 (both requests share the process)", n)
	}
}

func TestRawNoPreview(t *testing.T) {
	c, _ := fakeCache(t, "exif-none", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 3, media.KindRaw), ThumbSize)
	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("err = %v, want ErrNoImage", err)
	}
}

func TestRawRejectsHugePreview(t *testing.T) {
	c, _ := fakeCache(t, "exif-huge", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 4, media.KindRaw), ThumbSize)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestRawJSONError(t *testing.T) {
	c, _ := fakeCache(t, "exif-error", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 14, media.KindRaw), ThumbSize)
	if !errors.Is(err, ErrUndecodable) || !strings.Contains(err.Error(), "File is empty") {
		t.Fatalf("err = %v, want ErrUndecodable carrying the JSON Error", err)
	}
}

func TestToolFailureSurfacesStderr(t *testing.T) {
	c, _ := fakeCache(t, "stderr", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 5, media.KindRaw), ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "File format error") {
		t.Fatalf("err = %v, want one containing the tool's stderr", err)
	}
	if !errors.Is(err, ErrUndecodable) {
		t.Errorf("err = %v, want ErrUndecodable", err)
	}
}

func TestToolGarbage(t *testing.T) {
	c, _ := fakeCache(t, "garbage", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 6, media.KindRaw), ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "exiftool output") {
		t.Fatalf("err = %v, want one containing exiftool output", err)
	}
	if !errors.Is(err, ErrUndecodable) {
		t.Errorf("err = %v, want ErrUndecodable", err)
	}
}

func TestToolTimeoutKills(t *testing.T) {
	c, _ := fakeCache(t, "hang", Config{Timeout: 300 * time.Millisecond})
	src := fakeSource(t, 7, media.KindRaw)
	start := time.Now()
	_, err := c.Get(context.Background(), src, ThumbSize)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if errors.Is(err, ErrUndecodable) {
		t.Errorf("a timeout is not ErrUndecodable: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Get took %v", d)
	}
}

func TestToolOutputCapped(t *testing.T) {
	old := maxToolOutput
	maxToolOutput = 1 << 20
	t.Cleanup(func() { maxToolOutput = old })
	c, argsFile := fakeCache(t, "big", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 8, media.KindVideo), ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "larger than") || !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
	if errors.Is(err, ErrUndecodable) {
		t.Errorf("runaway output is not ErrUndecodable: %v", err)
	}
	if n := len(recordedArgs(t, argsFile)); n != 1 {
		t.Fatalf("%d calls, want 1 (runaway output is not retried at 0 s)", n)
	}
}

func videoArgs(at, path string) []string {
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-ss", at, "-i", "file:" + path,
		"-frames:v", "1", "-an", "-sn", "-dn", "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "2", "pipe:1"}
}

func TestVideoShortClipRetriesAtZero(t *testing.T) {
	for _, mode := range []string{"ffmpeg-short", "ffmpeg-short-old"} {
		t.Run(mode, func(t *testing.T) {
			c, argsFile := fakeCache(t, mode, Config{})
			src := fakeSource(t, 9, media.KindVideo)
			dst, err := c.Get(context.Background(), src, ThumbSize)
			if err != nil {
				t.Fatal(err)
			}
			if w, h := dims(t, dst); w != 64 || h != 32 {
				t.Fatalf("size = %dx%d, want 64x32", w, h)
			}
			wantArgs(t, argsFile, videoArgs("1", src.Path), videoArgs("0", src.Path))
		})
	}
}

func TestVideoFailureReportsBoth(t *testing.T) {
	c, argsFile := fakeCache(t, "fail", Config{})
	src := fakeSource(t, 10, media.KindVideo)
	_, err := c.Get(context.Background(), src, ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "boom: bad file") {
		t.Fatalf("err = %v, want one containing the tool's stderr", err)
	}
	if n := strings.Count(err.Error(), "boom: bad file"); n != 2 {
		t.Errorf("error mentions the failure %d times, want 2 (both attempts): %v", n, err)
	}
	if !errors.Is(err, ErrUndecodable) {
		t.Errorf("err = %v, want ErrUndecodable", err)
	}
	wantArgs(t, argsFile, videoArgs("1", src.Path), videoArgs("0", src.Path))
}

func TestVideoBadFrame(t *testing.T) {
	c, _ := fakeCache(t, "garbage", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 11, media.KindVideo), ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "decode video frame") {
		t.Fatalf("err = %v, want one containing decode video frame", err)
	}
	if !errors.Is(err, ErrUndecodable) {
		t.Errorf("err = %v, want ErrUndecodable", err)
	}
}

func TestVideoRejectsHugeFrame(t *testing.T) {
	c, _ := fakeCache(t, "huge-frame", Config{})
	_, err := c.Get(context.Background(), fakeSource(t, 12, media.KindVideo), ThumbSize)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestOrientMatchesImaging(t *testing.T) {
	for o := 1; o <= 8; o++ {
		b, err := encodeJPEG(redBlueImage(8, 4), uint16(o))
		if err != nil {
			t.Fatal(err)
		}
		want, err := imaging.Decode(bytes.NewReader(b), imaging.AutoOrientation(true))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := imaging.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		got := orient(raw, o)
		if got.Bounds() != want.Bounds() {
			t.Fatalf("o=%d: bounds %v, want %v", o, got.Bounds(), want.Bounds())
		}
		if !bytes.Equal(imaging.Clone(got).Pix, imaging.Clone(want).Pix) {
			t.Errorf("o=%d: pixels differ from imaging's AutoOrientation", o)
		}
	}
}

func TestMissingTools(t *testing.T) {
	c := newCache(t, Config{Exiftool: "sd2nas-no-such-tool", FFmpeg: os.Args[0]})
	want := []string{"sd2nas-no-such-tool not found: RAW thumbnails are unavailable"}
	if got := c.MissingTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingTools() = %q, want %q", got, want)
	}
	_, err := c.Get(context.Background(), fakeSource(t, 13, media.KindRaw), ThumbSize)
	if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, ErrUndecodable) {
		t.Fatalf("err = %v, want exec.ErrNotFound and not ErrUndecodable", err)
	}
}

func TestRealExiftoolARW(t *testing.T) {
	needTool(t, "exiftool")
	jpg, err := encodeJPEG(redBlueImage(64, 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.ARW")
	writeARW(t, path, jpg, 6)
	c := newCache(t, Config{})
	dst, err := c.Get(context.Background(), Source{shaN(20), path, media.KindRaw}, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	img := decodeFile(t, dst)
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Fatalf("size = %v, want 32x64", b)
	}
	if !reddish(img.At(16, 4)) {
		t.Errorf("top is not reddish: %v", img.At(16, 4))
	}
}

func TestRealFFmpegVideo(t *testing.T) {
	ff := needTool(t, "ffmpeg")
	dir := t.TempDir()
	clips := []struct{ name, duration string }{{"long.mp4", "2"}, {"my clip.mp4", "2"}, {"short.mp4", "0.5"}}
	c := newCache(t, Config{})
	for i, clip := range clips {
		path := filepath.Join(dir, clip.name)
		out, err := exec.Command(ff, "-nostdin", "-v", "error", "-f", "lavfi",
			"-i", "testsrc=duration="+clip.duration+":size=320x240:rate=10", "-pix_fmt", "yuv420p", path).CombinedOutput()
		if err != nil {
			t.Fatalf("make %s: %v: %s", clip.name, err, out)
		}
		dst, err := c.Get(context.Background(), Source{shaN(30 + i), path, media.KindVideo}, ThumbSize)
		if err != nil {
			t.Fatalf("%s: %v", clip.name, err)
		}
		if w, h := dims(t, dst); w != 320 || h != 240 {
			t.Errorf("%s: size = %dx%d, want 320x240", clip.name, w, h)
		}
	}
}
