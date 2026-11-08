package tags

import (
	"encoding/binary"
	"io"
	"os"
)

// DSF (Sony's DSD stream file): a "DSD " chunk holding the file's length and where its tag is, a
// "fmt " chunk, a "data" chunk with the audio, then the ID3v2 tag that pointer leads to, at the end. A
// tag that fits the old one's room is written there; otherwise the file is written again with the new
// tag where the old one was (at the end, for a file without one), 2 KB of padding for next time, and
// the header's length and pointer set to match. The audio is copied as it was.

const dsfSpare = 2 << 10

func init() { register(dsfCodec{}, ".dsf") }

type dsfCodec struct{}

type dsfFile struct {
	head    []byte // the "DSD " chunk: "DSD ", 28, the file's length, the tag's place, all little-endian
	dataEnd int64  // where the audio ends
	ptr     int64  // where the tag is, 0 without
	tagLen  int64
	tag     *id3Tag
	odd     string // why it can't be written, "" when it can
}

func (d *dsfFile) refuse(why string) {
	if d.odd == "" {
		d.odd = why
	}
}

func readDSF(r io.ReaderAt, size int64) (*dsfFile, error) {
	h, err := readAt(r, 0, 28)
	if err != nil || string(h[:4]) != "DSD " || le64(h[4:]) != 28 {
		return nil, ErrCorrupt
	}
	d := &dsfFile{head: h}
	fh, err := readAt(r, 28, 12)
	if err != nil || string(fh[:4]) != "fmt " {
		return nil, ErrCorrupt
	}
	fmtSize := le64(fh[4:])
	if fmtSize < 12 || fmtSize > 1<<20 {
		return nil, ErrCorrupt
	}
	dh, err := readAt(r, 28+fmtSize, 12)
	if err != nil || string(dh[:4]) != "data" {
		return nil, ErrCorrupt
	}
	dataSize := le64(dh[4:])
	if dataSize < 12 {
		return nil, ErrCorrupt
	}
	d.dataEnd = 28 + fmtSize + dataSize
	if d.dataEnd > size {
		d.refuse("It's shorter than its header says, so its tags can't be changed here")
	}
	ptr := le64(h[20:])
	switch {
	case ptr == 0:
		return d, nil
	case ptr < 28+fmtSize+12 || ptr >= size:
		d.refuse("Its header doesn't lead to its tag, so its tags can't be changed here")
		return d, nil
	case ptr < d.dataEnd:
		d.refuse("Its tag is inside its audio, so its tags can't be changed here")
	}
	t, n, err := loadID3At(r, ptr, size-ptr)
	switch {
	case err == ErrCorrupt:
		d.refuse("Its ID3 tag can't be read, so its tags can't be changed here")
	case err != nil:
		return nil, err
	default:
		d.ptr, d.tagLen, d.tag = ptr, n, t
	}
	return d, nil
}

func (dsfCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	d, err := readDSF(r, size)
	if err != nil {
		return nil, nil, err
	}
	info := &Info{Format: "DSF", Writable: d.odd == "", Fields: map[Field]string{}}
	var pic *picRef
	switch {
	case d.tag != nil:
		info.Format = "DSF (" + d.tag.format() + ")"
		info.Fields, pic = d.tag.fields(), d.tag.picture()
		info.Notes = append(info.Notes, d.tag.notes()...)
	case info.Writable:
		info.Format = "DSF (no tag yet)"
	}
	if d.odd != "" {
		info.Notes = append(info.Notes, d.odd)
	}
	return info, pic, nil
}

func (dsfCodec) write(path string, c Changes) error {
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
	d, err := readDSF(f, size)
	if err != nil {
		return err
	}
	if d.odd != "" {
		return ErrReadOnly
	}
	t := d.tag
	if t == nil {
		t = newID3()
	}
	t.apply(c)
	if d.tag != nil && t.needed() <= d.tagLen {
		if err := patchID3(path, d.ptr, t, d.tagLen); err != errPatchID3 {
			return err
		}
	}
	f.Close()
	n := t.needed() + dsfSpare
	at, cut := size, size
	if d.tag != nil {
		at, cut = d.ptr, d.ptr+d.tagLen
	}
	newSize := size - (cut - at) + n
	head := append([]byte(nil), d.head...)
	binary.LittleEndian.PutUint64(head[12:], uint64(newSize))
	binary.LittleEndian.PutUint64(head[20:], uint64(at))
	return rewrite(path, newSize, func(src *os.File, w io.Writer) error {
		if t.src != nil && t.src.r != nil {
			t.src.r = src // the same file: the frames left there are where they were
		}
		if _, err := w.Write(head); err != nil {
			return err
		}
		if err := copyRange(w, src, 28, at-28); err != nil {
			return err
		}
		if err := t.writeTo(w, n); err != nil {
			return err
		}
		return copyRange(w, src, cut, size-cut)
	})
}
