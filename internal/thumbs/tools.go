package thumbs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"
)

// maxToolOutput caps what is read from exiftool or ffmpeg: a full-size
// JpgFromRaw of a 61 MP body is ~20 MB and ~27 MB as base64. It is a var
// so a test can lower it.
var maxToolOutput = 128 << 20

// errTooLarge is wrapped by runTool when the output passes maxToolOutput.
var errTooLarge = errors.New("output is larger than the cap")

// capBuffer keeps up to max bytes and drops the rest, so a chatty tool is
// never blocked on a full pipe.
type capBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// runTool runs name with args (never through a shell) and returns stdout.
// The context's deadline kills it. Its stderr goes into the error, or is
// logged when it exits 0.
func (c *Cache) runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	tool := filepath.Base(name)
	cmd := exec.CommandContext(ctx, name, args...)
	stdout := &capBuffer{max: maxToolOutput}
	stderr := &capBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second // don't hang on pipes held open by a killed tool's children
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.buf.String())
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s: %w", tool, ctxErr)
	}
	if err != nil {
		// A tool that ran and exited non-zero rejected the file. A missing
		// binary or a failure to start is not the file's fault.
		if errors.As(err, new(*exec.ExitError)) {
			err = fmt.Errorf("%w: %w", ErrUndecodable, err)
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", tool, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%s: %w (%d bytes)", tool, errTooLarge, maxToolOutput)
	}
	if msg != "" {
		c.cfg.Log.Warn("tool wrote to stderr", "tool", tool, "args", args, "stderr", msg)
	}
	return stdout.buf.Bytes(), nil
}

// decodeUntrusted decodes image bytes a tool produced, refusing images over
// maxPixels from the header before decoding. It does not auto-orient.
func decodeUntrusted(data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	if err := checkPixels(r); err != nil {
		return nil, undecodable(err)
	}
	img, err := imaging.Decode(r)
	if err != nil {
		return nil, undecodable(err)
	}
	return img, nil
}

// rawPreview returns the RAW file's embedded preview (falling back to
// JpgFromRaw), turned upright with the RAW's own Orientation tag.
func (c *Cache) rawPreview(ctx context.Context, path string) (image.Image, error) {
	for _, tag := range []string{"PreviewImage", "JpgFromRaw"} {
		out, err := c.runTool(ctx, c.cfg.Exiftool, "-json", "-b", "-n", "-Orientation", "-"+tag, "--", path)
		if err != nil {
			return nil, err
		}
		var res []map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, fmt.Errorf("%w: exiftool output: %w", ErrUndecodable, err)
		}
		if len(res) != 1 {
			return nil, fmt.Errorf("%w: exiftool output: %d results, want 1", ErrUndecodable, len(res))
		}
		enc, _ := res[0][tag].(string) // absent when the file has no such image
		if enc == "" {
			continue
		}
		b64, ok := strings.CutPrefix(enc, "base64:")
		if !ok {
			return nil, fmt.Errorf("%w: exiftool output: %s is not base64 binary", ErrUndecodable, tag)
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("%w: exiftool output: %s: %w", ErrUndecodable, tag, err)
		}
		img, err := decodeUntrusted(data) // no auto-orientation: the RAW's tag rules
		if err != nil {
			return nil, fmt.Errorf("decode embedded %s: %w", tag, err)
		}
		o, _ := res[0]["Orientation"].(float64) // -n makes it a number; absent means upright
		return orient(img, int(o)), nil
	}
	return nil, ErrNoImage
}

// videoFrame returns the frame at 1 s, or at 0 s for a clip shorter than
// that. Past the end of a short clip, ffmpeg 6.x exits 0 and writes
// nothing, while ffmpeg 7+ exits non-zero ("Nothing was written into
// output file"), so either result at 1 s means: try 0 s. ffmpeg applies
// rotation metadata itself. The "file:" prefix stops a path from being
// read as a protocol.
func (c *Cache) videoFrame(ctx context.Context, path string) (image.Image, error) {
	var first error // the failure at 1 s, reported with the one at 0 s
	for _, at := range []string{"1", "0"} {
		out, err := c.runTool(ctx, c.cfg.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
			"-ss", at, "-i", "file:"+path, "-frames:v", "1", "-an", "-sn", "-dn",
			"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "2", "pipe:1")
		if err != nil {
			// A cancelled or timed-out render, runaway output or a missing
			// ffmpeg is not a short clip: don't run ffmpeg again.
			if at == "1" && ctx.Err() == nil && !errors.Is(err, errTooLarge) && !errors.Is(err, exec.ErrNotFound) {
				first = err
				continue
			}
			if first != nil {
				return nil, errors.Join(first, err)
			}
			return nil, err
		}
		if len(out) == 0 {
			continue
		}
		img, err := decodeUntrusted(out)
		if err != nil {
			return nil, fmt.Errorf("decode video frame: %w", err)
		}
		return img, nil
	}
	if first != nil {
		return nil, errors.Join(ErrNoImage, first) // still errors.Is ErrNoImage
	}
	return nil, ErrNoImage
}

// orient turns img upright for EXIF orientation o (1-8; anything else is
// returned as is), the same way imaging.AutoOrientation does.
func orient(img image.Image, o int) image.Image {
	switch o {
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.Transpose(img)
	case 6:
		return imaging.Rotate270(img)
	case 7:
		return imaging.Transverse(img)
	case 8:
		return imaging.Rotate90(img)
	}
	return img
}

// MissingTools describes each external tool that can't be found.
func (c *Cache) MissingTools() []string {
	var out []string
	for _, t := range []struct{ cmd, what string }{
		{c.cfg.Exiftool, "RAW thumbnails"},
		{c.cfg.FFmpeg, "video thumbnails"},
	} {
		if _, err := exec.LookPath(t.cmd); err != nil {
			out = append(out, fmt.Sprintf("%s not found: %s are unavailable", filepath.Base(t.cmd), t.what))
		}
	}
	return out
}
