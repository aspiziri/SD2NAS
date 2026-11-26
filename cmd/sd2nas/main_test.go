package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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

	t.Run("bad token", func(t *testing.T) {
		if os.Getenv("SD2NAS_TEST_RUN_MAIN") == "1" {
			os.Args = os.Args[:1] // no test flags for config.Load
			main()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExitCode$/^bad_token$")
		// Should the token check ever be missing, run gets no further than a
		// failed listen, inside temp folders.
		dir := t.TempDir()
		cmd.Env = append(os.Environ(), "SD2NAS_TEST_RUN_MAIN=1", "SD2NAS_TOKEN=a b",
			"SD2NAS_ADDR=127.0.0.1:bad", "SD2NAS_CARDS="+dir, "SD2NAS_PHOTOS="+dir, "SD2NAS_CONFIG="+dir)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("exit: %v, want exit status 1 (output %s)", err, out)
		}
		want := `msg=exiting err="SD2NAS_TOKEN may use only letters, digits and . _ ~ + / = -"`
		if !strings.Contains(string(out), want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	})
}

func TestTokenWarning(t *testing.T) {
	const want = "SD2NAS_TOKEN is shorter than 16 characters: pick a longer, random one"
	for _, tc := range []struct{ token, want string }{
		{"", ""},
		{"a", want},
		{"0123456789abcde", want},
		{"0123456789abcdef", ""},
		{"0f3a9c41d2e8b7a6f5c4d3e2b1a0f9e8d7c6b5a4f3e2d1c0", ""},
	} {
		if got := tokenWarning(tc.token); got != tc.want {
			t.Errorf("tokenWarning(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}
