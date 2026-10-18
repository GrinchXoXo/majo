package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var _ Provider = (*LocalProvider)(nil)

func TestLocalProvider_PutGetDeleteRoundTrip(t *testing.T) {
	root := t.TempDir()
	p, err := NewLocalProvider(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "vendors/0b7e/media/abc123.jpg"
	if err := p.Put(ctx, key, []byte("hello"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(ctx, key)
	if err != nil || string(got) != "hello" {
		t.Fatalf("get: %q %v", got, err)
	}
	// Same key again overwrites in place (idempotent retries): one file, no temp leftovers.
	if err := p.Put(ctx, key, []byte("hello"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "vendors", "0b7e", "media"))
	if len(entries) != 1 || entries[0].Name() != "abc123.jpg" {
		t.Fatalf("expected exactly one stored object, got %v", entries)
	}
	info, _ := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	// Windows has no POSIX permission bits (Go reports rw-rw-rw-); the
	// production server is Linux, where 0600 is enforced.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 file mode, got %v", info.Mode().Perm())
	}
	if err := p.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := p.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing key should be a no-op, got %v", err)
	}
}

func TestLocalProvider_RejectsKeysThatEscapeTheRoot(t *testing.T) {
	root := t.TempDir()
	p, _ := NewLocalProvider(root)
	for _, key := range []string{"../evil", "a/../../evil", "/etc/passwd", "a//b", "a/./b", ".hidden", "", "a b", "caption: bag ₦18,500.jpg", `a\b`} {
		err := p.Put(context.Background(), key, []byte("x"), "")
		var ik InvalidKeyError
		if !errors.As(err, &ik) || !ik.Permanent() {
			t.Fatalf("key %q: expected permanent InvalidKeyError, got %v", key, err)
		}
	}
	// Nothing was written outside (or inside) the root.
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("rejected keys must not create files, found %v", entries)
	}
}

func TestLocalProvider_NoPublicURLs(t *testing.T) {
	p, _ := NewLocalProvider(t.TempDir())
	u, err := p.GenerateURL(context.Background(), "vendors/x/media/y.jpg")
	if !errors.Is(err, ErrURLNotSupported) || u != "" {
		t.Fatalf("expected no URL, got %q %v", u, err)
	}
}

func TestLocalProvider_LargeObjectAndRootCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "media")
	p, err := NewLocalProvider(root)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{7}, 3<<20)
	if err := p.Put(context.Background(), "k/big.mp4", data, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Get(context.Background(), "k/big.mp4")
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip mismatch")
	}
	if strings.Contains(p.root, "..") {
		t.Fatal("root must be absolute and clean")
	}
}
