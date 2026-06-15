// Package memfs implements a small, writable, in-memory filesystem that
// satisfies wazero's experimental/sys.FS interface. It lets the runner mount a
// real (mutable) filesystem into the Wasm sandbox without touching the host
// disk, which is what makes the runner work both natively and in the browser
// (GOOS=js), where there is no usable OS filesystem.
//
// The same FS object is shared by the guest (via the Wasm mount) and the host
// (via the ReadFile/WriteFile/MkdirAll helpers), so the GitHub Actions
// file-command protocol (GITHUB_OUTPUT, GITHUB_ENV, ...) flows through one
// in-memory namespace.
package memfs

import (
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	sysapi "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/sys"
)

// node is a single file or directory in the tree.
type node struct {
	name     string
	dir      bool
	data     []byte
	children map[string]*node
	mode     fs.FileMode
	modTime  time.Time
	ino      uint64
}

// FS is a writable in-memory filesystem.
type FS struct {
	sysapi.UnimplementedFS
	mu      sync.Mutex
	root    *node
	nextIno uint64
}

// New returns an empty in-memory filesystem with a root directory.
func New() *FS {
	f := &FS{}
	f.root = &node{
		name:     "",
		dir:      true,
		children: map[string]*node{},
		mode:     fs.ModeDir | 0o755,
		modTime:  time.Now(),
		ino:      1,
	}
	f.nextIno = 2
	return f
}

// --- path helpers ----------------------------------------------------------

// clean normalises a guest/host path into slash-separated components relative
// to the root. "", ".", "/" all map to the root (nil components).
func clean(p string) []string {
	p = strings.TrimPrefix(p, "/")
	p = path.Clean(p)
	if p == "." || p == "" || p == "/" {
		return nil
	}
	return strings.Split(p, "/")
}

func (f *FS) lookup(parts []string) (*node, bool) {
	n := f.root
	for _, part := range parts {
		if !n.dir {
			return nil, false
		}
		child, ok := n.children[part]
		if !ok {
			return nil, false
		}
		n = child
	}
	return n, true
}

// lookupParent returns the parent directory node and the final path element.
func (f *FS) lookupParent(parts []string) (*node, string, bool) {
	if len(parts) == 0 {
		return nil, "", false
	}
	parent, ok := f.lookup(parts[:len(parts)-1])
	if !ok || !parent.dir {
		return nil, "", false
	}
	return parent, parts[len(parts)-1], true
}

func (f *FS) newNode(name string, dir bool, mode fs.FileMode) *node {
	n := &node{
		name:    name,
		dir:     dir,
		mode:    mode,
		modTime: time.Now(),
		ino:     f.nextIno,
	}
	if dir {
		n.children = map[string]*node{}
	}
	f.nextIno++
	return n
}

func statOf(n *node) sys.Stat_t {
	var st sys.Stat_t
	st.Ino = n.ino
	st.Mode = n.mode
	st.Nlink = 1
	st.Size = int64(len(n.data))
	t := n.modTime.UnixNano()
	st.Atim, st.Mtim, st.Ctim = t, t, t
	return st
}

// --- sys.FS implementation -------------------------------------------------

// OpenFile opens (and optionally creates/truncates) a file or directory.
func (f *FS) OpenFile(p string, flag sysapi.Oflag, perm fs.FileMode) (sysapi.File, sysapi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	parts := clean(p)

	// Root directory.
	if len(parts) == 0 {
		return &file{fs: f, node: f.root}, 0
	}

	n, ok := f.lookup(parts)
	if !ok {
		if flag&sysapi.O_CREAT == 0 {
			return nil, sysapi.ENOENT
		}
		parent, name, pok := f.lookupParent(parts)
		if !pok {
			return nil, sysapi.ENOENT
		}
		n = f.newNode(name, false, perm&fs.ModePerm)
		if n.mode == 0 {
			n.mode = 0o644
		}
		parent.children[name] = n
	} else {
		if flag&(sysapi.O_CREAT|sysapi.O_EXCL) == (sysapi.O_CREAT | sysapi.O_EXCL) {
			return nil, sysapi.EEXIST
		}
		if !n.dir && flag&sysapi.O_TRUNC != 0 {
			n.data = nil
			n.modTime = time.Now()
		}
	}

	return &file{fs: f, node: n, append: flag&sysapi.O_APPEND != 0}, 0
}

func (f *FS) Stat(p string) (sys.Stat_t, sysapi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.lookup(clean(p))
	if !ok {
		return sys.Stat_t{}, sysapi.ENOENT
	}
	return statOf(n), 0
}

// Lstat has no symlinks to follow here, so it behaves like Stat.
func (f *FS) Lstat(p string) (sys.Stat_t, sysapi.Errno) {
	return f.Stat(p)
}

func (f *FS) Mkdir(p string, perm fs.FileMode) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := clean(p)
	if len(parts) == 0 {
		return sysapi.EEXIST
	}
	if _, ok := f.lookup(parts); ok {
		return sysapi.EEXIST
	}
	parent, name, ok := f.lookupParent(parts)
	if !ok {
		return sysapi.ENOENT
	}
	parent.children[name] = f.newNode(name, true, fs.ModeDir|(perm&fs.ModePerm))
	return 0
}

func (f *FS) Rmdir(p string) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := clean(p)
	n, ok := f.lookup(parts)
	if !ok {
		return sysapi.ENOENT
	}
	if !n.dir {
		return sysapi.ENOTDIR
	}
	if len(n.children) > 0 {
		return sysapi.ENOTEMPTY
	}
	parent, name, _ := f.lookupParent(parts)
	delete(parent.children, name)
	return 0
}

func (f *FS) Unlink(p string) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := clean(p)
	n, ok := f.lookup(parts)
	if !ok {
		return sysapi.ENOENT
	}
	if n.dir {
		return sysapi.EISDIR
	}
	parent, name, _ := f.lookupParent(parts)
	delete(parent.children, name)
	return 0
}

func (f *FS) Rename(from, to string) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	fromParts := clean(from)
	src, ok := f.lookup(fromParts)
	if !ok {
		return sysapi.ENOENT
	}
	srcParent, srcName, ok := f.lookupParent(fromParts)
	if !ok {
		return sysapi.ENOENT
	}
	toParts := clean(to)
	dstParent, dstName, ok := f.lookupParent(toParts)
	if !ok {
		return sysapi.ENOENT
	}
	delete(srcParent.children, srcName)
	src.name = dstName
	dstParent.children[dstName] = src
	return 0
}

// Chmod and Utimens are accepted so tools that set permissions or timestamps
// don't fail; the in-memory FS doesn't otherwise enforce them.
func (f *FS) Chmod(p string, perm fs.FileMode) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.lookup(clean(p))
	if !ok {
		return sysapi.ENOENT
	}
	n.mode = (n.mode & fs.ModeType) | (perm & fs.ModePerm)
	return 0
}

func (f *FS) Utimens(p string, atim, mtim int64) sysapi.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.lookup(clean(p))
	if !ok {
		return sysapi.ENOENT
	}
	n.modTime = time.Unix(0, mtim)
	return 0
}

// --- host-side helpers -----------------------------------------------------

// MkdirAll creates a directory and all missing parents.
func (f *FS) MkdirAll(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.root
	for _, part := range clean(p) {
		child, ok := n.children[part]
		if !ok {
			child = f.newNode(part, true, fs.ModeDir|0o755)
			n.children[part] = child
		}
		if !child.dir {
			return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
		}
		n = child
	}
	return nil
}

// WriteFile writes data to a file, creating it (and parents) or truncating it.
func (f *FS) WriteFile(p string, data []byte) error {
	dir := path.Dir(strings.TrimPrefix(p, "/"))
	if dir != "." && dir != "/" {
		if err := f.MkdirAll(dir); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := clean(p)
	parent, name, ok := f.lookupParent(parts)
	if !ok {
		return &fs.PathError{Op: "write", Path: p, Err: fs.ErrNotExist}
	}
	n, ok := parent.children[name]
	if !ok {
		n = f.newNode(name, false, 0o644)
		parent.children[name] = n
	}
	n.data = append([]byte(nil), data...)
	n.modTime = time.Now()
	return nil
}

// ReadFile returns the contents of a file. ok is false if it doesn't exist.
func (f *FS) ReadFile(p string) (data []byte, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, found := f.lookup(clean(p))
	if !found || n.dir {
		return nil, false
	}
	return append([]byte(nil), n.data...), true
}

// --- file handle -----------------------------------------------------------

type file struct {
	sysapi.UnimplementedFile
	fs     *FS
	node   *node
	offset int64
	append bool
	dirIdx int
}

func (h *file) IsDir() (bool, sysapi.Errno) { return h.node.dir, 0 }

func (h *file) Stat() (sys.Stat_t, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	return statOf(h.node), 0
}

func (h *file) Read(buf []byte) (int, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.node.dir {
		return 0, sysapi.EISDIR
	}
	if h.offset >= int64(len(h.node.data)) {
		return 0, 0 // EOF is a zero-length read in wazero's sys API.
	}
	n := copy(buf, h.node.data[h.offset:])
	h.offset += int64(n)
	return n, 0
}

func (h *file) Pread(buf []byte, off int64) (int, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.node.dir {
		return 0, sysapi.EISDIR
	}
	if off >= int64(len(h.node.data)) {
		return 0, 0
	}
	return copy(buf, h.node.data[off:]), 0
}

func (h *file) Write(buf []byte) (int, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.node.dir {
		return 0, sysapi.EISDIR
	}
	if h.append {
		h.offset = int64(len(h.node.data))
	}
	end := h.offset + int64(len(buf))
	if end > int64(len(h.node.data)) {
		grown := make([]byte, end)
		copy(grown, h.node.data)
		h.node.data = grown
	}
	copy(h.node.data[h.offset:], buf)
	h.offset = end
	h.node.modTime = time.Now()
	return len(buf), 0
}

func (h *file) Pwrite(buf []byte, off int64) (int, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.node.dir {
		return 0, sysapi.EISDIR
	}
	end := off + int64(len(buf))
	if end > int64(len(h.node.data)) {
		grown := make([]byte, end)
		copy(grown, h.node.data)
		h.node.data = grown
	}
	copy(h.node.data[off:], buf)
	h.node.modTime = time.Now()
	return len(buf), 0
}

func (h *file) Seek(offset int64, whence int) (int64, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	switch whence {
	case io.SeekStart:
		h.offset = offset
	case io.SeekCurrent:
		h.offset += offset
	case io.SeekEnd:
		h.offset = int64(len(h.node.data)) + offset
	default:
		return 0, sysapi.EINVAL
	}
	return h.offset, 0
}

func (h *file) Truncate(size int64) sysapi.Errno {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.node.dir {
		return sysapi.EISDIR
	}
	if size <= int64(len(h.node.data)) {
		h.node.data = h.node.data[:size]
	} else {
		grown := make([]byte, size)
		copy(grown, h.node.data)
		h.node.data = grown
	}
	h.node.modTime = time.Now()
	return 0
}

func (h *file) Readdir(n int) ([]sysapi.Dirent, sysapi.Errno) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if !h.node.dir {
		return nil, sysapi.ENOTDIR
	}
	names := make([]string, 0, len(h.node.children))
	for name := range h.node.children {
		names = append(names, name)
	}
	sort.Strings(names)

	if h.dirIdx >= len(names) {
		return nil, 0
	}
	remaining := names[h.dirIdx:]
	if n > 0 && n < len(remaining) {
		remaining = remaining[:n]
	}
	out := make([]sysapi.Dirent, 0, len(remaining))
	for _, name := range remaining {
		c := h.node.children[name]
		out = append(out, sysapi.Dirent{Ino: c.ino, Name: name, Type: c.mode.Type()})
	}
	h.dirIdx += len(remaining)
	return out, 0
}

func (h *file) IsAppend() bool { return h.append }

func (h *file) SetAppend(enable bool) sysapi.Errno {
	h.append = enable
	return 0
}

func (h *file) Sync() sysapi.Errno     { return 0 }
func (h *file) Datasync() sysapi.Errno { return 0 }
func (h *file) Close() sysapi.Errno    { return 0 }
