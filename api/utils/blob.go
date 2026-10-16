package utils

// Blob storage with two interchangeable backends.
//
// docs/PLAN.md §7.1 puts every dossier object at
// "<app>/<tenant>/<dossier>/<uuid>-<name>" in the shared private bucket,
// downloaded through short-lived presigned URLs and never through a public
// ACL. That is backend A (s3Blob) and it is what runs in the cluster.
//
// Backend B (localBlob) writes the same key space under api/tmp/blobs so the
// whole ingestion → OCR → render → export path is exercisable on a laptop
// with no S3 credentials. It is a development convenience: it has no
// durability, no replication and no cross-replica visibility, so it must
// never be the active backend in a deployment.
//
// Both enforce the starter's hard "<app>/" prefix guard: a key that does not
// live under this app's prefix, or that tries to climb out of it, is
// rejected before any I/O happens.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

var blobLog = NewLogger("utils.blob")

// Blob failure classes, so services can map to a status without string
// matching. A missing object is a 404; anything else is a 502.
var (
	ErrBlobNotFound     = errors.New("blob not found")
	ErrBlobKeyForbidden = errors.New("blob key outside the app prefix")
	ErrBlobBadSignature = errors.New("invalid or expired blob signature")
)

// BlobStore is the storage abstraction every Nomos service uses. Nothing
// outside this file touches utils.S3 for dossier objects.
type BlobStore interface {
	// Put stores r under key. size may be -1 when unknown (the S3 backend
	// then buffers in multipart chunks). contentType is advisory metadata.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens the object. The caller must Close it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// SignedURL returns a short-lived URL the browser can fetch directly.
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Backend names the active implementation ("s3" or "local").
	Backend() string
}

// Blob is the package-level handle: utils.Blob.Put(ctx, key, …).
//
// It resolves its backend on first use rather than at package init, because
// utils.S3 is only populated once InitClients has run. Boot order therefore
// cannot silently pin the local backend in a deployment that does have S3.
var Blob BlobStore = &lazyBlob{}

type lazyBlob struct {
	once  sync.Once
	inner BlobStore
}

func (l *lazyBlob) resolve() BlobStore {
	l.once.Do(func() {
		if S3 != nil {
			l.inner = &s3Blob{}
			blobLog.Infof("Blob backend: s3 (bucket=%s prefix=%s)", S3Bucket, BlobPrefix())
		} else {
			root := localBlobRoot()
			l.inner = &localBlob{root: root}
			blobLog.Infof("Blob backend: local filesystem (root=%s prefix=%s) — development only, not durable", root, BlobPrefix())
		}
	})
	return l.inner
}

func (l *lazyBlob) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	return l.resolve().Put(ctx, key, r, size, contentType)
}
func (l *lazyBlob) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return l.resolve().Get(ctx, key)
}
func (l *lazyBlob) Delete(ctx context.Context, key string) error {
	return l.resolve().Delete(ctx, key)
}
func (l *lazyBlob) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return l.resolve().SignedURL(ctx, key, ttl)
}
func (l *lazyBlob) Backend() string { return l.resolve().Backend() }

// InitBlob resolves and logs the backend eagerly. Call it once from boot,
// after InitClients, so the selected backend appears in the startup log
// instead of on the first upload. Safe to call more than once.
func InitBlob() { Blob.Backend() }

// --- keys -------------------------------------------------------------------

// unsafeKeySegment matches anything we refuse to put in a key verbatim.
var unsafeKeySegment = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// BlobPrefix is this app's hard key prefix ("<app>/"). It mirrors S3Prefix
// when S3 is configured and is derived from APP_NAME otherwise, so the local
// backend lays out exactly the same key space.
func BlobPrefix() string {
	if S3Prefix != "" {
		return S3Prefix
	}
	app := os.Getenv("APP_NAME")
	if app == "" {
		app = "app" // same local-dev fallback as initS3
	}
	return app + "/"
}

// DossierKey builds the canonical object key for a dossier-scoped file:
//
//	<app>/<tenant>/<dossier>/<name>
//
// Every segment is sanitised, so neither a tenant id nor an uploaded
// filename can traverse out of the app prefix (docs/PLAN.md §7.1: tenant_id
// belongs in the row, in the Casbin domain AND in the key).
func DossierKey(tenantID, dossierID, name string) string {
	return BlobPrefix() + sanitizeSegment(tenantID) + "/" + sanitizeSegment(dossierID) + "/" + sanitizeSegment(name)
}

// sanitizeSegment reduces one path segment to safe key characters.
func sanitizeSegment(s string) string {
	s = filepath.Base(strings.TrimSpace(s))
	s = unsafeKeySegment.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		return "unnamed"
	}
	return s
}

// checkKey is the prefix guard. It runs before every read and every write,
// on both backends.
func checkKey(key string) error {
	if key == "" || strings.Contains(key, "\x00") {
		return fmt.Errorf("%w: %q", ErrBlobKeyForbidden, key)
	}
	if !strings.HasPrefix(key, BlobPrefix()) {
		return fmt.Errorf("%w: %q", ErrBlobKeyForbidden, key)
	}
	// path.Clean collapses "a/../.." — if the result leaves the prefix the
	// key was trying to escape it.
	if cleaned := path.Clean(key); cleaned != key || !strings.HasPrefix(cleaned, BlobPrefix()) {
		return fmt.Errorf("%w: %q", ErrBlobKeyForbidden, key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrBlobKeyForbidden, key)
		}
	}
	return nil
}

// --- backend A: S3 ----------------------------------------------------------

type s3Blob struct{}

func (s *s3Blob) Backend() string { return "s3" }

func (s *s3Blob) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := S3.PutObject(ctx, S3Bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *s3Blob) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	obj, err := S3.GetObject(ctx, S3Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy: Stat surfaces "no such key" now rather than on the
	// first Read, so callers get ErrBlobNotFound where they expect it.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrBlobNotFound
		}
		return nil, err
	}
	return obj, nil
}

func (s *s3Blob) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return S3.RemoveObject(ctx, S3Bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3Blob) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	u, err := S3.PresignedGetObject(ctx, S3Bucket, key, ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// --- backend B: local filesystem -------------------------------------------

type localBlob struct{ root string }

func (l *localBlob) Backend() string { return "local" }

// path maps a validated key to a file under the local root.
func (l *localBlob) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

func (l *localBlob) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	full, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	// Write to a temp file in the same directory, then rename: a crashed
	// write never leaves a half object under a real key.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".partial-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	var src io.Reader = r
	if size >= 0 {
		src = io.LimitReader(r, size)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

func (l *localBlob) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBlobNotFound
	}
	return f, err
}

func (l *localBlob) Delete(ctx context.Context, key string) error {
	full, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SignedURL returns an app-relative, HMAC-signed, short-lived URL. There is
// no object store to presign against locally, so the API serves the bytes
// itself; the signature keeps the semantics identical to S3 (a link that
// carries its own authorisation and expires).
func (l *localBlob) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	expires := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("/api/blobs/%s?expires=%d&signature=%s",
		key, expires, signBlob(key, expires)), nil
}

// localBlobRoot is api/tmp/blobs (gitignored via api/.gitignore's "tmp/").
func localBlobRoot() string { return filepath.Join(TmpRoot(), "blobs") }

// --- local signed URLs ------------------------------------------------------

// blobSecret is process-local and random: signed local URLs die with the
// process, which is the correct blast radius for a dev-only backend.
var blobSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		blobLog.Errorf("Could not seed the blob signing secret: %v", err)
	}
	return b
}()

func signBlob(key string, expires int64) string {
	mac := hmac.New(sha256.New, blobSecret)
	fmt.Fprintf(mac, "%s\n%d", key, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBlobSignature checks a local signed URL. The handler that serves
// GET /api/blobs/*key calls this before streaming anything.
func VerifyBlobSignature(key, expires, signature string) error {
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return ErrBlobBadSignature
	}
	if time.Now().Unix() > exp {
		return ErrBlobBadSignature
	}
	if !hmac.Equal([]byte(signBlob(key, exp)), []byte(signature)) {
		return ErrBlobBadSignature
	}
	return nil
}

// --- tmp root ---------------------------------------------------------------

// tmpRoot is where local blobs and per-job scratch directories live. It is
// relative to the API working directory (api/), matching api/.gitignore.
var tmpRoot = "tmp"

// TmpRoot returns the absolute tmp root, creating nothing.
func TmpRoot() string {
	abs, err := filepath.Abs(tmpRoot)
	if err != nil {
		return tmpRoot
	}
	return abs
}

// SetTmpRoot overrides the tmp root. Tests use it to stay off the real
// working directory; production never calls it.
func SetTmpRoot(p string) { tmpRoot = p }
