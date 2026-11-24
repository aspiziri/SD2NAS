package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ctxReader returns ctx's error before each Read.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// hashFile is media.HashFile that stops when ctx ends (a 4 GB video must
// not hold up shutdown), reading sequentially in 1 MiB blocks.
func hashFile(ctx context.Context, path string) (sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("hash %s: %w", path, cerr))
		}
	}()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, 1<<20)); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
