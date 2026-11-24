package main

import (
	"errors"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/lockfile"
)

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("x")); got != 1 {
		t.Errorf("exitCode(plain error) = %d, want 1", got)
	}
	e := &configInUseError{msg: "m", err: &lockfile.HeldError{}}
	if got := exitCode(e); got != 3 {
		t.Errorf("exitCode(configInUseError) = %d, want 3", got)
	}
	if e.Error() != "m" {
		t.Errorf("Error() = %q, want %q", e.Error(), "m")
	}
}
