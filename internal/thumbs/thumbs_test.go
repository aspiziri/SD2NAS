package thumbs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// redBlueImage is w×h, red on the left half and blue on the right.
func redBlueImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	return img
}

// encodeJPEG encodes img (quality 95) and, when orientation > 0, inserts an
// APP1 EXIF segment right after SOI whose IFD0 holds only Orientation
// (tag 0x0112, SHORT, 1).
func encodeJPEG(img image.Image, orientation uint16) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		return nil, err
	}
	if orientation == 0 {
		return buf.Bytes(), nil
	}
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, // little-endian header, IFD0 at offset 8
		1, 0, // one entry
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(orientation), byte(orientation >> 8), 0, 0,
		0, 0, 0, 0} // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	out = append(out, payload...)
	out = append(out, buf.Bytes()[2:]...)
	return out, nil
}

func reddish(c color.Color) bool {
	r, _, b, _ := c.RGBA()
	return r > 2*b
}

func bluish(c color.Color) bool {
	r, _, b, _ := c.RGBA()
	return b > 2*r
}

func decodeFile(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return img
}

func newCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func writeSource(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

func shaN(n int) string { return fmt.Sprintf("%064x", n) }

func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeJPEGSource writes a w×h red/blue JPEG into a fresh temp dir.
func writeJPEGSource(t *testing.T, w, h int, orientation uint16) string {
	t.Helper()
	data, err := encodeJPEG(redBlueImage(w, h), orientation)
	if err != nil {
		t.Fatal(err)
	}
	return writeSource(t, t.TempDir(), "a.jpg", data)
}

func dims(t *testing.T, path string) (int, int) {
	t.Helper()
	b := decodeFile(t, path).Bounds()
	return b.Dx(), b.Dy()
}

func TestGetJPEGThumb(t *testing.T) {
	dir := t.TempDir()
	c := newCache(t, Config{Dir: dir})
	src := Source{shaN(1), writeJPEGSource(t, 1200, 800, 0), media.KindImage}
	got, err := c.Get(context.Background(), src, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, src.SHA256[:2], src.SHA256+"-400.jpg"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if w, h := dims(t, got); w != 400 || h != 266 {
		t.Fatalf("size = %dx%d, want 400x266", w, h)
	}
}

func TestGetPreviewNotUpscaled(t *testing.T) {
	c := newCache(t, Config{})
	src := Source{shaN(2), writeJPEGSource(t, 300, 200, 0), media.KindImage}
	got, err := c.Get(context.Background(), src, PreviewSize)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, got); w != 300 || h != 200 {
		t.Fatalf("size = %dx%d, want 300x200", w, h)
	}
}

func TestGetAppliesOrientation(t *testing.T) {
	c := newCache(t, Config{})
	src := Source{shaN(3), writeJPEGSource(t, 64, 32, 6), media.KindImage}
	got, err := c.Get(context.Background(), src, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, got); w != 32 || h != 64 {
		t.Fatalf("size = %dx%d, want 32x64", w, h)
	}
	img := decodeFile(t, got)
	if !reddish(img.At(16, 8)) {
		t.Errorf("pixel (16,8) = %v, want reddish", img.At(16, 8))
	}
	if !bluish(img.At(16, 56)) {
		t.Errorf("pixel (16,56) = %v, want bluish", img.At(16, 56))
	}
}

func TestGetPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, redBlueImage(500, 500)); err != nil {
		t.Fatal(err)
	}
	c := newCache(t, Config{})
	src := Source{shaN(4), writeSource(t, t.TempDir(), "a.png", buf.Bytes()), media.KindImage}
	got, err := c.Get(context.Background(), src, ThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, got); w != 400 || h != 400 {
		t.Fatalf("size = %dx%d, want 400x400", w, h)
	}
}

func TestGetCached(t *testing.T) {
	c := newCache(t, Config{})
	orig := c.render
	var n atomic.Int32
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		n.Add(1)
		return orig(ctx, src)
	}
	src := Source{shaN(5), writeJPEGSource(t, 800, 600, 0), media.KindImage}
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), src, ThumbSize); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("renders after two Gets = %d, want 1", got)
	}
	if _, err := c.Get(context.Background(), src, PreviewSize); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("renders after the other size = %d, want 2", got)
	}
}

func TestGetSingleFlight(t *testing.T) {
	c := newCache(t, Config{})
	release := make(chan struct{})
	var n atomic.Int32
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		n.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return redBlueImage(10, 10), nil
	}
	src := Source{shaN(6), writeSource(t, t.TempDir(), "a.bin", []byte("x")), media.KindImage}
	const callers = 5
	paths := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = c.Get(context.Background(), src, ThumbSize)
		}()
	}
	pollUntil(t, "the render to start", func() bool { return n.Load() == 1 })
	close(release)
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatalf("caller %d path %q != %q", i, paths[i], paths[0])
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("renders = %d, want 1", got)
	}
}

func TestGetWorkersBound(t *testing.T) {
	c := newCache(t, Config{Workers: 2})
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		cur := active.Add(1)
		for {
			m := maxActive.Load()
			if cur <= m || maxActive.CompareAndSwap(m, cur) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return redBlueImage(10, 10), nil
	}
	path := writeSource(t, t.TempDir(), "a.bin", []byte("x"))
	const callers = 6
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Get(context.Background(), Source{shaN(100 + i), path, media.KindImage}, ThumbSize)
		}()
	}
	pollUntil(t, "two active renders", func() bool { return active.Load() == 2 })
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max concurrent renders = %d, want 2", got)
	}
}

func (c *Cache) waitersFor(dst string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl := c.calls[dst]; cl != nil {
		return cl.waiters
	}
	return 0
}

func TestGetCallerCancelKeepsSharedRender(t *testing.T) {
	c := newCache(t, Config{})
	release := make(chan struct{})
	renderErr := make(chan error, 1)
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		renderErr <- ctx.Err()
		return redBlueImage(10, 10), nil
	}
	src := Source{shaN(7), writeSource(t, t.TempDir(), "a.bin", []byte("x")), media.KindImage}
	dst := c.path(src.SHA256, ThumbSize)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	aErr := make(chan error, 1)
	go func() {
		_, err := c.Get(ctxA, src, ThumbSize)
		aErr <- err
	}()
	type result struct {
		path string
		err  error
	}
	bRes := make(chan result, 1)
	go func() {
		p, err := c.Get(context.Background(), src, ThumbSize)
		bRes <- result{p, err}
	}()
	pollUntil(t, "both callers to wait", func() bool { return c.waitersFor(dst) == 2 })

	cancelA()
	select {
	case err := <-aErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("A: err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for A to return")
	}
	close(release)
	select {
	case r := <-bRes:
		if r.err != nil {
			t.Fatalf("B: %v", r.err)
		}
		if r.path != dst {
			t.Fatalf("B path = %q, want %q", r.path, dst)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for B to return")
	}
	if err := <-renderErr; err != nil {
		t.Fatalf("render saw ctx.Err() = %v, want nil", err)
	}
}

func TestGetLastCallerCancelCancelsRender(t *testing.T) {
	c := newCache(t, Config{})
	started := make(chan struct{})
	renderDone := make(chan error, 1)
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		close(started)
		<-ctx.Done()
		renderDone <- ctx.Err()
		return nil, ctx.Err()
	}
	src := Source{shaN(8), writeSource(t, t.TempDir(), "a.bin", []byte("x")), media.KindImage}
	ctx, cancel := context.WithCancel(context.Background())
	getErr := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, src, ThumbSize)
		getErr <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the render to start")
	}
	cancel()
	select {
	case err := <-renderDone:
		if err == nil {
			t.Fatal("render ctx not cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the render ctx to be done")
	}
	if err := <-getErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v, want context.Canceled", err)
	}
	pollUntil(t, "the call entry to be removed", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.calls) == 0
	})
}

func TestGetTimeout(t *testing.T) {
	dir := t.TempDir()
	c := newCache(t, Config{Dir: dir, Timeout: 50 * time.Millisecond})
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	src := Source{shaN(9), writeSource(t, t.TempDir(), "a.bin", []byte("x")), media.KindImage}
	_, err := c.Get(context.Background(), src, ThumbSize)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".jpg") || strings.HasSuffix(p, ".tmp")) {
			t.Errorf("unexpected file %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetRenderErrorNotCached(t *testing.T) {
	c := newCache(t, Config{})
	orig := c.render
	var n atomic.Int32
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		if n.Add(1) == 1 {
			return nil, errors.New("broken")
		}
		return orig(ctx, src)
	}
	src := Source{shaN(10), writeJPEGSource(t, 100, 100, 0), media.KindImage}
	if _, err := c.Get(context.Background(), src, ThumbSize); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("first Get err = %v, want one containing broken", err)
	}
	if _, err := c.Get(context.Background(), src, ThumbSize); err != nil {
		t.Fatalf("second Get: %v", err)
	}
}

func TestGetBadInput(t *testing.T) {
	dir := t.TempDir()
	c := newCache(t, Config{Dir: dir})
	path := writeSource(t, t.TempDir(), "a.bin", []byte("x"))
	for _, sha := range []string{"../../etc/passwd", strings.Repeat("A", 64), strings.Repeat("a", 63), ""} {
		_, err := c.Get(context.Background(), Source{sha, path, media.KindImage}, ThumbSize)
		if !errors.Is(err, ErrBadSHA) {
			t.Errorf("sha %q: err = %v, want ErrBadSHA", sha, err)
		}
	}
	if _, err := c.Get(context.Background(), Source{shaN(11), path, media.KindImage}, 500); !errors.Is(err, ErrBadSize) {
		t.Errorf("size 500: err = %v, want ErrBadSize", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Dir has %d entries, want none", len(entries))
	}
}

// hugePNG is a PNG header declaring w×h pixels, with no pixel data.
func hugePNG(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // 8-bit RGB
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(binary.BigEndian.AppendUint32(nil, uint32(len(ihdr))))
	b.Write(chunk)
	b.Write(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(chunk)))
	return b.Bytes()
}

func TestGetRejectsHugeImage(t *testing.T) {
	c := newCache(t, Config{})
	for _, d := range [][2]uint32{{20000, 20000}, {2000000000, 2}} {
		src := Source{shaN(20), writeSource(t, t.TempDir(), "huge.png", hugePNG(d[0], d[1])), media.KindImage}
		start := time.Now()
		_, err := c.Get(context.Background(), src, ThumbSize)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%v: err = %v, want ErrTooLarge", d, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%dx%d", d[0], d[1])) {
			t.Errorf("err %q does not name the dimensions", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("rejection took %v", time.Since(start))
		}
	}
}

func TestGetMissingSource(t *testing.T) {
	c := newCache(t, Config{})
	src := Source{shaN(12), filepath.Join(t.TempDir(), "nope.jpg"), media.KindImage}
	if _, err := c.Get(context.Background(), src, ThumbSize); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestGetUnknownKind(t *testing.T) {
	c := newCache(t, Config{})
	src := Source{shaN(13), writeSource(t, t.TempDir(), "a.bin", []byte("x")), "bogus"}
	_, err := c.Get(context.Background(), src, ThumbSize)
	if err == nil || !strings.Contains(err.Error(), "no renderer") {
		t.Fatalf("err = %v, want one containing no renderer", err)
	}
}

func TestNewRemovesStaleTemps(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ab"), 0o777); err != nil {
		t.Fatal(err)
	}
	stale := writeSource(t, filepath.Join(dir, "ab"), "x.jpg.123.tmp", []byte("x"))
	keep := writeSource(t, filepath.Join(dir, "ab"), "keep.jpg", []byte("x"))
	newCache(t, Config{Dir: dir})
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp: stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("keep.jpg: %v", err)
	}
}

func TestCloseCancelsRenders(t *testing.T) {
	c := newCache(t, Config{})
	c.render = func(ctx context.Context, src Source) (image.Image, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	src := Source{shaN(14), writeSource(t, t.TempDir(), "a.bin", []byte("x")), media.KindImage}
	dst := c.path(src.SHA256, ThumbSize)
	getErr := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), src, ThumbSize)
		getErr <- err
	}()
	pollUntil(t, "the Get to wait", func() bool { return c.waitersFor(dst) == 1 })

	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Close to return")
	}
	select {
	case err := <-getErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the Get to return")
	}
	if _, err := c.Get(context.Background(), src, ThumbSize); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close err = %v, want ErrClosed", err)
	}
}

func TestValidSHA(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a1", 32), true},
		{shaN(255), true},
		{strings.Repeat("A", 64), false},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("g", 64), false},
		{"../../etc/passwd", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ValidSHA(tt.in); got != tt.want {
			t.Errorf("ValidSHA(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
