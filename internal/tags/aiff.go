package tags

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// AIFF, AIFF-C and WAV are chunks: a FORM or RIFF header (the length of what follows, then "AIFF",
// "AIFC" or "WAVE"), then chunks of an id, a size (big-endian in AIFF, little-endian in WAV) and a
// body padded to an even length. Their tags are an ID3v2 tag in a chunk of its own, "ID3 " or "id3 ",
// which ffmpeg and mutagen put after the audio. A tag that fits its chunk is written there, zeros
// taking up the rest; otherwise the file is written again with the chunk replaced where it was, or
// added at the end, with 2 KB of padding in the tag for next time and the FORM or RIFF size set to
// the new length. Every other chunk, and the audio, is copied as it was. Which of the two a file is
// comes from its first bytes, not its name. Older text (AIFF's NAME, AUTH and ANNO chunks, WAV's LIST
// INFO) is shown when there's no ID3 tag and starts a new one; it is never changed.

const (
	iffSpare     = 2 << 10
	iffMaxChunks = 1024 // past this many the walk stops and the file isn't written: real ones have a few
	iffTooMany   = "It has too many chunks, so its tags can't be changed here"
)

func init() { register(iffCodec{}, ".aiff", ".aif", ".aifc") }

// iffCodec is AIFF's and WAV's; wav.go registers it for .wav.
type iffCodec struct{}

type iffChunk struct {
	id   string
	off  int64 // its header's offset
	size int64 // its body's length
	next int64 // where the chunk after it starts: past its pad byte when the file has one
}

type iffFile struct {
	kind   string // "AIFF", "AIFF-C", "WAV"
	big    bool   // sizes are big-endian
	rf64   bool   // an RF64 WAV, its sizes in a ds64 chunk
	end    int64  // where the chunks end
	chunks []iffChunk
	id3    *iffChunk        // the first ID3 chunk, nil without
	tag    *id3Tag          // its tag; nil without, or when it can't be read
	text   map[Field]string // what the older text chunks say
	from   []string         // the ids of those that say something: "NAME", "LIST"
	notes  []string
	odd    string // why it can't be written, "" when it can
}

func (ff *iffFile) u32(b []byte) int64 {
	if ff.big {
		return int64(binary.BigEndian.Uint32(b))
	}
	return int64(binary.LittleEndian.Uint32(b))
}

func (ff *iffFile) put32(b []byte, n int64) {
	if ff.big {
		binary.BigEndian.PutUint32(b, uint32(n))
	} else {
		binary.LittleEndian.PutUint32(b, uint32(n))
	}
}

// refuse records why the file can't be written, the first reason kept.
func (ff *iffFile) refuse(why string) {
	if ff.odd == "" {
		ff.odd = why
	}
}

// iffID is whether id is a chunk id: four printable ASCII characters.
func iffID(id string) bool {
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7E {
			return false
		}
	}
	return len(id) == 4
}

// le64 is a little-endian 64-bit size, -1 past what a file could hold.
func le64(b []byte) int64 {
	n := binary.LittleEndian.Uint64(b)
	if n > 1<<62 {
		return -1
	}
	return int64(n)
}

func readIFF(r io.ReaderAt, size int64) (*iffFile, error) {
	h, err := readAt(r, 0, 12)
	if err != nil {
		return nil, ErrCorrupt
	}
	ff := &iffFile{}
	switch string(h[:4]) + string(h[8:12]) {
	case "FORMAIFF":
		ff.kind, ff.big = "AIFF", true
	case "FORMAIFC":
		ff.kind, ff.big = "AIFF-C", true
	case "RIFFWAVE":
		ff.kind = "WAV"
	case "RF64WAVE", "BW64WAVE":
		ff.kind, ff.rf64 = "WAV", true
	default:
		return nil, ErrCorrupt
	}
	declared := 8 + ff.u32(h[4:])
	var data64 int64 // RF64's data size
	if ff.rf64 {
		ff.refuse("It's an RF64 WAV, whose tags can't be changed here")
		ds, err := readAt(r, 12, 28) // "ds64", its size, the RIFF size, the data size, ...
		if err != nil || string(ds[:4]) != "ds64" {
			return nil, ErrCorrupt
		}
		if declared == 8+0xFFFFFFFF {
			declared = 8 + le64(ds[8:])
		}
		data64 = le64(ds[16:])
	}
	if declared < 12 {
		return nil, ErrCorrupt
	}
	ff.end = min(declared, size)
	off := int64(12)
	for off+8 <= ff.end {
		if len(ff.chunks) == iffMaxChunks {
			ff.refuse(iffTooMany)
			break
		}
		b, err := readAt(r, off, 8)
		if err != nil {
			return nil, err
		}
		c := iffChunk{id: string(b[:4]), off: off, size: ff.u32(b[4:])}
		if ff.rf64 && c.id == "data" && c.size == 0xFFFFFFFF {
			c.size = data64
		}
		if !iffID(c.id) || c.size < 0 || off+8+c.size > ff.end {
			break
		}
		c.next = off + 8 + c.size
		if c.size&1 != 0 && c.next < ff.end {
			c.next++
		}
		ff.chunks = append(ff.chunks, c)
		off = c.next
	}
	switch {
	case declared > size && !(declared == size+1 && off == size && size&1 != 0):
		// Longer than the file, unless by the pad byte of an odd last chunk that has none.
		ff.refuse("It's shorter than its header says, so its tags can't be changed here")
	case off != ff.end:
		ff.refuse("Its chunks don't fill it as its header says, so its tags can't be changed here")
	}
	second := false
	for i := range ff.chunks {
		if c := &ff.chunks[i]; strings.EqualFold(c.id, "id3 ") {
			if ff.id3 == nil {
				ff.id3 = c
			} else {
				second = true
			}
		}
	}
	if c := ff.id3; c != nil {
		t, _, err := loadID3At(r, c.off+8, c.size)
		switch {
		case err == ErrCorrupt:
			ff.refuse("Its ID3 chunk doesn't hold a tag this can read, so its tags can't be changed here")
		case err != nil:
			return nil, err
		default:
			ff.tag = t
		}
	}
	if ff.id3 == nil && len(ff.chunks) >= iffMaxChunks {
		// A save would add the ID3 chunk past the most the walk reads, and the tag it wrote would never
		// be read again.
		ff.refuse(iffTooMany)
	}
	if second {
		ff.notes = append(ff.notes, "It has a second ID3 chunk, which is left as it is")
	}
	if ff.big {
		ff.text, ff.from = aiffText(r, ff.chunks)
	} else {
		ff.text, ff.from = wavInfo(r, ff.chunks)
	}
	return ff, nil
}

// loadID3At is the tag that starts the room bytes at off, and its length; ErrCorrupt when they
// don't start with one that fits.
func loadID3At(r io.ReaderAt, off, room int64) (*id3Tag, int64, error) {
	if room < 10 {
		return nil, 0, ErrCorrupt
	}
	h, err := readAt(r, off, 10)
	if err != nil {
		return nil, 0, err
	}
	n := id3Len(h)
	if n == 0 || n > room {
		return nil, 0, ErrCorrupt
	}
	t, err := loadID3(r, off, n)
	return t, n, err
}

func (iffCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	ff, err := readIFF(r, size)
	if err != nil {
		return nil, nil, err
	}
	info := &Info{Format: ff.kind, Writable: ff.odd == "", Fields: map[Field]string{}}
	var pic *picRef
	switch {
	case ff.tag != nil:
		info.Format = ff.kind + " (" + ff.tag.format() + ")"
		info.Fields, pic = ff.tag.fields(), ff.tag.picture()
		info.Notes = append(info.Notes, ff.tag.notes()...)
		if len(ff.text) > 0 {
			info.Notes = append(info.Notes, "It has "+ff.textName()+" too, which saving leaves as it is: "+
				"some players show what it says for a field left empty here")
		}
	case len(ff.text) > 0:
		info.Format = ff.kind + " (RIFF INFO)"
		if ff.big {
			info.Format = ff.kind + " (text chunks)"
		}
		for k, v := range ff.text {
			info.Fields[k] = v
		}
		if info.Writable {
			info.Notes = append(info.Notes, "It has "+ff.textName()+" only: saving adds an ID3 tag with its fields and leaves it as it is")
		}
	case info.Writable:
		info.Format = ff.kind + " (no tag yet)"
	}
	if ff.odd != "" {
		info.Notes = append(info.Notes, ff.odd)
	}
	info.Notes = append(info.Notes, ff.notes...)
	return info, pic, nil
}

// textName is the older text as a note names it: "a RIFF INFO list", "AIFF NAME and AUTH chunks".
func (ff *iffFile) textName() string {
	n := len(ff.from)
	switch {
	case !ff.big:
		return "a RIFF INFO list"
	case n == 1:
		return "an AIFF " + ff.from[0] + " chunk"
	}
	return "AIFF " + strings.Join(ff.from[:n-1], ", ") + " and " + ff.from[n-1] + " chunks"
}

func (iffCodec) write(path string, c Changes) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	ff, err := readIFF(f, size)
	if err != nil {
		return err
	}
	if ff.odd != "" {
		return ErrReadOnly
	}
	t := ff.tag
	if t == nil {
		// A new tag starts with what the older text says, so a player reading the new one loses none of it.
		t = newID3()
		t.apply(Changes{Set: ff.text})
	}
	t.apply(c)
	if ch := ff.id3; ch != nil && t.needed() <= ch.size {
		if err := patchID3(path, ch.off+8, t, ch.size); err != errPatchID3 {
			return err
		}
	}
	f.Close()
	// Otherwise the chunk is replaced where it was, or a new one added at the end (after the pad byte
	// an odd last chunk is missing).
	n := t.needed() + iffSpare
	n += n & 1
	at, cut, pad, id := ff.end, ff.end, ff.end&1, "id3 "
	if ff.big {
		id = "ID3 "
	}
	if ch := ff.id3; ch != nil {
		at, cut, pad, id = ch.off, ch.next, 0, ch.id
	}
	form := ff.end - 8 - (cut - at) + pad + 8 + n
	if form > 0xFFFFFFFF {
		return ErrTooBig
	}
	newSize := size - (cut - at) + pad + 8 + n
	return rewrite(path, newSize, func(src *os.File, w io.Writer) error {
		if t.src != nil && t.src.r != nil {
			t.src.r = src // the same file: the frames left there are where they were
		}
		h, err := readAt(src, 0, 4)
		if err != nil {
			return err
		}
		h = append(h, 0, 0, 0, 0)
		ff.put32(h[4:], form)
		if _, err := w.Write(h); err != nil {
			return err
		}
		if err := copyRange(w, src, 8, at-8); err != nil {
			return err
		}
		ch := make([]byte, pad+8)
		copy(ch[pad:], id)
		ff.put32(ch[pad+4:], n)
		if _, err := w.Write(ch); err != nil {
			return err
		}
		if err := t.writeTo(w, n); err != nil {
			return err
		}
		return copyRange(w, src, cut, size-cut)
	})
}

// errPatchID3 is a tag patchID3 can't write in place without holding what it keeps: the file is
// written again instead.
var errPatchID3 = errors.New("tags: the tag can't be written over the old one")

// patchID3 writes t over the room bytes at off its old tag had, in place, zeros padding it. The
// frames are laid out (id3.go's fits) so that one left in the file, a picture, is copied as the
// writing passes it, never held; errPatchID3 when they can't be.
func patchID3(path string, off int64, t *id3Tag, room int64) error {
	if room-10 > id3MaxSize {
		return ErrTooBig
	}
	if !t.fits(room) {
		return errPatchID3
	}
	if Stopping() {
		return ErrStopped
	}
	return t.patchTo(path, off, room)
}

// iffText is a text chunk's or INFO item's value: up to a NUL, UTF-8 when it is that (ffmpeg writes
// it), else Latin-1.
func iffText(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if utf8.Valid(b) {
		return strings.TrimSpace(string(b))
	}
	return strings.TrimSpace(id3Latin1(b))
}

// aiffText are the fields AIFF's text chunks hold (NAME the title, AUTH the artist, the first ANNO
// the comment), and the ids of those that hold one. Only the first chunk of each is read.
func aiffText(r io.ReaderAt, chunks []iffChunk) (map[Field]string, []string) {
	out := map[Field]string{}
	tried := map[Field]bool{}
	var ids []string
	for _, c := range chunks {
		f := map[string]Field{"NAME": Title, "AUTH": Artist, "ANNO": Comment}[c.id]
		if f == "" || tried[f] {
			continue
		}
		tried[f] = true
		if c.size > 64<<10 {
			continue
		}
		if b, err := readAt(r, c.off+8, c.size); err == nil {
			if v := iffText(b); v != "" {
				out[f] = v
				ids = append(ids, c.id)
			}
		}
	}
	return out, ids
}
