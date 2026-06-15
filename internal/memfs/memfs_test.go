package memfs

import (
	"testing"

	sysapi "github.com/tetratelabs/wazero/experimental/sys"
)

func TestHostHelpersRoundTrip(t *testing.T) {
	f := New()
	if err := f.MkdirAll("/github"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := f.WriteFile("/github/output", []byte("greeting=hi\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, ok := f.ReadFile("/github/output")
	if !ok || string(data) != "greeting=hi\n" {
		t.Fatalf("ReadFile = %q, %v", data, ok)
	}
}

func TestWriteFileCreatesParents(t *testing.T) {
	f := New()
	if err := f.WriteFile("/work/sub/note.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, ok := f.ReadFile("/work/sub/note.txt"); !ok {
		t.Fatal("expected nested file to exist")
	}
}

func TestOpenFileCreateWriteAppendRead(t *testing.T) {
	f := New()

	// Create + write.
	wf, errno := f.OpenFile("/log", sysapi.O_CREAT|sysapi.O_WRONLY, 0o644)
	if errno != 0 {
		t.Fatalf("open create: errno %d", errno)
	}
	if _, e := wf.Write([]byte("line1\n")); e != 0 {
		t.Fatalf("write: errno %d", e)
	}
	wf.Close()

	// Append.
	af, errno := f.OpenFile("/log", sysapi.O_WRONLY|sysapi.O_APPEND, 0o644)
	if errno != 0 {
		t.Fatalf("open append: errno %d", errno)
	}
	if !af.IsAppend() {
		t.Fatal("expected IsAppend true")
	}
	if _, e := af.Write([]byte("line2\n")); e != 0 {
		t.Fatalf("append write: errno %d", e)
	}
	af.Close()

	if got, _ := f.ReadFile("/log"); string(got) != "line1\nline2\n" {
		t.Fatalf("append result = %q", got)
	}
}

func TestOpenTruncate(t *testing.T) {
	f := New()
	_ = f.WriteFile("/x", []byte("oldcontent"))
	wf, errno := f.OpenFile("/x", sysapi.O_WRONLY|sysapi.O_TRUNC, 0o644)
	if errno != 0 {
		t.Fatalf("open trunc: errno %d", errno)
	}
	wf.Write([]byte("new"))
	wf.Close()
	if got, _ := f.ReadFile("/x"); string(got) != "new" {
		t.Fatalf("trunc result = %q", got)
	}
}

func TestOpenMissingNoCreate(t *testing.T) {
	f := New()
	if _, errno := f.OpenFile("/missing", sysapi.O_RDONLY, 0); errno != sysapi.ENOENT {
		t.Fatalf("expected ENOENT, got %d", errno)
	}
}

func TestMkdirAndReaddir(t *testing.T) {
	f := New()
	if errno := f.Mkdir("/d", 0o755); errno != 0 {
		t.Fatalf("mkdir: errno %d", errno)
	}
	_ = f.WriteFile("/d/b", []byte("b"))
	_ = f.WriteFile("/d/a", []byte("a"))

	dh, errno := f.OpenFile("/d", sysapi.O_RDONLY, 0)
	if errno != 0 {
		t.Fatalf("open dir: errno %d", errno)
	}
	ents, errno := dh.Readdir(-1)
	if errno != 0 {
		t.Fatalf("readdir: errno %d", errno)
	}
	if len(ents) != 2 || ents[0].Name != "a" || ents[1].Name != "b" {
		t.Fatalf("readdir entries = %+v", ents)
	}
}

func TestUnlinkAndRename(t *testing.T) {
	f := New()
	_ = f.WriteFile("/a", []byte("1"))
	if errno := f.Rename("/a", "/b"); errno != 0 {
		t.Fatalf("rename: errno %d", errno)
	}
	if _, ok := f.ReadFile("/a"); ok {
		t.Fatal("expected /a gone after rename")
	}
	if got, ok := f.ReadFile("/b"); !ok || string(got) != "1" {
		t.Fatalf("rename dest = %q %v", got, ok)
	}
	if errno := f.Unlink("/b"); errno != 0 {
		t.Fatalf("unlink: errno %d", errno)
	}
	if _, ok := f.ReadFile("/b"); ok {
		t.Fatal("expected /b gone after unlink")
	}
}
