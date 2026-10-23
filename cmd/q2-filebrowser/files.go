package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/group-a-study/q2-filebrowser/internal/q2"
)

// The card as the browser sees it: paths are "/"-rooted at the card ("/Music/a.flac"), cleaned
// before use, so no path reaches outside it. Uploads are written aside (a hidden .q2part beside
// their name) in chunks, each opened and closed around its write, so the card is almost never busy
// and USB storage can take it, then renamed into place.

const (
	chunk    = 512 << 10 // bytes gathered before one open-write-close on the card
	spare    = 16 << 20  // room an upload leaves on the card
	partExt  = ".q2part"
	maxFound = 300 // a search's results
)

// cardReady is whether the card is there (not lent to USB storage); tests have one that is.
var cardReady = q2.CardMounted

// apiErr is a failure the browser is told, with its HTTP status.
type apiErr struct {
	code int
	msg  string
}

func (e *apiErr) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &apiErr{code, fmt.Sprintf(format, args...)}
}

var errBadPath = fail(400, "That isn't a path on the card")

type entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // ms since 1970
	Path  string `json:"path,omitempty"`
}

// transfer is a file moving, for the Q2's page.
type transfer struct {
	up         bool
	name       string
	done, size int64
}

type files struct {
	root string

	mu     sync.Mutex
	moving map[*transfer]bool
	ctx    context.Context // cancelled on quit: uploads end
	stop   context.CancelFunc
	once   sync.Once
}

func (f *files) init() {
	f.once.Do(func() {
		f.moving = map[*transfer]bool{}
		f.ctx, f.stop = context.WithCancel(context.Background())
	})
}

func (f *files) cancel() { f.init(); f.stop() }

// abs is the browser's path p on disk, with p cleaned.
func (f *files) abs(p string) (string, string, error) {
	if strings.ContainsRune(p, 0) || !utf8.ValidString(p) {
		return "", "", errBadPath
	}
	clean := path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
	return filepath.Join(f.root, filepath.FromSlash(clean)), clean, nil
}

func (f *files) begin(up bool, name string, size int64) *transfer {
	f.init()
	t := &transfer{up: up, name: name, size: size}
	f.mu.Lock()
	f.moving[t] = true
	f.mu.Unlock()
	return t
}

func (f *files) end(t *transfer) {
	f.mu.Lock()
	delete(f.moving, t)
	f.mu.Unlock()
}

func (f *files) progress(t *transfer, n int64) {
	f.mu.Lock()
	t.done += n
	f.mu.Unlock()
}

// busy is whether a file is moving.
func (f *files) busy() bool {
	f.init()
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.moving) > 0
}

// activity is the files moving: how many each way, the newest upload's (else download's) name,
// and the bytes done of those of known size.
func (f *files) activity() (ups, downs int, name string, done, size int64) {
	f.init()
	f.mu.Lock()
	defer f.mu.Unlock()
	for t := range f.moving {
		if t.up {
			ups++
			name = t.name
		} else {
			downs++
			if ups == 0 {
				name = t.name
			}
		}
		if t.size > 0 {
			done, size = done+t.done, size+t.size
		}
	}
	return
}

// usage is the card's free and total bytes.
func (f *files) usage() (free, total int64) {
	var st syscall.Statfs_t
	if syscall.Statfs(f.root, &st) != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }

func mkEntry(name string, info fs.FileInfo) entry {
	e := entry{Name: name, Dir: info.IsDir(), MTime: info.ModTime().UnixMilli()}
	if !e.Dir {
		e.Size = info.Size()
	}
	return e
}

// list is a folder's entries, folders first, then by name; dot files only if asked.
func (f *files) list(p string, showHidden bool) ([]entry, string, error) {
	full, clean, err := f.abs(p)
	if err != nil {
		return nil, "", err
	}
	des, err := os.ReadDir(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, clean, fail(404, "That folder isn't on the card")
	}
	if err != nil {
		return nil, clean, fail(500, "Couldn't read the folder: %v", err)
	}
	out := make([]entry, 0, len(des))
	for _, d := range des {
		name := d.Name()
		if strings.HasSuffix(name, partExt) || (!showHidden && hidden(name)) {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		out = append(out, mkEntry(name, info))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, clean, nil
}

// validName is a file or folder name the card takes (FAT's rules), or why not.
func validName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fail(400, "Give it a name")
	case len(name) > 255:
		return fail(400, "That name is too long")
	case strings.ContainsAny(name, `/\<>:"|?*`):
		return fail(400, `A name can't have / \ < > : " | ? or *`)
	case strings.TrimRight(name, ". ") != name:
		return fail(400, "A name can't end in a dot or a space")
	}
	for _, c := range name {
		if c < 32 || c == utf8.RuneError {
			return fail(400, "That name has a character the card can't take")
		}
	}
	return nil
}

func (f *files) exists(full string) bool {
	_, err := os.Lstat(full)
	return err == nil
}

// mkdir makes the folder p (and its parents).
func (f *files) mkdir(p string) (string, error) {
	full, clean, err := f.abs(p)
	if err != nil || clean == "/" {
		return "", errBadPath
	}
	if err := validName(path.Base(clean)); err != nil {
		return "", err
	}
	if info, err := os.Stat(full); err == nil {
		if info.IsDir() {
			return "", fail(409, "There's a folder named %s already", path.Base(clean))
		}
		return "", fail(409, "There's a file named %s already", path.Base(clean))
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return "", fail(500, "Couldn't make the folder: %v", err)
	}
	return clean, nil
}

// existing is p on disk, which must be there and not the card itself.
func (f *files) existing(p string) (string, string, error) {
	full, clean, err := f.abs(p)
	if err != nil {
		return "", "", err
	}
	if clean == "/" {
		return "", "", fail(400, "That's the whole card")
	}
	if !f.exists(full) {
		return "", "", fail(404, "%s isn't on the card any more", path.Base(clean))
	}
	return full, clean, nil
}

// rename gives p the name name, in its folder.
func (f *files) rename(p, name string) (string, error) {
	full, clean, err := f.existing(p)
	if err != nil {
		return "", err
	}
	if err := validName(name); err != nil {
		return "", err
	}
	to := filepath.Join(filepath.Dir(full), name)
	dest := path.Join(path.Dir(clean), name)
	if name == path.Base(clean) {
		return dest, nil
	}
	if strings.EqualFold(name, path.Base(clean)) {
		// The card's FAT doesn't tell case apart: by way of another name.
		tmp := full + partExt
		if err := os.Rename(full, tmp); err != nil {
			return "", fail(500, "Couldn't rename: %v", err)
		}
		full = tmp
	} else if f.exists(to) {
		return "", fail(409, "There's already something named %s there", name)
	}
	if err := os.Rename(full, to); err != nil {
		return "", fail(500, "Couldn't rename: %v", err)
	}
	return dest, nil
}

// unique is full, or "name (2).ext" and so on when something is there already.
func unique(full string) string {
	ext := filepath.Ext(full)
	base := strings.TrimSuffix(full, ext)
	for n := 2; ; n++ {
		if _, err := os.Lstat(full); err != nil {
			return full
		}
		full = fmt.Sprintf("%s (%d)%s", base, n, ext)
	}
}

// move puts each of paths in the folder to, keeping its name (or numbering it when the name is
// taken there). It returns the new paths.
func (f *files) move(paths []string, to string) ([]string, error) {
	dir, dclean, err := f.abs(to)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fail(404, "That folder isn't on the card")
	}
	var moved []string
	for _, p := range paths {
		full, clean, err := f.existing(p)
		if err != nil {
			return moved, err
		}
		if dclean == clean || strings.HasPrefix(dclean+"/", clean+"/") {
			return moved, fail(400, "A folder can't go inside itself")
		}
		if path.Dir(clean) == dclean {
			moved = append(moved, clean)
			continue
		}
		dest := unique(filepath.Join(dir, filepath.Base(full)))
		if err := os.Rename(full, dest); err != nil {
			return moved, fail(500, "Couldn't move %s: %v", path.Base(clean), err)
		}
		rel, _ := filepath.Rel(f.root, dest)
		moved = append(moved, "/"+filepath.ToSlash(rel))
	}
	return moved, nil
}

// remove deletes each of paths, folders with all they hold: all of them there, or none deleted.
func (f *files) remove(paths []string) error {
	fulls := make([]string, len(paths))
	for i, p := range paths {
		full, _, err := f.existing(p)
		if err != nil {
			return err
		}
		fulls[i] = full
	}
	for _, full := range fulls {
		if err := os.RemoveAll(full); err != nil {
			return fail(500, "Couldn't delete %s: %v", filepath.Base(full), err)
		}
	}
	return nil
}

// safeRel is a sender's relative name (folders with / or \) made card-safe: each part's FAT-unsafe
// characters replaced, "." and ".." parts dropped, so it stays inside the folder it lands in.
func safeRel(name string) string {
	var parts []string
	for _, p := range strings.FieldsFunc(name, func(c rune) bool { return c == '/' || c == '\\' }) {
		p = strings.Map(func(c rune) rune {
			if c < 32 || strings.ContainsRune(`<>:"|?*`, c) || c == utf8.RuneError {
				return '_'
			}
			return c
		}, p)
		p = strings.TrimRight(strings.TrimSpace(p), ". ")
		if p == "" || p == "." || p == ".." {
			continue
		}
		if len(p) > 200 {
			n := 200
			for n > 0 && !utf8.RuneStart(p[n]) {
				n--
			}
			p = p[:n]
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "file"
	}
	return filepath.Join(parts...)
}

// cardWrite writes data at off in path, opened and closed around the write and synced, so the
// card is busy only for that long.
func cardWrite(path string, off int64, data []byte) error {
	if !cardReady() {
		return q2.ErrNoCard
	}
	return q2.Append(path, off, data)
}

// upload saves body as name (a relative path: a folder upload's folders kept) in the folder dir:
// numbered when the name is taken, unless overwrite. size is the body's length if known (-1). It
// returns the file's path.
func (f *files) upload(dir, name string, size int64, overwrite bool, body io.Reader) (string, error) {
	f.init()
	base, _, err := f.abs(dir)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return "", fail(404, "That folder isn't on the card")
	}
	if free, _ := f.usage(); size > 0 && free > 0 && size+spare > free {
		return "", fail(507, "Not enough room on the card")
	}
	dest := filepath.Join(base, safeRel(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fail(500, "Couldn't make the folder: %v", err)
	}
	if info, err := os.Stat(dest); err == nil && (!overwrite || info.IsDir()) {
		dest = unique(dest)
	}
	part := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+partExt)
	os.Remove(part)
	t := f.begin(true, filepath.Base(dest), size)
	defer f.end(t)
	buf := make([]byte, 0, chunk)
	tmp := make([]byte, 64<<10)
	var off int64
	for {
		n, rerr := body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) >= chunk || (rerr != nil && len(buf) > 0) {
			if err = cardWrite(part, off, buf); err != nil {
				break
			}
			off += int64(len(buf))
			f.progress(t, int64(len(buf)))
			buf = buf[:0]
		}
		if f.ctx.Err() != nil {
			err = errors.New("File Browser was turned off")
			break
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil && off == 0 {
		err = cardWrite(part, 0, nil) // an empty file
	}
	if err == nil && size >= 0 && off != size {
		err = fmt.Errorf("got %d of %d bytes", off, size)
	}
	if err == nil {
		err = os.Rename(part, dest)
	}
	if err != nil {
		os.Remove(part)
		if errors.Is(err, q2.ErrNoCard) {
			return "", fail(503, "The card went away (USB storage?)")
		}
		return "", fail(500, "Couldn't save %s: %v", filepath.Base(dest), err)
	}
	rel, _ := filepath.Rel(f.root, dest)
	return "/" + filepath.ToSlash(rel), nil
}

// search finds names holding q (any case) under p, up to maxFound, looking for at most 8 s.
func (f *files) search(p, q string, showHidden bool) ([]entry, error) {
	full, _, err := f.abs(p)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil, fail(400, "Search for what?")
	}
	var out []entry
	end := time.Now().Add(8 * time.Second)
	errStop := errors.New("stop")
	filepath.WalkDir(full, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || fp == full {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, partExt) || (!showHidden && hidden(name)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(strings.ToLower(name), q) {
			if info, err := d.Info(); err == nil {
				e := mkEntry(name, info)
				rel, _ := filepath.Rel(f.root, fp)
				e.Path = "/" + filepath.ToSlash(rel)
				out = append(out, e)
			}
		}
		if len(out) >= maxFound || time.Now().After(end) {
			return errStop
		}
		return nil
	})
	return out, nil
}

// counter counts what a transfer has read.
type counter struct {
	f *files
	t *transfer
	r io.ReadSeeker
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.f.progress(c.t, int64(n))
	return n, err
}

func (c *counter) Seek(off int64, whence int) (int64, error) { return c.r.Seek(off, whence) }

// zip writes paths to w as a ZIP (stored, not compressed: music doesn't compress, and the Q2's CPU
// is slow), each folder with all it holds but dot files, under its own name.
func (f *files) zip(paths []string, w io.Writer) error {
	zw := zip.NewWriter(w)
	t := f.begin(false, "ZIP", 0)
	defer f.end(t)
	for _, p := range paths {
		full, clean, err := f.existing(p)
		if err != nil {
			return err
		}
		parent := filepath.Dir(full)
		err = filepath.WalkDir(full, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if fp != full && (hidden(d.Name()) || strings.HasSuffix(d.Name(), partExt)) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(parent, fp)
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			h.Name, h.Method = filepath.ToSlash(rel), zip.Store
			if d.IsDir() {
				h.Name += "/"
				_, err = zw.CreateHeader(h)
				return err
			}
			out, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			in, err := os.Open(fp)
			if err != nil {
				return err
			}
			defer in.Close()
			n, err := io.Copy(out, in)
			f.progress(t, n)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", clean, err)
		}
	}
	return zw.Close()
}
