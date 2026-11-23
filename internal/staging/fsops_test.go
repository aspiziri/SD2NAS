package staging

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func xdev(o, n string) error {
	return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV}
}

func TestMoveFileRename(t *testing.T) {
	e := newOpsEnv(t)
	src := e.write(t, "Staging/x/a.JPG", "one")
	dir := filepath.Join(e.photos, "Archive", "sub")
	got, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "a.JPG") || readFile(t, got) != "one" || exists(src) {
		t.Fatalf("got %s, src exists %v", got, exists(src))
	}
}

func TestMoveFileCollision(t *testing.T) {
	e := newOpsEnv(t)
	src := e.write(t, "Staging/x/a.JPG", "new")
	dir := filepath.Join(e.photos, "Archive")
	e.write(t, "Archive/a.JPG", "old")
	e.write(t, "Archive/a-1.JPG", "older")
	got, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "a-2.JPG") || readFile(t, got) != "new" {
		t.Fatalf("got %s", got)
	}
	if readFile(t, filepath.Join(dir, "a.JPG")) != "old" || readFile(t, filepath.Join(dir, "a-1.JPG")) != "older" {
		t.Fatal("an existing file was overwritten")
	}
}

func TestMoveFileIdentical(t *testing.T) {
	e := newOpsEnv(t)
	src := e.write(t, "Staging/x/a.JPG", "same")
	e.write(t, "Archive/a.JPG", "same")
	dir := filepath.Join(e.photos, "Archive")
	got, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "a.JPG") || exists(src) {
		t.Fatalf("got %s, src exists %v", got, exists(src))
	}
	if files := e.files(t); !slices.Equal(files, []string{"Archive/a.JPG"}) {
		t.Fatalf("files = %v", files)
	}
}

func TestMoveFileCrossDevice(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.rename = xdev
	src := e.write(t, "Staging/x/a.JPG", "payload")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.photos, "Archive")
	got, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", shaOf("payload"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, got) != "payload" || exists(src) || !st.ModTime().Equal(old) {
		t.Fatalf("copy wrong: src exists %v, mtime %v", exists(src), st.ModTime())
	}
}

func TestMoveFileCrossDeviceMismatch(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.rename = xdev
	src := e.write(t, "Staging/x/a.JPG", "payload")
	dir := filepath.Join(e.photos, "Archive")
	if _, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", shaOf("other")); err == nil {
		t.Fatal("want an error")
	}
	if !exists(src) {
		t.Fatal("src was removed")
	}
	if files := e.files(t); !slices.Equal(files, []string{"Staging/x/a.JPG"}) {
		t.Fatalf("files = %v", files)
	}
}

func TestMoveFileCancelled(t *testing.T) {
	e := newOpsEnv(t)
	src := e.write(t, "Staging/x/a.JPG", "one")
	dir := filepath.Join(e.photos, "Archive", "sub")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.svc.moveFile(ctx, src, dir, "a.JPG", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !exists(src) || exists(dir) {
		t.Fatalf("src exists %v, dir exists %v", exists(src), exists(dir))
	}
}

func TestMoveFileRenameError(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.rename = func(o, n string) error { return fs.ErrPermission }
	src := e.write(t, "Staging/x/a.JPG", "one")
	dir := filepath.Join(e.photos, "Archive")
	if _, err := e.svc.moveFile(t.Context(), src, dir, "a.JPG", ""); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
	if files := e.files(t); !slices.Equal(files, []string{"Staging/x/a.JPG"}) {
		t.Fatalf("files = %v", files)
	}
}

func TestRemoveEmptyTree(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("x/Capture")
	touch("x/Videos/.DS_Store")
	touch("x/Thumbs.db")
	gone, kept, err := removeEmptyTree(filepath.Join(root, "x"))
	if err != nil || !gone || len(kept) != 0 || exists(filepath.Join(root, "x")) {
		t.Fatalf("x: gone=%v kept=%v err=%v", gone, kept, err)
	}

	touch("y/Capture/a.ARW")
	touch("y/.DS_Store")
	mkdir("y/Videos")
	gone, kept, err = removeEmptyTree(filepath.Join(root, "y"))
	if err != nil || gone || !slices.Equal(kept, []string{"Capture/a.ARW"}) {
		t.Fatalf("y: gone=%v kept=%v err=%v", gone, kept, err)
	}
	if !exists(filepath.Join(root, "y", ".DS_Store")) {
		t.Fatal("junk was removed from a folder that stays")
	}
	if exists(filepath.Join(root, "y", "Videos")) {
		t.Fatal("empty subfolder was kept")
	}

	gone, _, err = removeEmptyTree(filepath.Join(root, "missing"))
	if err != nil || !gone {
		t.Fatalf("missing: gone=%v err=%v", gone, err)
	}
}

func TestFolderFiles(t *testing.T) {
	e := newOpsEnv(t)
	for _, rel := range []string{"f/b.JPG", "f/Capture/a.ARW", "f/.DS_Store", "f/._a.JPG", "f/x.JPG.partial", "f/THUMBS.DB"} {
		e.write(t, rel, "x")
	}
	got, err := folderFiles(filepath.Join(e.photos, "f"))
	if err != nil || !slices.Equal(got, []string{"Capture/a.ARW", "b.JPG"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	got, err = folderFiles(filepath.Join(e.photos, "none"))
	if err != nil || got != nil {
		t.Fatalf("missing: %v, %v", got, err)
	}
}

func TestCrossDevice(t *testing.T) {
	if !crossDevice(&os.LinkError{Err: syscall.EXDEV}) || crossDevice(fs.ErrPermission) {
		t.Fatal("crossDevice wrong")
	}
}
