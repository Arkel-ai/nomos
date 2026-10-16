package utils

// External CLI invocation, hardened. Nomos shells out to poppler
// (pdftotext / pdftoppm), tesseract and pandoc — see docs/PLAN.md §7.2/§7.3.
// Every one of those calls goes through RunTool, which enforces the rules the
// plan sets out: exec.CommandContext with an ARGUMENT ARRAY (never a shell
// string built from a filename), a per-job temporary directory that is always
// removed, a hard timeout, and strict input/output size caps.
//
// Errors are typed so a service can tell "the binary is not installed in this
// image" (→ 503 "<tool> not available") apart from "the binary ran and
// failed" (→ 502/422 with the tool's own diagnostic).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var execLog = NewLogger("utils.exec")

// Defaults applied when a ToolSpec leaves the corresponding field zero.
const (
	defaultToolTimeout   = 120 * time.Second
	defaultMaxOutput     = 64 << 20  // 64 MiB of stdout
	defaultMaxInputBytes = 256 << 20 // 256 MiB per input file
	maxStderrKept        = 8 << 10   // stderr excerpt attached to errors
	toolWaitDelay        = 5 * time.Second
)

// Failure classes. Callers switch on these to pick an HTTP status.
var (
	// ErrToolMissing — the executable is not on PATH. This is a deployment
	// problem (the image lacks poppler/tesseract/pandoc), not a bad input:
	// degrade to 503 with a clear detail message.
	ErrToolMissing = errors.New("tool not available")
	// ErrToolFailed — the executable ran and exited non-zero.
	ErrToolFailed = errors.New("tool failed")
	// ErrToolTimeout — the hard timeout (or the caller's context) fired.
	ErrToolTimeout = errors.New("tool timed out")
	// ErrOutputTooLarge — stdout exceeded MaxOutput; the child was killed.
	ErrOutputTooLarge = errors.New("tool output too large")
	// ErrInputTooLarge — an input file (or stdin) exceeded MaxInputBytes.
	ErrInputTooLarge = errors.New("tool input too large")
	// ErrInvalidToolSpec — a malformed spec (empty name, NUL in an
	// argument). Never reachable from well-formed callers; it exists so a
	// crafted filename cannot smuggle anything into the argv array.
	ErrInvalidToolSpec = errors.New("invalid tool spec")
)

// ToolSpec describes one external command invocation.
type ToolSpec struct {
	// Name is the executable, resolved through PATH (e.g. "pdftotext").
	Name string
	// Args is the argument array. It is passed to the kernel as-is: no
	// shell, no globbing, no word splitting. Filenames belong here.
	Args []string
	// Dir is the working directory. When empty, RunTool creates a private
	// per-job temp directory and removes it (recursively) on return.
	Dir string
	// Stdin, when non-nil, is fed to the child's standard input.
	Stdin []byte
	// InputPaths are files the tool will read. Each is stat'ed against
	// MaxInputBytes before the child starts, so an oversized document is
	// rejected without spending CPU on it.
	InputPaths []string
	// Timeout is the hard wall-clock limit (default 120s).
	Timeout time.Duration
	// MaxOutput caps stdout in bytes (default 64 MiB).
	MaxOutput int64
	// MaxInputBytes caps each entry of InputPaths and len(Stdin)
	// (default 256 MiB).
	MaxInputBytes int64
	// Env replaces the child environment entirely when non-nil. Leave nil
	// to inherit the API process environment.
	Env []string
}

// ToolError carries the failure class plus enough context to log and to build
// a user-facing detail string. The tool's own stderr is truncated and kept —
// it is a diagnostic, and poppler/tesseract/pandoc do not echo document text
// there — but callers must still not forward it verbatim to a client.
type ToolError struct {
	Tool     string
	Kind     error // one of the Err* sentinels above
	ExitCode int
	Stderr   string
	Err      error
}

func (e *ToolError) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Tool, e.Kind)
	if e.ExitCode != 0 {
		msg += fmt.Sprintf(" (exit %d)", e.ExitCode)
	}
	if e.Stderr != "" {
		msg += ": " + strings.TrimSpace(e.Stderr)
	}
	return msg
}

// Unwrap exposes the sentinel so errors.Is(err, ErrToolMissing) works.
func (e *ToolError) Unwrap() error { return e.Kind }

// IsToolMissing reports whether err means "the binary is not installed".
func IsToolMissing(err error) bool { return errors.Is(err, ErrToolMissing) }

// RunTool executes spec and returns the child's stdout.
//
// It never builds a shell command line: the executable is resolved with
// exec.LookPath and the arguments are handed over as an array, so a filename
// containing spaces, quotes, semicolons or $(…) is inert.
func RunTool(ctx context.Context, spec ToolSpec) ([]byte, error) {
	if spec.Name == "" {
		return nil, &ToolError{Tool: "<none>", Kind: ErrInvalidToolSpec}
	}
	for _, a := range spec.Args {
		if strings.ContainsRune(a, 0) {
			return nil, &ToolError{Tool: spec.Name, Kind: ErrInvalidToolSpec}
		}
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	maxOutput := spec.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	maxInput := spec.MaxInputBytes
	if maxInput <= 0 {
		maxInput = defaultMaxInputBytes
	}

	if int64(len(spec.Stdin)) > maxInput {
		return nil, &ToolError{Tool: spec.Name, Kind: ErrInputTooLarge}
	}
	for _, p := range spec.InputPaths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, &ToolError{Tool: spec.Name, Kind: ErrInvalidToolSpec, Err: err}
		}
		if info.Size() > maxInput {
			return nil, &ToolError{Tool: spec.Name, Kind: ErrInputTooLarge}
		}
	}

	// "tool missing" is decided before we spend a process on it, so the
	// caller gets a 503 instead of an opaque exec error.
	bin, err := exec.LookPath(spec.Name)
	if err != nil {
		return nil, &ToolError{Tool: spec.Name, Kind: ErrToolMissing, Err: err}
	}

	dir := spec.Dir
	if dir == "" {
		jobDir, cleanup, err := JobDir(spec.Name)
		if err != nil {
			return nil, &ToolError{Tool: spec.Name, Kind: ErrToolFailed, Err: err}
		}
		defer cleanup()
		dir = jobDir
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, spec.Args...)
	cmd.Dir = dir
	cmd.Env = spec.Env
	// Give a killed child a moment to die before Wait returns, so we never
	// leak a tesseract that ignored SIGKILL's predecessor.
	cmd.WaitDelay = toolWaitDelay
	if spec.Stdin != nil {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}

	stdout := &cappedBuffer{limit: maxOutput}
	stderr := &cappedBuffer{limit: maxStderrKept}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	if stdout.exceeded {
		execLog.Warnf("Tool %s produced more than %d bytes on stdout — killed", spec.Name, maxOutput)
		return nil, &ToolError{Tool: spec.Name, Kind: ErrOutputTooLarge, Stderr: stderr.String()}
	}
	if runErr != nil {
		if ctx.Err() != nil {
			execLog.Warnf("Tool %s timed out after %s (args=%d)", spec.Name, elapsed.Round(time.Millisecond), len(spec.Args))
			return nil, &ToolError{Tool: spec.Name, Kind: ErrToolTimeout, Stderr: stderr.String(), Err: ctx.Err()}
		}
		code := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		execLog.Warnf("Tool %s failed (exit=%d, %s)", spec.Name, code, elapsed.Round(time.Millisecond))
		return nil, &ToolError{Tool: spec.Name, Kind: ErrToolFailed, ExitCode: code, Stderr: stderr.String(), Err: runErr}
	}

	execLog.Debugf("Tool %s ok (args=%d, stdout=%dB, %s)", spec.Name, len(spec.Args), stdout.Len(), elapsed.Round(time.Millisecond))
	return stdout.Bytes(), nil
}

// JobDir creates a private per-job temporary directory under the API's tmp
// root and returns it with a cleanup function that removes it recursively.
// Steps that must stage inputs and collect outputs (pdftoppm writing page
// images, pandoc reading a reference doc) use it and pass the directory to
// RunTool as ToolSpec.Dir.
//
//	dir, cleanup, err := utils.JobDir("ocr")
//	if err != nil { … }
//	defer cleanup()
func JobDir(prefix string) (string, func(), error) {
	root := filepath.Join(TmpRoot(), "jobs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, err
	}
	dir, err := os.MkdirTemp(root, sanitizeSegment(prefix)+"-")
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() {
		if err := os.RemoveAll(dir); err != nil {
			execLog.Warnf("Job dir cleanup failed: %v", err)
		}
	}, nil
}

// cappedBuffer is an io.Writer that stops accepting bytes past limit and
// records that it happened. Once exceeded it returns an error, which makes
// exec kill the child instead of letting it fill memory.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.exceeded {
		return 0, ErrOutputTooLarge
	}
	if int64(b.buf.Len())+int64(len(p)) > b.limit {
		remaining := b.limit - int64(b.buf.Len())
		if remaining > 0 {
			b.buf.Write(p[:remaining])
		}
		b.exceeded = true
		return 0, ErrOutputTooLarge
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) Len() int       { return b.buf.Len() }
func (b *cappedBuffer) String() string { return b.buf.String() }

// compile-time assertion: cappedBuffer is a valid exec Stdout/Stderr sink.
var _ io.Writer = (*cappedBuffer)(nil)
