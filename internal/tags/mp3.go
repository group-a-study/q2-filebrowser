package tags

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MP3, MP2 and AAC (ADTS): an ID3v2 tag at the start, and on some files an ID3v1 tag in the last 128
// bytes. The ID3v2 tag is written in place when the new one fits the room the old one took, its
// padding taking up the difference: a big cover is copied where it goes a piece at a time, the few
// bytes a longer frame before it pushes it along held as the writing passes them (id3.go's fits and
// patchTo). Otherwise the file is written again, the audio after a new tag with 2 KB of padding for
// next time. A file without one gets an ID3v2.4 tag. An ID3v1 tag is kept in step with the fields
// that change, in its own 128 bytes, so a cleared field can't show through there; one is never added
// or removed. Only the first of several ID3v2 tags is read and changed, and an APEv2 tag is left as
// it is.

// mpegSpare is the padding a tag written again leaves for next time.
const mpegSpare = 2 << 10

func init() {
	register(mpegCodec{"MP3"}, ".mp3")
	register(mpegCodec{"MP2"}, ".mp2")
	register(mpegCodec{"AAC"}, ".aac")
}

type mpegCodec struct{ kind string }

// mpegFile is what is around an MPEG file's audio.
type mpegFile struct {
	tag    *id3Tag // the ID3v2 tag at the start, nil without
	tagLen int64   // its length, header to footer
	second bool    // another ID3v2 tag follows it
	v1     []byte  // the ID3v1 tag's 128 bytes, nil without
	ape    bool    // an APEv2 tag at the end
	other  string  // what a file without an ID3v2 tag is when it isn't MPEG audio: "WAV", "MP4"
}

func readMPEG(r io.ReaderAt, size int64) (*mpegFile, error) {
	m := &mpegFile{}
	if size >= 10 {
		h, err := readAt(r, 0, 10)
		if err != nil {
			return nil, err
		}
		if string(h[:3]) == "ID3" {
			n := id3Len(h)
			if n == 0 || n > size {
				return nil, ErrCorrupt
			}
			if m.tag, err = loadID3(r, 0, n); err != nil {
				return nil, err
			}
			m.tagLen = n
			if next, err := readAt(r, n, 10); err == nil && id3Len(next) > 0 {
				m.second = true
			}
		} else {
			m.other = mpegOther(h)
		}
	}
	if size-m.tagLen >= 128 {
		b, err := readAt(r, size-128, 128)
		if err != nil {
			return nil, err
		}
		if string(b[:3]) == "TAG" {
			m.v1 = b
		}
	}
	at := size - 32
	if m.v1 != nil {
		at -= 128
	}
	if at >= m.tagLen {
		if b, err := readAt(r, at, 8); err == nil && string(b) == "APETAGEX" {
			m.ape = true
		}
	}
	return m, nil
}

// mpegOther is the format a file is, by its first bytes, when that isn't MPEG audio: a tag put in
// front of it would spoil it. "" for anything else.
func mpegOther(h []byte) string {
	if string(h[4:8]) == "ftyp" {
		return "MP4"
	}
	return map[string]string{"RIFF": "WAV", "fLaC": "FLAC", "OggS": "Ogg", "FORM": "AIFF", "MAC ": "APE",
		"DSD ": "DSF", "FRM8": "DFF", "wvpk": "WavPack", "\x30\x26\xb2\x75": "WMA"}[string(h[:4])]
}

// mpegAsMP4 is the MP4 codec, for a .aac file that is an MP4 inside; nil when there's none.
func mpegAsMP4() codec { return codecs[".m4a"] }

func (c mpegCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	m, err := readMPEG(r, size)
	if err != nil {
		return nil, nil, err
	}
	if m.other == "MP4" && mpegAsMP4() != nil {
		return mpegAsMP4().read(r, size)
	}
	info := &Info{Format: c.kind + " (no tag yet)", Writable: m.other == "", Fields: map[Field]string{}}
	var pic *picRef
	switch {
	case m.other != "":
		info.Format = c.kind
		info.Notes = append(info.Notes, "Inside it's "+m.other+", not "+c.kind+", so its tags can't be changed here")
	case m.tag != nil:
		info.Format = c.kind + " (" + m.tag.format() + ")"
		info.Fields, pic = m.tag.fields(), m.tag.picture()
		info.Notes = append(info.Notes, m.tag.notes()...)
	case m.v1 != nil:
		info.Format = c.kind + " (ID3v1)"
		info.Fields = id3v1Fields(m.v1)
		info.Notes = append(info.Notes, "It has an old ID3v1 tag only: saving adds an ID3v2.4 tag and keeps the ID3v1 one in step")
	}
	if m.second {
		info.Notes = append(info.Notes, "It has a second ID3v2 tag after the first, which is left as it is")
	}
	if m.ape {
		info.Notes = append(info.Notes, "It has an APEv2 tag too, which is left as it is")
	}
	return info, pic, nil
}

func (c mpegCodec) write(path string, ch Changes) error {
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
	m, err := readMPEG(f, size)
	if err != nil {
		return err
	}
	if m.other == "MP4" && mpegAsMP4() != nil {
		f.Close()
		return mpegAsMP4().write(path, ch)
	}
	if m.other != "" {
		return ErrReadOnly
	}
	t := m.tag
	if t == nil {
		// A new tag starts with what an ID3v1 tag says, so the Q2, reading the new one, loses none of it.
		t = newID3()
		if m.v1 != nil {
			t.apply(Changes{Set: id3v1Fields(m.v1)})
		}
	}
	t.apply(ch)
	var v1 []byte
	if m.v1 != nil {
		v1 = id3v1Update(m.v1, ch.Set)
	}
	if m.tag != nil && t.fits(m.tagLen) {
		// In place: the new tag, padded to the old one's length, big frames copied from where they are
		// a piece at a time.
		f.Close()
		if err := t.patchTo(path, 0, m.tagLen); err != nil {
			return err
		}
		if v1 != nil && !bytes.Equal(v1, m.v1) {
			return patch(path, size-128, v1)
		}
		return nil
	}
	f.Close()
	tagLen := t.needed() + mpegSpare
	end := size - int64(len(v1))
	return rewrite(path, tagLen+end-m.tagLen+int64(len(v1)), func(src *os.File, w io.Writer) error {
		if t.src != nil && t.src.r != nil {
			t.src.r = src // the same file: lazy frames are where they were
		}
		if err := t.writeTo(w, tagLen); err != nil {
			return err
		}
		if err := copyRange(w, src, m.tagLen, end-m.tagLen); err != nil {
			return err
		}
		_, err := w.Write(v1)
		return err
	})
}

// ID3v1: "TAG", title, artist and album in 30 bytes each, the year in 4, a comment in 30 (28 and a
// zero and the track number in ID3v1.1) and a genre number, in Latin-1.

// id3v1Fields are an ID3v1 tag's fields. A field that is valid UTF-8 is read as that: ffmpeg writes
// UTF-8 there (cutting a long value by bytes, so its last character may be cut short), and Latin-1
// with accents is hardly ever valid UTF-8.
func id3v1Fields(b []byte) map[Field]string {
	str := func(x []byte) string {
		full := true
		if i := bytes.IndexByte(x, 0); i >= 0 {
			x, full = x[:i], false
		}
		if utf8.Valid(x) {
			return strings.TrimSpace(string(x))
		}
		if y := id3v1Cut(x); full && y != nil {
			return strings.TrimSpace(string(y))
		}
		return strings.TrimSpace(id3Latin1(x))
	}
	out := map[Field]string{}
	put := func(f Field, v string) {
		if v != "" {
			out[f] = v
		}
	}
	put(Title, str(b[3:33]))
	put(Artist, str(b[33:63]))
	put(Album, str(b[63:93]))
	put(Date, str(b[93:97]))
	if b[125] == 0 && b[126] != 0 {
		put(Comment, str(b[97:125]))
		put(Track, strconv.Itoa(int(b[126])))
	} else {
		put(Comment, str(b[97:127]))
	}
	put(Genre, genreName(int(b[127])))
	return out
}

// id3v1Cut is a field that fills its room without the UTF-8 character a writer cut short at its end,
// when what is left is UTF-8 with a character past ASCII in it; nil otherwise, so Latin-1 ending in
// an accent ("Caf\xe9") stays Latin-1.
func id3v1Cut(x []byte) []byte {
	for k := 1; k <= 3 && k <= len(x); k++ {
		c := x[len(x)-k]
		if c < 0x80 {
			return nil
		}
		if c < 0xC0 {
			continue // a continuation byte: its first byte is further back
		}
		need := 2
		switch {
		case c >= 0xF8:
			return nil
		case c >= 0xF0:
			need = 4
		case c >= 0xE0:
			need = 3
		}
		if k >= need {
			return nil // a whole character, so not one cut short
		}
		if y := x[:len(x)-k]; utf8.Valid(y) && utf8.RuneCount(y) < len(y) {
			return y
		}
		return nil
	}
	return nil
}

// id3v1Update is the ID3v1 tag old with set's fields written in: Latin-1, cut to fit, '?' for a
// character it can't hold; a genre it doesn't list is none (255).
func id3v1Update(old []byte, set map[Field]string) []byte {
	b := append([]byte(nil), old...)
	put := func(off, n int, s string) {
		clear(b[off : off+n])
		copy(b[off:off+n], id3Str(0, s, false))
	}
	for f, s := range set {
		switch f {
		case Title:
			put(3, 30, s)
		case Artist:
			put(33, 30, s)
		case Album:
			put(63, 30, s)
		case Date:
			if len(s) < 4 || !id3Digits(s[:4]) {
				s = ""
			}
			put(93, 4, s)
		case Genre:
			b[127] = 255
			for i, g := range genres {
				if strings.EqualFold(g, s) && i < 255 {
					b[127] = byte(i)
				}
			}
		}
	}
	track := 0
	if b[125] == 0 && b[126] != 0 {
		track = int(b[126])
	}
	s, newTrack := set[Track]
	if newTrack {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 256 {
			track = n
		} else {
			track = 0
		}
	}
	cm, newComment := set[Comment]
	switch {
	case newComment && track > 0:
		put(97, 28, cm)
		b[125], b[126] = 0, byte(track)
	case newComment:
		put(97, 30, cm)
	case newTrack && track > 0:
		b[125], b[126] = 0, byte(track)
	case newTrack && b[125] == 0:
		b[126] = 0
	}
	return b
}
