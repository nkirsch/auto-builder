package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := Dir(t.TempDir())
	if err := d.Put(ctx, "attachments/1/a.txt", "text/plain", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	rc, err := d.Get(ctx, "attachments/1/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hi" {
		t.Fatalf("got %q", b)
	}
	if _, err := d.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDirStaysInside(t *testing.T) {
	root := t.TempDir()
	d := Dir(filepath.Join(root, "store"))
	if err := d.Put(context.Background(), "../../escape", "text/plain", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "store", "escape")); err != nil {
		t.Fatalf("object not kept inside the directory: %v", err)
	}
}

func TestPrefixed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := Prefixed{Storage: Dir(dir), Prefix: "t/alpha/"}
	if err := p.Put(ctx, "attachments/1/a.txt", "text/plain", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "t/alpha/attachments/1/a.txt")); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Get(ctx, "attachments/1/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	other := Prefixed{Storage: Dir(dir), Prefix: "t/beta/"}
	if _, err := other.Get(ctx, "attachments/1/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant reads it: %v", err)
	}
}
