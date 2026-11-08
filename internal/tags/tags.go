// Package tags reads and writes the tags of the songs the Q2 plays: ID3v2 (MP3, MP2, AAC, and AIFF,
// WAV and DSF's ID3 chunk or tag), FLAC's Vorbis comments, Ogg Vorbis and Opus comments, MP4's ilst
// (m4a, m4b) and APEv2 (Monkey's Audio), the audio and every other frame, block or atom kept as they
// were. A tag that fits the room the file already has (its
// padding, a free atom) is written in place; otherwise the file is written again beside itself and
// renamed over, so a player holding it open keeps reading the old one. Standard library only.
package tags

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// Field is a tag as the editor shows it, whatever the format calls it.
type Field string

const (
	Title       Field = "title"
	Artist      Field = "artist"
	Album       Field = "album"
	AlbumArtist Field = "albumartist"
	Track       Field = "track"      // its number alone: "3"
	TrackTotal  Field = "tracktotal" // of how many: "12"
	Disc        Field = "disc"
	DiscTotal   Field = "disctotal"
	Date        Field = "date" // a year, or a date that starts with one: "2004", "2004-03-22"
	Genre       Field = "genre"
	Composer    Field = "composer"
	Comment     Field = "comment"
	Lyrics      Field = "lyrics" // unsynced, several lines
)

// Fields is every field, in the order the editor shows them.
var Fields = []Field{Title, Artist, Album, AlbumArtist, Track, TrackTotal, Disc, DiscTotal, Date, Genre,
	Composer, Comment, Lyrics}

var names = map[Field]string{Title: "Title", Artist: "Artist", Album: "Album", AlbumArtist: "Album Artist",
	Track: "Track", TrackTotal: "Tracks", Disc: "Disc", DiscTotal: "Discs", Date: "Year", Genre: "Genre",
	Composer: "Composer", Comment: "Comment", Lyrics: "Lyrics"}

// Name is the field as the Q2's page names it: "Album Artist".
func (f Field) Name() string { return names[f] }

// Known is whether f is one of Fields.
func Known(f Field) bool { _, ok := names[f]; return ok }

// numeric fields hold a number alone.
func numeric(f Field) bool { return f == Track || f == TrackTotal || f == Disc || f == DiscTotal }

// PictureInfo is what the editor shows of an embedded picture.
type PictureInfo struct {
	MIME        string `json:"mime"`
	Size        int    `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Progressive bool   `json:"progressive"`
}

// Picture is an embedded picture: Type is ID3's and FLAC's picture type (3, the front cover).
type Picture struct {
	MIME string
	Type byte
	Desc string
	Data []byte
}

// Info is one song's tags.
type Info struct {
	Format   string           `json:"format"`   // "MP3 (ID3v2.4)", "FLAC", "Opus", "M4A"
	Writable bool             `json:"writable"` // whether Write can change it
	Fields   map[Field]string `json:"fields"`   // the fields it has, "" left out
	Picture  *PictureInfo     `json:"picture"`  // the front cover (else the first picture), nil without one
	Notes    []string         `json:"notes"`    // what the editor should know, in a sentence each
}

// Changes are what Write does: each field in Set takes its value ("" removes it), Picture replaces
// every embedded picture with itself as the front cover, RemovePicture removes them all.
type Changes struct {
	Set           map[Field]string
	Picture       *Picture
	RemovePicture bool
}

// Empty is whether c changes nothing.
func (c Changes) Empty() bool { return len(c.Set) == 0 && c.Picture == nil && !c.RemovePicture }

var (
	ErrUnsupported = errors.New("tags: not a format this reads")
	ErrReadOnly    = errors.New("tags: this format can be read, not written")
	ErrNoRoom      = errors.New("tags: not enough room on the card to write the file again")
	ErrNoCard      = errors.New("tags: the card isn't there")
	ErrTooBig      = errors.New("tags: the file is too big to write")
	ErrCorrupt     = errors.New("tags: the file's tags or structure aren't as its format says")
)

// CardReady is whether the card is there to write to; the app points it at its own check.
var CardReady = func() bool { return true }

// Spare is room a full rewrite leaves on the card, as uploads do.
const Spare = 16 << 20

// MaxPicture is the biggest picture Write takes, and maxValue the longest value but Lyrics'.
const (
	MaxPicture = 4 << 20
	maxValue   = 1024
	maxLyrics  = 64 << 10
)

// picRef is a picture a codec found: where its bytes are in the file (Off, Len), or, when the file
// holds them some other way (base64, unsynchronised), the bytes themselves (Data).
type picRef struct {
	MIME      string
	Type      byte
	Desc      string
	Off, Len  int64
	Data      []byte
	hasOffset bool
}

// atFile is a picture whose bytes are in the file as they are.
func atFile(mime string, typ byte, desc string, off, n int64) *picRef {
	return &picRef{MIME: mime, Type: typ, Desc: desc, Off: off, Len: n, hasOffset: true}
}

// codec is one format: read gives the tags and the front cover (else the first picture, nil
// without), write changes them (Write has checked c and holds the file's lock).
type codec interface {
	read(r io.ReaderAt, size int64) (*Info, *picRef, error)
	write(path string, c Changes) error
}

var codecs = map[string]codec{}

// register is called from each format's init with its extensions, lower case with the dot.
func register(c codec, exts ...string) {
	for _, e := range exts {
		codecs[e] = c
	}
}

func codecFor(name string) codec { return codecs[strings.ToLower(filepath.Ext(name))] }

// Supported is whether name's extension is one this reads.
func Supported(name string) bool { return codecFor(name) != nil }

// Exts is the extensions this reads.
func Exts() []string {
	out := make([]string, 0, len(codecs))
	for e := range codecs {
		out = append(out, e)
	}
	return out
}

func open(path string) (*os.File, int64, codec, error) {
	c := codecFor(path)
	if c == nil {
		return nil, 0, nil, ErrUnsupported
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, 0, nil, ErrUnsupported
	}
	return f, st.Size(), c, nil
}

// Read is path's tags.
func Read(path string) (*Info, error) {
	f, size, c, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, pic, err := safeRead(c, f, size)
	if err != nil {
		return nil, err
	}
	if info.Fields == nil {
		info.Fields = map[Field]string{}
	}
	for k, v := range info.Fields {
		if v = clean(k, v); v == "" {
			delete(info.Fields, k)
		} else {
			info.Fields[k] = v
		}
	}
	if pic != nil {
		head, n := pic.Data, int(pic.Len)
		if pic.hasOffset {
			// The size is near the start of most JPEGs; past a big EXIF or ICC block, read on.
			for _, k := range []int64{16 << 10, 256 << 10} {
				head = make([]byte, min(pic.Len, k))
				m, _ := f.ReadAt(head, pic.Off)
				head = head[:m]
				if pi := pictureInfo(head); pi.Width > 0 || int64(m) >= pic.Len {
					break
				}
			}
		} else {
			n = len(pic.Data)
		}
		pi := pictureInfo(head)
		if pi.MIME == "" {
			pi.MIME = declared(pic.MIME)
		}
		pi.Size = n
		info.Picture = &pi
	}
	return info, nil
}

// ReadPicture is path's front cover (else its first picture); nil, nil without one.
func ReadPicture(path string) (*Picture, error) {
	f, size, c, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, pic, err := safeRead(c, f, size)
	if err != nil || pic == nil {
		return nil, err
	}
	p := &Picture{MIME: pic.MIME, Type: pic.Type, Desc: pic.Desc, Data: pic.Data}
	if pic.hasOffset {
		if pic.Len > 16<<20 {
			return nil, ErrTooBig
		}
		p.Data = make([]byte, pic.Len)
		if _, err := f.ReadAt(p.Data, pic.Off); err != nil {
			return nil, err
		}
	}
	if pi := pictureInfo(p.Data); pi.MIME != "" {
		p.MIME = pi.MIME
	}
	return p, nil
}

// PictureFile is a cover's bytes as they are sent, read from the song a piece at a time; Close it.
type PictureFile struct {
	io.ReadSeeker
	MIME string
	Size int64
	f    *os.File
}

func (p *PictureFile) Close() error {
	if p.f != nil {
		return p.f.Close()
	}
	return nil
}

// OpenPicture is path's front cover (else its first picture) to send; nil, nil without one.
func OpenPicture(path string) (*PictureFile, error) {
	f, size, c, err := open(path)
	if err != nil {
		return nil, err
	}
	_, pic, err := safeRead(c, f, size)
	if err != nil || pic == nil {
		f.Close()
		return nil, err
	}
	if !pic.hasOffset {
		f.Close()
		mime := pictureInfo(pic.Data).MIME
		if mime == "" {
			mime = declared(pic.MIME)
		}
		return &PictureFile{ReadSeeker: bytes.NewReader(pic.Data), MIME: mime, Size: int64(len(pic.Data))}, nil
	}
	head := make([]byte, min(pic.Len, 32))
	n, _ := f.ReadAt(head, pic.Off)
	mime := pictureInfo(head[:n]).MIME
	if mime == "" {
		mime = declared(pic.MIME)
	}
	return &PictureFile{ReadSeeker: io.NewSectionReader(f, pic.Off, pic.Len), MIME: mime, Size: pic.Len, f: f}, nil
}

// Clean is v as Write keeps it in f: valid UTF-8 without NULs; one line, trimmed, but for Lyrics; a
// number without leading zeros.
func Clean(f Field, v string) string { return clean(f, v) }

func clean(f Field, v string) string {
	v = strings.ToValidUTF8(strings.NewReplacer("\x00", "", string(rune(0xFEFF)), "").Replace(v), "")
	if f == Lyrics {
		return strings.TrimRight(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(v), "\n ")
	}
	v = strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(v))
	if numeric(f) {
		v = strings.TrimLeft(v, "0") // "03" is 3; "0", none
	}
	return v
}

// Check is why c can't be written, nil when it can: an unknown field, a number that isn't one, a value
// too long, a picture that isn't a JPEG or PNG or is too big.
func Check(c Changes) error {
	for f, v := range c.Set {
		if !Known(f) {
			return fmt.Errorf("tags: no field %q", f)
		}
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("tags: %s isn't text", f.Name())
		}
		if f == Lyrics && len(v) > maxLyrics || f != Lyrics && len(v) > maxValue {
			return fmt.Errorf("tags: %s is too long", f.Name())
		}
		if v = strings.TrimSpace(v); numeric(f) && v != "" {
			if len(v) > 4 || strings.Trim(v, "0123456789") != "" {
				return fmt.Errorf("tags: %s must be a number", f.Name())
			}
		}
		// The Q2 takes a song's year from its date's first four digits, and ID3v2.3 keeps only those.
		if f == Date && v != "" && (len(v) < 4 || strings.Trim(v[:4], "0123456789") != "" ||
			len(v) > 4 && v[4] >= '0' && v[4] <= '9') {
			return errors.New("tags: Year must start with a 4-digit year, like 2004 or 2004-05-06")
		}
	}
	if c.Picture != nil {
		if len(c.Picture.Data) > MaxPicture {
			return fmt.Errorf("tags: the picture is over %d MB", MaxPicture>>20)
		}
		if pi := pictureInfo(c.Picture.Data); pi.MIME == "" {
			return errors.New("tags: the picture must be a JPEG or a PNG")
		}
	}
	return nil
}

// Write makes c's changes to path's tags.
func Write(path string, c Changes) error {
	cd := codecFor(path)
	if cd == nil {
		return ErrUnsupported
	}
	if err := Check(c); err != nil {
		return err
	}
	if c.Empty() {
		return nil
	}
	set := make(map[Field]string, len(c.Set))
	for f, v := range c.Set {
		set[f] = clean(f, v)
	}
	c.Set = set
	if c.Picture != nil {
		p := *c.Picture
		p.MIME, p.Type = pictureInfo(p.Data).MIME, 3
		c.Picture = &p
	}
	if !CardReady() {
		return ErrNoCard
	}
	unlock := lock(path)
	defer unlock()
	// A value the song has already is left alone, so saving what was read writes nothing.
	if f, size, _, err := open(path); err == nil {
		info, _, rerr := safeRead(cd, f, size)
		f.Close()
		if rerr == nil && info.Writable {
			for k, v := range c.Set {
				if clean(k, info.Fields[k]) == v {
					delete(c.Set, k)
				}
			}
			if c.Empty() {
				return nil
			}
		}
	}
	return safeWrite(cd, path, c)
}

// A song's lock: the card's FAT or exFAT doesn't tell case apart, so neither does its key; an
// entry goes once nobody holds it or waits for it.
type pathLock struct {
	mu sync.Mutex
	n  int
}

var (
	locksMu sync.Mutex
	locks   = map[string]*pathLock{}
)

// lock holds path against another write until the returned func is called.
func lock(path string) func() {
	key := strings.ToLower(filepath.Clean(path))
	locksMu.Lock()
	l := locks[key]
	if l == nil {
		l = &pathLock{}
		locks[key] = l
	}
	l.n++
	locksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		locksMu.Lock()
		if l.n--; l.n == 0 {
			delete(locks, key)
		}
		locksMu.Unlock()
	}
}

// Stopping is whether the app is ending: a rewrite under way gives up (path as it was), so the app
// can go at once; the app points it at its own.
var Stopping = func() bool { return false }

// ErrStopped is a rewrite given up because the app is ending.
var ErrStopped = errors.New("tags: File Browser is turning off")

// patch writes data at off in path, whose length must not change: the room was there.
func patch(path string, off int64, data []byte) error {
	if !CardReady() {
		return ErrNoCard
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil && off+int64(len(data)) > st.Size() {
		err = fmt.Errorf("tags: a patch past the end (%d+%d > %d)", off, len(data), st.Size())
	}
	if err == nil {
		_, err = f.WriteAt(data, off)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// patchWith writes n bytes at off in path, in place, as build writes them, a piece at a time: what
// is kept (a big cover) is copied rather than held. build reads the file as it is from src, and may
// read a byte only before the writing reaches it: what it copies must keep its place or move towards
// the start, never towards the end. The file's length doesn't change.
func patchWith(path string, off, n int64, build func(src *os.File, w io.Writer) error) error {
	if !CardReady() {
		return ErrNoCard
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil && off+n > st.Size() {
		err = fmt.Errorf("tags: a patch past the end (%d+%d > %d)", off, n, st.Size())
	}
	if err == nil {
		at := &atWriter{f: f, off: off, end: off + n}
		bw := bufio.NewWriterSize(at, 64<<10)
		if err = build(src, bw); err == nil {
			err = bw.Flush()
		}
		if err == nil && at.off != at.end {
			err = fmt.Errorf("tags: patched %d bytes, meant %d", n-(at.end-at.off), n)
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// atWriter writes in order from off, refusing to pass end.
type atWriter struct {
	f        *os.File
	off, end int64
}

func (a *atWriter) Write(b []byte) (int, error) {
	if a.off+int64(len(b)) > a.end {
		return 0, fmt.Errorf("tags: a patch past its room")
	}
	n, err := a.f.WriteAt(b, a.off)
	a.off += int64(n)
	return n, err
}

// free is the room on the filesystem holding path, -1 when it can't tell.
var free = func(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(filepath.Dir(path), &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// partName is where rewrite builds path's new copy: hidden beside it, with the suffix the file browser
// hides and the Q2's scan never takes for a song.
func partName(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".q2part")
}

// rewrite writes path again: build gets the file as it is and writes the whole new one, of newSize
// bytes (the free room checked for it), beside it; that is synced and renamed over path, its mode
// kept. A failure leaves path as it was.
func rewrite(path string, newSize int64, build func(src *os.File, w io.Writer) error) (err error) {
	if !CardReady() {
		return ErrNoCard
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	part := partName(path)
	os.Remove(part) // one a save cut short left: its room is free again
	if room := free(path); room >= 0 && newSize+Spare > room {
		return ErrNoRoom
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(part)
		}
	}()
	bw := bufio.NewWriterSize(out, 256<<10)
	cw := &countWriter{w: bw}
	if err = build(src, cw); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if newSize >= 0 && cw.n != newSize {
		return fmt.Errorf("tags: wrote %d bytes, meant %d", cw.n, newSize)
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if !CardReady() {
		return ErrNoCard
	}
	if Stopping() {
		return ErrStopped
	}
	return os.Rename(part, path)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	if Stopping() {
		return 0, ErrStopped
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// copyRange copies n bytes at off in src to w.
func copyRange(w io.Writer, src io.ReaderAt, off, n int64) error {
	k, err := io.Copy(w, io.NewSectionReader(src, off, n))
	if err == nil && k != n {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// readAt is n bytes at off, or ErrCorrupt when the file is shorter.
func readAt(r io.ReaderAt, off, n int64) ([]byte, error) {
	if n < 0 || n > 64<<20 {
		return nil, ErrCorrupt
	}
	b := make([]byte, n)
	k, err := r.ReadAt(b, off)
	if int64(k) == n {
		return b, nil
	}
	if err == nil || err == io.EOF {
		err = ErrCorrupt
	}
	return nil, err
}

// pictureInfo sniffs a JPEG or PNG from its first bytes; MIME is "" for anything else.
func pictureInfo(b []byte) PictureInfo {
	pi := PictureInfo{Size: len(b)}
	switch {
	case len(b) >= 24 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) && string(b[12:16]) == "IHDR":
		pi.MIME = "image/png"
		pi.Width, pi.Height = int(binary.BigEndian.Uint32(b[16:])), int(binary.BigEndian.Uint32(b[20:]))
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		pi.MIME = "image/jpeg"
		for i := 2; i+9 < len(b); {
			if b[i] != 0xFF {
				break
			}
			m := b[i+1]
			if m == 0xFF {
				i++
				continue
			}
			if m == 0xD8 || m == 0x01 || m >= 0xD0 && m <= 0xD7 {
				i += 2
				continue
			}
			n := int(binary.BigEndian.Uint16(b[i+2:]))
			// SOF0-SOF15 but DHT (C4), JPG (C8) and DAC (CC); SOF2/6/10/14 are progressive.
			if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
				pi.Height, pi.Width = int(binary.BigEndian.Uint16(b[i+5:])), int(binary.BigEndian.Uint16(b[i+7:]))
				pi.Progressive = m == 0xC2 || m == 0xC6 || m == 0xCA || m == 0xCE
				break
			}
			if m == 0xDA || n < 2 {
				break
			}
			i += 2 + n
		}
	}
	return pi
}

// PictureNote is why the Q2 may not show a picture, "" when it should: its own thumbnailer takes a
// baseline JPEG up to 6 MB, or a PNG up to 1 MB and 1024 px.
func PictureNote(pi *PictureInfo) string {
	switch {
	case pi == nil:
		return ""
	case pi.MIME == "image/jpeg" && pi.Progressive:
		return "The cover is a progressive JPEG, which some of the Q2's views don't show"
	case pi.MIME == "image/png" && (pi.Size > 1<<20 || pi.Width > 1024 || pi.Height > 1024):
		return "The cover is a big PNG, which some of the Q2's views don't show"
	case pi.MIME == "image/jpeg" && pi.Size > 6<<20:
		return "The cover is over 6 MB, which some of the Q2's views don't show"
	case pi.MIME != "image/jpeg" && pi.MIME != "image/png":
		if k := pictureKinds[pi.MIME]; k != "" {
			return "The cover is a " + k + ", which the Q2 doesn't show: pick a new one"
		}
		return "The cover isn't a JPEG or a PNG, so the Q2 doesn't show it: pick a new one"
	}
	return ""
}

// pictureKinds are the pictures other than a JPEG or a PNG a song may hold, named.
var pictureKinds = map[string]string{"image/gif": "GIF", "image/bmp": "BMP", "image/x-ms-bmp": "BMP",
	"image/webp": "WebP", "image/tiff": "TIFF", "image/heic": "HEIC", "image/avif": "AVIF"}

// declared is the type a song says its picture is, when its bytes aren't a JPEG or a PNG: a JPEG or
// PNG that isn't one is unknown.
func declared(mime string) string {
	switch m := strings.ToLower(strings.TrimSpace(mime)); m {
	case "image/jpeg", "image/jpg", "image/png", "":
		return "application/octet-stream"
	default:
		return m
	}
}

// guard turns a codec's panic, on a file laid out as no format allows, into ErrCorrupt: one bad song
// mustn't end the app.
func guard(err *error) {
	if r := recover(); r != nil {
		log.Printf("tags: %v", r)
		*err = ErrCorrupt
	}
}

func safeRead(c codec, r io.ReaderAt, size int64) (info *Info, pic *picRef, err error) {
	defer guard(&err)
	return c.read(r, size)
}

func safeWrite(c codec, path string, ch Changes) (err error) {
	defer guard(&err)
	return c.write(path, ch)
}

// genres are ID3v1's, with Winamp's additions, by number.
var genres = []string{"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge", "Hip-Hop",
	"Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B", "Rap", "Reggae", "Rock", "Techno",
	"Industrial", "Alternative", "Ska", "Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient",
	"Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical", "Instrumental", "Acid", "House",
	"Game", "Sound Clip", "Gospel", "Noise", "Alternative Rock", "Bass", "Soul", "Punk", "Space",
	"Meditative", "Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic", "Darkwave",
	"Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream", "Southern Rock", "Comedy", "Cult",
	"Gangsta", "Top 40", "Christian Rap", "Pop/Funk", "Jungle", "Native American", "Cabaret", "New Wave",
	"Psychedelic", "Rave", "Showtunes", "Trailer", "Lo-Fi", "Tribal", "Acid Punk", "Acid Jazz", "Polka",
	"Retro", "Musical", "Rock & Roll", "Hard Rock", "Folk", "Folk-Rock", "National Folk", "Swing",
	"Fast Fusion", "Bebop", "Latin", "Revival", "Celtic", "Bluegrass", "Avantgarde", "Gothic Rock",
	"Progressive Rock", "Psychedelic Rock", "Symphonic Rock", "Slow Rock", "Big Band", "Chorus",
	"Easy Listening", "Acoustic", "Humour", "Speech", "Chanson", "Opera", "Chamber Music", "Sonata",
	"Symphony", "Booty Bass", "Primus", "Porn Groove", "Satire", "Slow Jam", "Club", "Tango", "Samba",
	"Folklore", "Ballad", "Power Ballad", "Rhythmic Soul", "Freestyle", "Duet", "Punk Rock", "Drum Solo",
	"A Cappella", "Euro-House", "Dance Hall", "Goa", "Drum & Bass", "Club-House", "Hardcore Techno",
	"Terror", "Indie", "Britpop", "Afro-Punk", "Polsk Punk", "Beat", "Christian Gangsta Rap",
	"Heavy Metal", "Black Metal", "Crossover", "Contemporary Christian", "Christian Rock", "Merengue",
	"Salsa", "Thrash Metal", "Anime", "J-Pop", "Synthpop"}

// genreName is ID3v1 genre n's name, "" past the list.
func genreName(n int) string {
	if n < 0 || n >= len(genres) {
		return ""
	}
	return genres[n]
}

// Genres is the ID3v1 list, for an editor's suggestions.
func Genres() []string { return append([]string(nil), genres...) }

// splitNum reads "3/12" as "3" and "12", "3" as "3" and "".
func splitNum(v string) (n, of string) {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '/'); i >= 0 {
		return clean(Track, v[:i]), clean(Track, v[i+1:])
	}
	return clean(Track, v), ""
}
