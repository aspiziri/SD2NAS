package thumbs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// errStopped means exiftool's output ended before the answer did: the
// process exited (or its pipes broke) in the middle of a request.
var errStopped = errors.New("stopped before answering")

// exifServer keeps one "exiftool -stay_open True -@ -" process and runs
// one request at a time through it. Any doubt about the stream (timeout,
// cancel, overflow, a write or read error, the process exiting) kills the
// process; the next request starts a new one.
type exifServer struct {
	cmd    string
	log    *slog.Logger
	lock   chan struct{} // cap 1: held for a request, a start or close
	proc   *exifProc     // nil before the first request and after a kill
	seq    int
	closed bool
	starts int // for tests

	afterWrite func()          // tests: runs once a request is written
	onStart    func(*exifProc) // tests: runs after each start
}

// pipeDrain bounds the wait, after a kill, for a request's readers to see
// the end of exiftool's output; past it the pipes are closed under them
// (only a child of exiftool still holding them open would need that).
const pipeDrain = 5 * time.Second

// exifProc is one running exiftool.
type exifProc struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
	stdout     *bufio.Reader
	stderr     *bufio.Reader
	reaping    bool          // reap has started Wait; guarded by the server's lock
	exited     chan struct{} // closed once Wait has returned
	waitErr    error         // Wait's result; read it after exited is closed
}

// exifReply is what one reader goroutine of a request got.
type exifReply struct {
	stderr bool // from stderr (else stdout)
	data   []byte
	err    error
}

func newExifServer(cmd string, log *slog.Logger) *exifServer {
	return &exifServer{cmd: cmd, log: log, lock: make(chan struct{}, 1)}
}

// run sends args (one exiftool command, no newline in any of them) and
// returns the command's stdout and its trimmed stderr. Errors from the
// server itself (a failed start, a broken stream) are not ErrUndecodable;
// judging the answer is the caller's job.
func (e *exifServer) run(ctx context.Context, args []string) (stdout []byte, stderr string, err error) {
	select {
	case e.lock <- struct{}{}:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The render's whole budget went on waiting behind other files.
			e.log.Warn("RAW preview timed out waiting for exiftool, busy with other files")
			return nil, "", fmt.Errorf("exiftool busy with other files: %w", ctx.Err())
		}
		return nil, "", ctx.Err()
	}
	defer func() { <-e.lock }()
	if e.closed {
		return nil, "", ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err // don't touch the process for a request nobody wants
	}
	if e.proc == nil {
		if err := e.start(); err != nil {
			return nil, "", err
		}
	}
	p := e.proc
	e.seq++
	marker := fmt.Sprintf("{ready%d}", e.seq)

	// Unless this request ends cleanly (both markers read, nothing left
	// unread), the stream can't be trusted: kill the process, so that a
	// panic or an early return never leaves a live process with an unread
	// reply for the next request to take as its own.
	replies := make(chan exifReply, 2)
	pending := 0
	clean := false
	defer func() {
		if !clean {
			e.kill(p)
			e.finish(p, replies, &pending, nil)
		}
	}()

	var req strings.Builder
	for _, a := range args {
		req.WriteString(a)
		req.WriteByte('\n')
	}
	fmt.Fprintf(&req, "-echo4\n%s\n-execute%d\n", marker, e.seq)
	if _, err := io.WriteString(p.stdin, req.String()); err != nil {
		return nil, "", fmt.Errorf("exiftool: send request: %w", err)
	}
	if e.afterWrite != nil {
		e.afterWrite()
	}
	limit := maxToolOutput
	pending = 2
	go func() {
		data, err := readReply(p.stdout, marker, limit, true)
		replies <- exifReply{data: data, err: err}
	}()
	go func() {
		data, err := readReply(p.stderr, marker, 4<<10, false)
		replies <- exifReply{stderr: true, data: data, err: err}
	}()

	var out, errOut []byte
	for pending > 0 {
		select {
		case r := <-replies:
			pending--
			if r.stderr {
				errOut = r.data
			} else {
				out = r.data
			}
			if r.err != nil {
				return nil, "", e.failure(p, r.err, replies, &pending, errOut)
			}
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	clean = true
	return out, strings.TrimSpace(string(errOut)), nil
}

// failure kills p after a reader failed with err, waits for the other
// reader and for p to be reaped, and describes the failure with what
// exiftool wrote to stderr and its exit status.
func (e *exifServer) failure(p *exifProc, err error, replies <-chan exifReply, pending *int, errOut []byte) error {
	e.kill(p)
	e.finish(p, replies, pending, func(r exifReply) {
		if r.stderr {
			errOut = r.data
		}
	})
	err = fmt.Errorf("exiftool: %w", err)
	if errors.Is(err, errStopped) && p.waitErr != nil {
		err = fmt.Errorf("%w (%v)", err, p.waitErr)
	}
	if msg := strings.TrimSpace(string(errOut)); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// start launches the process. Called with the lock held.
func (e *exifServer) start() error {
	// No WaitDelay: with all three pipes from Std*Pipe there are no copying
	// goroutines for it to bound, and Wait (run only once the readers are
	// done, see reap) returns as soon as exiftool has exited.
	cmd := exec.Command(e.cmd, "-stay_open", "True", "-@", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("exiftool: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("exiftool: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("exiftool: %w", err)
	}
	if err := cmd.Start(); err != nil { // closes the pipes when it fails
		return fmt.Errorf("exiftool: %w", err)
	}
	p := &exifProc{cmd: cmd, stdin: stdin, stdoutPipe: stdout, stderrPipe: stderr,
		stdout: bufio.NewReaderSize(stdout, 64<<10), stderr: bufio.NewReader(stderr),
		exited: make(chan struct{})}
	e.proc = p
	e.starts++
	if e.onStart != nil {
		e.onStart(p)
	}
	return nil
}

// kill stops p and forgets it; finish then waits for its readers and
// reaps it. Called with the lock held; killing a process that is already
// gone is harmless.
func (e *exifServer) kill(p *exifProc) {
	// Kill fails for a process that has already exited (on Windows also
	// before it is reaped), and closing stdin can only fail for a pipe
	// already broken: neither stops the process from going.
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		e.log.Debug("kill exiftool", "err", err)
	}
	if err := p.stdin.Close(); err != nil {
		e.log.Debug("close exiftool's stdin", "err", err)
	}
	if e.proc == p {
		e.proc = nil
	}
}

// finish waits for a request's pending readers of p (passing each reply to
// got, when not nil) and then reaps p. Wait closes the read ends of the
// pipes, so it runs only once the readers are done: a tool dying mid-request
// keeps its last words on stderr. Called with the lock held, after kill.
func (e *exifServer) finish(p *exifProc, replies <-chan exifReply, pending *int, got func(exifReply)) {
	if *pending > 0 {
		timer := time.NewTimer(pipeDrain)
		defer timer.Stop()
		for *pending > 0 {
			select {
			case r := <-replies:
				*pending--
				if got != nil {
					got(r)
				}
			case <-timer.C:
				e.log.Warn("exiftool's output stayed open after it was killed: closing it")
				for _, c := range []io.Closer{p.stdoutPipe, p.stderrPipe} {
					if err := c.Close(); err != nil {
						e.log.Debug("close exiftool's output", "err", err)
					}
				}
			}
		}
	}
	e.reap(p)
	<-p.exited
}

// reap starts Wait for p, once. Called with the lock held, when no reader
// of p is running.
func (e *exifServer) reap(p *exifProc) {
	if p.reaping {
		return
	}
	p.reaping = true
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.exited)
	}()
}

// close asks the process to exit, waits at most 1 s, and kills it if it
// is still running. Later requests fail with ErrClosed. Call it once no
// request can be running (Cache.Close does, after its renders end).
func (e *exifServer) close() {
	e.lock <- struct{}{}
	defer func() { <-e.lock }()
	if e.closed {
		return
	}
	e.closed = true
	p := e.proc
	if p == nil {
		return
	}
	_, err := io.WriteString(p.stdin, "-stay_open\nFalse\n")
	if cerr := p.stdin.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.log.Warn("stop exiftool: killing it", "err", err)
		e.kill(p)
		e.reap(p)
		<-p.exited
		return
	}
	e.reap(p) // no request is running, so nothing reads its pipes
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-p.exited:
		e.proc = nil
		if p.waitErr != nil {
			// exiftool's exit status says some earlier command had an
			// error; each of those was already answered as a failed render.
			e.log.Debug("exiftool stopped", "err", p.waitErr)
		}
	case <-timer.C:
		e.log.Warn("exiftool did not stop within 1 s: killing it")
		e.kill(p)
		<-p.exited
	}
}

// readReply reads r up to a line equal to marker (ended by "\n" or
// "\r\n") that is the last thing read, and returns what came before it.
// The marker counts only at the start of a line. Past limit bytes, a
// strict read fails with errTooLarge; otherwise it keeps the first limit
// bytes and reads on. A {ready<M>} line for another request, or output
// already waiting after the marker, is an error: the stream is out of
// step.
func readReply(r *bufio.Reader, marker string, limit int, strict bool) ([]byte, error) {
	var buf []byte
	lineStart := true
	for {
		chunk, err := r.ReadSlice('\n')
		if lineStart && err == nil {
			line := bytes.TrimSuffix(bytes.TrimSuffix(chunk, []byte("\n")), []byte("\r"))
			if string(line) == marker {
				if n := r.Buffered(); n > 0 {
					return buf, fmt.Errorf("%d bytes after %s", n, marker)
				}
				return buf, nil
			}
			if isReadyLine(line) {
				return buf, fmt.Errorf("read %s while waiting for %s", line, marker)
			}
		}
		if room := limit - len(buf); len(chunk) > room {
			if strict {
				return buf, fmt.Errorf("%w (%d bytes)", errTooLarge, limit)
			}
			buf = append(buf, chunk[:max(room, 0)]...)
		} else {
			buf = append(buf, chunk...)
		}
		switch {
		case err == nil:
			lineStart = true
		case errors.Is(err, bufio.ErrBufferFull):
			lineStart = false
		default:
			return buf, fmt.Errorf("%w: %w", errStopped, err)
		}
	}
}

// isReadyLine reports whether line is a {ready} or {ready<N>} marker.
func isReadyLine(line []byte) bool {
	n, ok := bytes.CutPrefix(line, []byte("{ready"))
	if !ok {
		return false
	}
	n, ok = bytes.CutSuffix(n, []byte("}"))
	if !ok {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
