package utils

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestBlob returns a local backend rooted in t.TempDir(), so tests never
// touch api/tmp.
func newTestBlob(t *testing.T) *localBlob {
	t.Helper()
	root := t.TempDir()
	SetTmpRoot(root)
	t.Cleanup(func() { SetTmpRoot("tmp") })
	return &localBlob{root: filepath.Join(root, "blobs")}
}

func TestBlobLocalRoundTrip(t *testing.T) {
	b := newTestBlob(t)
	ctx := context.Background()
	key := DossierKey("tenant-1", "dossier-9", "contrat de travail.pdf")

	if !strings.HasPrefix(key, BlobPrefix()) {
		t.Fatalf("key %q is not under the app prefix %q", key, BlobPrefix())
	}
	if strings.Contains(key, " ") {
		t.Fatalf("key %q was not sanitised", key)
	}

	payload := []byte("%PDF-1.7 fake bytes")
	if err := b.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "application/pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get returned %q, want %q", got, payload)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Get(ctx, key); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrBlobNotFound", err)
	}
	// Deleting twice is not an error — retries must converge (§4.3).
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestBlobPrefixGuard(t *testing.T) {
	b := newTestBlob(t)
	ctx := context.Background()

	bad := []string{
		"",
		"other-app/tenant/file.pdf",
		BlobPrefix() + "../../etc/passwd",
		BlobPrefix() + "tenant/../../escape.pdf",
		BlobPrefix() + "tenant//double.pdf",
	}
	for _, key := range bad {
		if err := b.Put(ctx, key, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrBlobKeyForbidden) {
			t.Errorf("Put(%q) = %v, want ErrBlobKeyForbidden", key, err)
		}
		if _, err := b.Get(ctx, key); !errors.Is(err, ErrBlobKeyForbidden) {
			t.Errorf("Get(%q) = %v, want ErrBlobKeyForbidden", key, err)
		}
		if _, err := b.SignedURL(ctx, key, time.Minute); !errors.Is(err, ErrBlobKeyForbidden) {
			t.Errorf("SignedURL(%q) = %v, want ErrBlobKeyForbidden", key, err)
		}
	}
	// Nothing escaped the root.
	if _, err := os.Stat(filepath.Join(b.root, "..", "..", "etc", "passwd")); err == nil {
		t.Fatal("a traversal key wrote outside the blob root")
	}
}

func TestBlobSignedURL(t *testing.T) {
	b := newTestBlob(t)
	ctx := context.Background()
	key := DossierKey("t1", "d1", "bordereau.docx")

	raw, err := b.SignedURL(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("SignedURL returned an unparseable URL %q: %v", raw, err)
	}
	q := parsed.Query()
	if err := VerifyBlobSignature(key, q.Get("expires"), q.Get("signature")); err != nil {
		t.Fatalf("VerifyBlobSignature on a fresh URL: %v", err)
	}
	if err := VerifyBlobSignature(key, q.Get("expires"), "deadbeef"); !errors.Is(err, ErrBlobBadSignature) {
		t.Fatalf("tampered signature accepted: %v", err)
	}
	if err := VerifyBlobSignature("app/other/key", q.Get("expires"), q.Get("signature")); !errors.Is(err, ErrBlobBadSignature) {
		t.Fatalf("signature replayed onto another key: %v", err)
	}
	if err := VerifyBlobSignature(key, "1", q.Get("signature")); !errors.Is(err, ErrBlobBadSignature) {
		t.Fatalf("expired URL accepted: %v", err)
	}
}

func TestRunToolSuccess(t *testing.T) {
	newTestBlob(t) // pins TmpRoot so job dirs land in t.TempDir()

	out, err := RunTool(context.Background(), ToolSpec{
		Name: "cat",
		// The argument array is passed through verbatim: this "filename"
		// would be catastrophic through a shell and is inert here.
		Stdin:   []byte("hello; rm -rf /"),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunTool(cat): %v", err)
	}
	if string(out) != "hello; rm -rf /" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunToolJobDirIsCleanedUp(t *testing.T) {
	newTestBlob(t)

	var seen string
	// pwd prints the working directory RunTool created for the job.
	out, err := RunTool(context.Background(), ToolSpec{Name: "pwd", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("RunTool(pwd): %v", err)
	}
	seen = strings.TrimSpace(string(out))
	if seen == "" {
		t.Fatal("pwd printed nothing")
	}
	if _, err := os.Stat(seen); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job dir %q survived the call (%v)", seen, err)
	}
}

func TestRunToolMissingBinary(t *testing.T) {
	newTestBlob(t)

	_, err := RunTool(context.Background(), ToolSpec{Name: "nomos-definitely-not-installed"})
	if !IsToolMissing(err) {
		t.Fatalf("err = %v, want ErrToolMissing", err)
	}
	var te *ToolError
	if !errors.As(err, &te) || te.Tool != "nomos-definitely-not-installed" {
		t.Fatalf("expected a *ToolError naming the tool, got %#v", err)
	}
}

func TestRunToolFailureIsDistinctFromMissing(t *testing.T) {
	newTestBlob(t)

	_, err := RunTool(context.Background(), ToolSpec{Name: "false", Timeout: 10 * time.Second})
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("err = %v, want ErrToolFailed", err)
	}
	if IsToolMissing(err) {
		t.Fatal("a non-zero exit was misreported as a missing tool")
	}
}

func TestRunToolTimeout(t *testing.T) {
	newTestBlob(t)

	start := time.Now()
	_, err := RunTool(context.Background(), ToolSpec{
		Name:    "sleep",
		Args:    []string{"30"},
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, ErrToolTimeout) {
		t.Fatalf("err = %v, want ErrToolTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s — the child was not killed", elapsed)
	}
}

func TestRunToolOutputCap(t *testing.T) {
	newTestBlob(t)

	_, err := RunTool(context.Background(), ToolSpec{
		Name:      "cat",
		Stdin:     bytes.Repeat([]byte("a"), 4096),
		MaxOutput: 512,
		Timeout:   10 * time.Second,
	})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err = %v, want ErrOutputTooLarge", err)
	}
}

func TestRunToolInputCaps(t *testing.T) {
	dir := t.TempDir()
	newTestBlob(t)

	big := filepath.Join(dir, "big.pdf")
	if err := os.WriteFile(big, bytes.Repeat([]byte("b"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RunTool(context.Background(), ToolSpec{
		Name:          "cat",
		Args:          []string{big},
		InputPaths:    []string{big},
		MaxInputBytes: 1024,
	})
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("oversized input file: err = %v, want ErrInputTooLarge", err)
	}

	_, err = RunTool(context.Background(), ToolSpec{
		Name:          "cat",
		Stdin:         bytes.Repeat([]byte("c"), 2048),
		MaxInputBytes: 1024,
	})
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("oversized stdin: err = %v, want ErrInputTooLarge", err)
	}
}

func TestRunToolRejectsNulInArgs(t *testing.T) {
	newTestBlob(t)

	_, err := RunTool(context.Background(), ToolSpec{Name: "cat", Args: []string{"a\x00b"}})
	if !errors.Is(err, ErrInvalidToolSpec) {
		t.Fatalf("err = %v, want ErrInvalidToolSpec", err)
	}
}
