package lockfile

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sd2nas.lock")
	const info = "pid 1 on test since x"

	l, err := Acquire(p, info)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if first, _, _ := strings.Cut(string(b), "\n"); first != info {
		t.Fatalf("first line = %q, want %q", first, info)
	}

	l2, err := Acquire(p, "other")
	if err == nil {
		l2.Release()
		t.Fatal("second Acquire succeeded while the lock was held")
	}
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire error = %v, want ErrHeld", err)
	}
	var he *HeldError
	if !errors.As(err, &he) {
		t.Fatalf("error %v is not a *HeldError", err)
	}
	if he.Holder != info {
		t.Fatalf("Holder = %q, want %q", he.Holder, info)
	}
	if he.Path != p {
		t.Fatalf("Path = %q, want %q", he.Path, p)
	}
	if want := p + " is locked by another process (" + info + ")"; he.Error() != want {
		t.Fatalf("Error() = %q, want %q", he.Error(), want)
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("lock file gone after Release: %v", err)
	}
	l3, err := Acquire(p, "third")
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l3.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("lock file gone after Release: %v", err)
	}
}

func TestHeldErrorUnknownHolder(t *testing.T) {
	e := &HeldError{Path: "p"}
	if want := "p is locked by another process (unknown)"; e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestHeldAcrossProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sd2nas.lock")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "SD2NAS_LOCKFILE_HELPER="+p)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			stdin.Close()
			cmd.Process.Kill()
			cmd.Wait()
		}
	})

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(10 * time.Second)
	for locked := false; !locked; {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("helper exited before printing locked")
			}
			locked = line == "locked"
		case <-deadline:
			t.Fatal("helper never printed locked")
		}
	}

	l, err := Acquire(p, "parent")
	if err == nil {
		l.Release()
		t.Fatal("Acquire succeeded while another process held the lock")
	}
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire error = %v, want ErrHeld", err)
	}
	var he *HeldError
	if !errors.As(err, &he) || !strings.HasPrefix(he.Holder, "helper ") {
		t.Fatalf("error %v: want a HeldError whose holder starts with \"helper \"", err)
	}

	stdin.Close()
	waited = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	// Windows releases a dead process's locks asynchronously.
	end := time.Now().Add(10 * time.Second)
	for {
		l, err = Acquire(p, "parent again")
		if err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("Acquire after the holder exited: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestHelperHoldLock is the child process of TestHeldAcrossProcesses.
func TestHelperHoldLock(t *testing.T) {
	p := os.Getenv("SD2NAS_LOCKFILE_HELPER")
	if p == "" {
		return
	}
	l, err := Acquire(p, "helper "+filepath.Base(p))
	if err != nil {
		t.Fatalf("helper Acquire: %v", err)
	}
	defer l.Release()
	if _, err := os.Stdout.WriteString("locked\n"); err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(os.Stdin).ReadByte() // blocks until the parent closes stdin
}

func TestAcquireMissingDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "sd2nas.lock")
	l, err := Acquire(p, "x")
	if err == nil {
		l.Release()
		t.Fatal("Acquire in a missing directory succeeded")
	}
	if errors.Is(err, ErrHeld) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("error %v should be neither ErrHeld nor ErrUnsupported", err)
	}
}
