// Package thumbs renders and caches the JPEG thumbnails (ThumbSize) and
// previews (PreviewSize) the web UI shows for staged files. JPEG, PNG, GIF
// and TIFF are decoded and resized in Go; RAW files give up their embedded
// preview through exiftool, and videos a frame at 1 s through ffmpeg.
// Results are cached under Config.Dir by content hash and size.
package thumbs

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// Longest edge, in pixels, of the two sizes the UI asks for.
const (
	ThumbSize   = 400
	PreviewSize = 1600
)

var (
	ErrBadSHA  = errors.New("thumbs: hash must be 64 lowercase hex digits")
	ErrBadSize = errors.New("thumbs: size must be ThumbSize or PreviewSize")
	ErrNoImage = errors.New("thumbs: the file has no embedded preview or video frame")
	ErrClosed  = errors.New("thumbs: cache closed")
	// ErrTooLarge means the image declares more than maxPixels pixels; it is
	// refused before decoding so that it cannot exhaust memory.
	ErrTooLarge = errors.New("thumbs: image has too many pixels to decode")
)

// maxPixels caps the pixels of any image decoded in Go (120 megapixels).
const maxPixels = 120_000_000

// Source is a file to render.
type Source struct {
	SHA256 string     // content hash: the cache key
	Path   string     // OS path of the file
	Kind   media.Kind // image, raw or video
}

// Config configures a Cache; zero values get the defaults.
type Config struct {
	Dir      string        // cache root, e.g. /config/thumbs (created if missing)
	Exiftool string        // command for RAW previews; default "exiftool"
	FFmpeg   string        // command for video frames; default "ffmpeg"
	Workers  int           // renders at once; default max(1, min(NumCPU, 4))
	Timeout  time.Duration // per render, from when it gets a worker slot; default 60 s
	Log      *slog.Logger  // default slog.Default()
}

// call is one render that any number of Get calls wait for.
type call struct {
	done    chan struct{} // closed when the render has finished
	err     error         // set before done is closed
	waiters int           // Get calls still waiting; guarded by Cache.mu
	cancel  context.CancelFunc
}

// Cache renders thumbnails on demand and keeps them on disk.
type Cache struct {
	cfg    Config
	sem    chan struct{} // one slot per running render
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // running renders
	mu     sync.Mutex
	calls  map[string]*call // by destination path
	closed bool
	render func(ctx context.Context, src Source) (image.Image, error) // renderSource; tests replace it
}

// New creates the cache directory and removes unfinished files a crash
// left behind.
func New(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("thumbs: Config.Dir is empty")
	}
	if cfg.Exiftool == "" {
		cfg.Exiftool = "exiftool"
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = max(1, min(runtime.NumCPU(), 4))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o777); err != nil {
		return nil, fmt.Errorf("thumbnail cache: %w", err)
	}
	removed, err := removeTemps(cfg.Dir)
	if removed > 0 {
		cfg.Log.Info("removed unfinished thumbnails", "count", removed)
	}
	if err != nil {
		return nil, fmt.Errorf("thumbnail cache: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{cfg: cfg, sem: make(chan struct{}, cfg.Workers), ctx: ctx, cancel: cancel, calls: map[string]*call{}}
	c.render = c.renderSource
	return c, nil
}

// removeTemps deletes *.tmp files below dir.
func removeTemps(dir string) (int, error) {
	n := 0
	var errs []error
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			if err := os.Remove(p); err != nil {
				errs = append(errs, err)
			} else {
				n++
			}
		}
		return nil
	})
	return n, errors.Join(append(errs, err)...)
}

// ValidSHA reports whether s is 64 lowercase hex digits, the only form
// that is ever turned into a cache path.
func ValidSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func (c *Cache) path(sha string, size int) string {
	return filepath.Join(c.cfg.Dir, sha[:2], fmt.Sprintf("%s-%d.jpg", sha, size))
}

// Get returns the path of src's cached JPEG at size, rendering it first
// when needed. Concurrent calls for the same file and size share one
// render, which is cancelled only once every caller has gone. At most
// Workers renders run at once.
func (c *Cache) Get(ctx context.Context, src Source, size int) (string, error) {
	if !ValidSHA(src.SHA256) {
		return "", fmt.Errorf("%w: %q", ErrBadSHA, src.SHA256)
	}
	if size != ThumbSize && size != PreviewSize {
		return "", fmt.Errorf("%w: %d", ErrBadSize, size)
	}
	dst := c.path(src.SHA256, size)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("thumbnail cache: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", ErrClosed
	}
	cl := c.calls[dst]
	if cl == nil {
		// The timeout starts in generate, once the render has a worker
		// slot: time spent queued behind other renders doesn't count.
		rctx, cancel := context.WithCancel(c.ctx)
		cl = &call{done: make(chan struct{}), cancel: cancel}
		c.calls[dst] = cl
		c.wg.Add(1)
		go c.run(rctx, cl, src, size, dst)
	}
	cl.waiters++
	c.mu.Unlock()

	select {
	case <-cl.done:
		if cl.err != nil {
			return "", cl.err
		}
		return dst, nil
	case <-ctx.Done():
		c.mu.Lock()
		cl.waiters--
		if cl.waiters == 0 {
			// Nobody wants it any more: stop the render, and let the next
			// Get start afresh instead of joining a cancelled one.
			cl.cancel()
			if c.calls[dst] == cl {
				delete(c.calls, dst)
			}
		}
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

func (c *Cache) run(ctx context.Context, cl *call, src Source, size int, dst string) {
	defer c.wg.Done()
	err := c.generate(ctx, src, size, dst)
	cl.cancel()
	c.mu.Lock()
	if c.calls[dst] == cl {
		delete(c.calls, dst)
	}
	c.mu.Unlock()
	cl.err = err
	close(cl.done)
}

func (c *Cache) generate(ctx context.Context, src Source, size int, dst string) error {
	if _, err := os.Stat(src.Path); err != nil {
		return err // fs.ErrNotExist reaches the caller as is
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout) // the per-render timeout
	defer cancel()
	img, err := c.render(ctx, src)
	if err != nil {
		return fmt.Errorf("%s: %w", src.Path, err)
	}
	if err := ctx.Err(); err != nil {
		return err // cancelled or timed out while decoding in Go
	}
	return writeJPEG(dst, imaging.Fit(img, size, size, imaging.Lanczos), quality(size))
}

func quality(size int) int {
	if size == ThumbSize {
		return 80
	}
	return 85
}

// writeJPEG encodes img to a temporary file next to dst and renames it
// into place, so a reader never sees a half-written thumbnail.
func writeJPEG(dst string, img image.Image, quality int) (err error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rmErr := os.Remove(f.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	if err = jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

// renderSource decodes src into an upright image of any size. Task 6 adds
// the raw and video cases.
func (c *Cache) renderSource(ctx context.Context, src Source) (image.Image, error) {
	switch src.Kind {
	case media.KindImage:
		return decodeImage(src.Path)
	}
	return nil, fmt.Errorf("no renderer for kind %q", src.Kind)
}

// decodeImage opens path, rejects images over maxPixels from their header
// alone, and then decodes it upright.
func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := checkPixels(f); err != nil {
		return nil, err
	}
	return imaging.Decode(f, imaging.AutoOrientation(true))
}

// checkPixels reads r's image header, returns ErrTooLarge when it declares
// more than maxPixels pixels, and seeks r back to the start. Every decode of
// an untrusted image in this package goes through it first.
func checkPixels(r io.ReadSeeker) error {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return err
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return fmt.Errorf("%w: %dx%d", ErrTooLarge, cfg.Width, cfg.Height)
	}
	_, err = r.Seek(0, io.SeekStart)
	return err
}

// Close cancels running renders, waits for them, and makes later Gets fail
// with ErrClosed.
func (c *Cache) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}
