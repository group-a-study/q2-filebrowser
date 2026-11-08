package tags

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ID3v2: the tag at the start of an MP3, and in a chunk of its own in AIFF, WAV and DSF. Versions 2.2,
// 2.3 and 2.4 are read: unsynchronisation undone (the whole tag's in 2.2 and 2.3, each frame's in 2.4),
// compressed text frames inflated, data-length indicators and an extended header skipped, a footer
// allowed, and iTunes' plain (not syncsafe) frame sizes in 2.4 taken as they are. A tag is written in
// its own version (2.2 becomes 2.3), plain: no unsynchronisation, compression, encryption, extended
// header, footer or data-length indicators. A frame this doesn't change is written back as it was, in
// its place (so an early iTunSMPB or REPLAYGAIN_* stays early and intact); a new frame goes at the
// end, before the pictures there, and a new picture last. Comments and lyrics are the COMM and USLT
// frames with an empty description: those with one (iTunSMPB, iTunNORM) are left alone. The album
// artist is TPE2, else a TXXX ALBUMARTIST; a comment, else ffmpeg's TXXX "comment".
//
// Reading takes little memory whatever the tag holds: big frames stay in the file (an unsynchronised
// one too, undone as it is copied), at most id3MaxHeld of the rest is read, compressed frames other
// than text aren't inflated, and a tag of more than id3MaxFrames frames is refused.
//
// For a container's codec (MP3's here; AIFF, WAV and DSF build on it):
//
//	parseID3(b)    the tag b holds whole, from its 10-byte header on; t keeps slices of b
//	newID3()       an empty ID3v2.4 tag, for a file that has none
//	t.fields()     its fields, the track and disc split from their totals
//	t.apply(c)     makes c's changes: c.Set, c.Picture (the only picture after), c.RemovePicture
//	t.picture()    the front cover, else the first picture, nil without; as Data for a parseID3 tag
//	t.format()     its version as read: "ID3v2.3"
//	t.notes()      what the editor should know: a 2.2 tag saved as 2.3, frames saving leaves out
//	t.encode(min)  the tag as it is now, max(its length, min) bytes, zeros padding it; nil only past
//	               256 MB
//	t.needed()     encode's length without padding, to tell whether the tag fits the room it has
//	t.fits(room)   whether t can be written over the room bytes its old tag took, in place, a piece at
//	               a time: frames left in the file may move towards the end by id3MaxLag in all; past
//	               that it puts changed frames after those kept, if that does
//	t.patchTo(path, off, room)  writes it so, once fits said it can, at off where it was read: frames
//	               left in the file are copied, not held, but for the bytes the writing overtakes
//	id3Len(h)      the length of the tag whose 10-byte header h is, footer included; 0 if h isn't one
//
// loadID3(r, off, n) reads a tag in a file instead, leaving big frames (pictures) there: picture()
// then gives the cover's place in r, and encode and writeTo read those frames back from t.src.r,
// which must still be open (point it at the copy being read when writing the file again).

const (
	id3Pre       = 64 << 10  // the first bytes of a tag read at once; frames past them are read as needed
	id3PicHead   = 4 << 10   // a picture frame's first bytes: its MIME type, picture type and description
	id3MaxText   = 1 << 20   // a text frame longer than this is left in the file, unread
	id3MaxHeld   = 4 << 20   // the most of a tag's frames read past its first bytes; later ones stay in the file
	id3MaxFrames = 8192      // a tag with more frames is refused: real ones have tens, a podcast's chapters hundreds
	id3ZeroCheck = 4 << 10   // the bytes after the frames looked at to tell padding from frames misread
	id3ZeroScan  = 1 << 20   // the most looked at there when another reading of the frames has a frame there
	id3MaxLag    = 64 << 10  // the most an in-place save holds of frames left in the file that move towards the end
	id3MaxSize   = 1<<28 - 1 // the most a syncsafe size holds
)

type id3Frame struct {
	id     string // "TIT2"; v2.2's ids are read as v2.3's
	status byte   // its status flags, in its version's layout
	data   []byte // its body, plain
	kept   bool   // read from the tag, not made by a change
	// A lazy frame's body is left in the file: pre, then the n bytes at off from the tag's start, or,
	// when unsync, the n that the raw bytes at off give once undone, after the first skip of them.
	lazy      bool
	off, n    int64
	unsync    bool
	raw, skip int64
	pre       []byte // held before those: a v2.2 picture's header made an APIC's
	head      []byte // a lazy picture's first bytes, pre included
}

func (f *id3Frame) size() int64 {
	if f.lazy {
		return int64(len(f.pre)) + f.n
	}
	return int64(len(f.data))
}

// span is how many bytes from off copying a lazy frame reads from the file: n, or raw when unsync
// (the skip undone and passed over first).
func (f *id3Frame) span() int64 {
	if f.unsync {
		return f.raw
	}
	return f.n
}

// readable is whether the fields read f: a frame held, and not so long that decoding it costs.
func (f *id3Frame) readable() bool { return !f.lazy && len(f.data) <= id3MaxText }

// id3Src is where a tag's bytes are: pre holds its first bytes (all of them for a tag read whole), r
// the rest, the tag starting at base.
type id3Src struct {
	r    io.ReaderAt
	base int64
	pre  []byte
}

// get is n bytes at off from the tag's start.
func (s *id3Src) get(off, n int64) ([]byte, error) {
	if off >= 0 && n >= 0 && off+n <= int64(len(s.pre)) {
		return s.pre[off : off+n : off+n], nil
	}
	if s.r == nil {
		return nil, ErrCorrupt
	}
	return readAt(s.r, s.base+off, n)
}

// zeros is whether the bytes from off to end are zeros. It looks at id3ZeroCheck of them, or up to
// to when that is further (where another reading of the frames has them end), though no more than
// id3ZeroScan; it stops at the first that isn't zero.
func (s *id3Src) zeros(off, end, to int64) bool {
	end = min(end, max(off+id3ZeroCheck, min(to, off+id3ZeroScan)))
	var buf []byte
	for off < end {
		var b []byte
		if off < int64(len(s.pre)) {
			b = s.pre[off:min(end, int64(len(s.pre)))]
		} else {
			if s.r == nil {
				return false
			}
			if buf == nil {
				buf = make([]byte, min(end-off, 32<<10))
			}
			b = buf[:min(end-off, int64(len(buf)))]
			if k, _ := s.r.ReadAt(b, s.base+off); k < len(b) {
				return false
			}
		}
		for _, c := range b {
			if c != 0 {
				return false
			}
		}
		off += int64(len(b))
	}
	return true
}

// id3Stream reads a tag's bytes in order from raw up to end (both from the tag's start), undoing
// unsynchronisation when unsync: a zero after 0xFF is dropped.
type id3Stream struct {
	s        *id3Src
	raw, end int64
	unsync   bool
	ff       bool   // the last byte read was 0xFF
	buf      []byte // raw bytes from at
	at       int64
	own      []byte // room for buf when it isn't in s.pre
}

// left is the raw bytes from raw on that buf holds, filling it when it holds none; io.EOF at end.
func (d *id3Stream) left() ([]byte, error) {
	if i := d.raw - d.at; i >= 0 && i < int64(len(d.buf)) {
		return d.buf[i:], nil
	}
	if d.raw >= d.end {
		return nil, io.EOF
	}
	if d.raw < int64(len(d.s.pre)) {
		d.buf, d.at = d.s.pre[d.raw:min(int64(len(d.s.pre)), d.end)], d.raw
		return d.buf, nil
	}
	if d.s.r == nil {
		return nil, ErrCorrupt
	}
	if d.own == nil {
		d.own = make([]byte, 32<<10)
	}
	b := d.own[:min(d.end-d.raw, int64(len(d.own)))]
	if k, err := d.s.r.ReadAt(b, d.s.base+d.raw); k < len(b) {
		if err == nil || err == io.EOF {
			err = ErrCorrupt
		}
		return nil, err
	}
	d.buf, d.at = b, d.raw
	return b, nil
}

func (d *id3Stream) Read(p []byte) (int, error) {
	k := 0
	for k < len(p) {
		b, err := d.left()
		if err != nil {
			if k > 0 {
				return k, nil
			}
			return 0, err
		}
		if !d.unsync {
			m := copy(p[k:], b)
			k += m
			d.raw += int64(m)
			continue
		}
		j := 0
		for j < len(b) && k < len(p) {
			c := b[j]
			j++
			if d.ff && c == 0 {
				d.ff = false
				continue
			}
			d.ff = c == 0xFF
			p[k] = c
			k++
		}
		d.raw += int64(j)
	}
	return k, nil
}

// settle drops the zero that may follow a 0xFF the last read ended with, so raw is where the next
// byte starts.
func (d *id3Stream) settle() error {
	if !d.ff {
		return nil
	}
	d.ff = false
	b, err := d.left()
	switch {
	case err == io.EOF:
		return nil
	case err != nil:
		return err
	case b[0] == 0:
		d.raw++
	}
	return nil
}

// skip passes n bytes; io.ErrUnexpectedEOF when there are fewer.
func (d *id3Stream) skip(n int64) error {
	if !d.unsync {
		if n > d.end-d.raw {
			return io.ErrUnexpectedEOF
		}
		d.raw += n
		return nil
	}
	_, err := io.CopyN(io.Discard, d, n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// bytes reads the next n bytes; io.ErrUnexpectedEOF when there are fewer.
func (d *id3Stream) bytes(n int64) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(d, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

type id3Tag struct {
	major   byte // 2, 3 or 4, as read
	frames  []*id3Frame
	src     *id3Src // where lazy frames are; nil for a new tag
	note    []string
	used    int64  // past this the old tag held zeros only (its padding), as read
	held    int64  // the bytes of frames read past src.pre
	scratch []byte // room for one id3Stream's reading at a time
}

func newID3() *id3Tag { return &id3Tag{major: 4} }

// out is the version the tag is written as.
func (t *id3Tag) out() byte {
	if t.major == 4 {
		return 4
	}
	return 3
}

func (t *id3Tag) format() string { return fmt.Sprintf("ID3v2.%d", t.major) }

func (t *id3Tag) notes() []string { return t.note }

// stream reads the n raw bytes at off in s, undone when unsync.
func (t *id3Tag) stream(s *id3Src, off, n int64, unsync bool) *id3Stream {
	if t.scratch == nil {
		t.scratch = make([]byte, 32<<10)
	}
	return &id3Stream{s: s, raw: off, end: off + n, unsync: unsync, own: t.scratch}
}

func id3Syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 | int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

func id3PutSyncsafe(b []byte, n int64) {
	b[0], b[1], b[2], b[3] = byte(n>>21)&0x7f, byte(n>>14)&0x7f, byte(n>>7)&0x7f, byte(n)&0x7f
}

// id3Len is the length of the ID3v2 tag whose header h is, from that header to the end of its footer;
// 0 when h isn't the header of one.
func id3Len(h []byte) int64 {
	if len(h) < 10 || string(h[:3]) != "ID3" || h[3] == 0xFF || h[4] == 0xFF || (h[6]|h[7]|h[8]|h[9])&0x80 != 0 {
		return 0
	}
	n := 10 + id3Syncsafe(h[6:10])
	if h[3] >= 4 && h[5]&0x10 != 0 {
		n += 10
	}
	return n
}

// parseID3 reads the tag b holds whole, from its header on.
func parseID3(b []byte) (*id3Tag, error) {
	n := id3Len(b)
	if n == 0 || n > int64(len(b)) {
		return nil, ErrCorrupt
	}
	return readID3(&id3Src{pre: b[:n:n]}, n)
}

// loadID3 reads the tag of n bytes at off in r. Big frames it needn't read (pictures, mostly) are
// left there, so reading a song's tags doesn't read its cover.
func loadID3(r io.ReaderAt, off, n int64) (*id3Tag, error) {
	pre, err := readAt(r, off, min(n, id3Pre))
	if err != nil {
		return nil, err
	}
	return readID3(&id3Src{r: r, base: off, pre: pre}, n)
}

func readID3(s *id3Src, n int64) (*id3Tag, error) {
	h, err := s.get(0, 10)
	if err != nil {
		return nil, err
	}
	t := &id3Tag{major: h[3], src: s, used: n}
	flags := h[5]
	end := 10 + id3Syncsafe(h[6:10]) // the frames and padding end here, before a footer
	if t.major < 2 || t.major > 4 || end > n {
		return nil, ErrCorrupt
	}
	if t.major == 2 && flags&0x40 != 0 {
		return nil, ErrCorrupt // v2.2's compression was never defined
	}
	var heads []id3Head
	var stop int64
	var clean bool
	if t.major < 4 && flags&0x80 != 0 {
		heads, stop, clean, err = t.headsUnsync(s, end, flags&0x40 != 0)
	} else {
		heads, stop, clean, err = t.headsPlain(s, end, flags)
	}
	if err != nil {
		return nil, err
	}
	if clean && end == n {
		t.used = stop // the rest is padding
	}
	if !clean {
		t.note = append(t.note, "Part of its ID3 tag isn't laid out as frames, so saving leaves that part out")
	}
	left := map[string]bool{} // v2.2's ids that 2.3 has no place for
	var no22 []string
	unread := 0
	for _, h := range heads {
		if h.n == 0 {
			continue // an empty frame isn't one
		}
		if _, ok := id3From22[h.id]; t.major == 2 && !ok {
			if !left[h.id] {
				left[h.id] = true
				no22 = append(no22, h.id)
			}
			continue
		}
		f, err := t.frame(s, h)
		switch {
		case err != nil:
			return nil, err
		case f != nil:
			t.frames = append(t.frames, f)
		default:
			unread++
		}
	}
	if t.major == 2 {
		t.note = append(t.note, "Saving turns its ID3v2.2 tag into ID3v2.3")
		if len(no22) > 0 {
			list := strings.Join(no22[:min(len(no22), 5)], ", ")
			if len(no22) > 5 {
				list += fmt.Sprintf(" and %d more", len(no22)-5)
			}
			t.note = append(t.note, "Saving leaves out its "+list+" frames, which ID3v2.3 has no place for")
		}
	}
	if unread > 0 {
		t.note = append(t.note, fmt.Sprintf("Saving leaves out %d of its frames that are encrypted or can't be read", unread))
	}
	long, big := false, false
	for _, f := range t.frames {
		long = long || f.lazy && id3IsText(f.id)
		big = big || f.lazy && f.unsync && f.id == "APIC" && f.n > MaxPicture
	}
	if long {
		t.note = append(t.note, "Some of its text is too long to show here; saving keeps it")
	}
	if big {
		t.note = append(t.note, "A picture in its tag is stored in a form too big to show here; saving keeps it")
	}
	return t, nil
}

type id3Head struct {
	id     string
	st, fm byte  // status and format flags
	off, n int64 // the body's place and length in the file
	dn     int64 // its length once unsynchronisation is undone; -1 until counted
	unsync bool  // it is unsynchronised in the file
}

// headsPlain finds the frames of a tag whose frame headers are as they are in the file: past an
// extended header, and in 2.4 with sizes syncsafe or, as iTunes wrote them, plain.
func (t *id3Tag) headsPlain(s *id3Src, end int64, flags byte) ([]id3Head, int64, bool, error) {
	start := int64(10)
	if t.major >= 3 && flags&0x40 != 0 {
		e, err := s.get(10, 4)
		if err != nil {
			return nil, 0, false, err
		}
		if t.major == 3 {
			start += 4 + int64(binary.BigEndian.Uint32(e))
		} else {
			start += id3Syncsafe(e)
		}
		if start < 14 || start > end {
			return nil, 0, false, ErrCorrupt
		}
	}
	unsync := t.major == 4 && flags&0x80 != 0
	heads, stop, clean, odd, err := t.heads(s, start, end, true, unsync)
	if err != nil {
		return nil, 0, false, err
	}
	if t.major == 4 && (!clean || odd) {
		// iTunes wrote 2.4 frame sizes as plain numbers: take whichever reading of them lays the tag
		// out as frames then zeros, else finds more frames. A reading's zeros are looked at as far as
		// the other's frames go, when those end cleanly: a run of zeros inside a frame isn't padding.
		h2, stop2, clean2, _, err := t.heads(s, start, end, false, unsync)
		if err == nil {
			reach := func(ok bool, at int64) int64 {
				if ok {
					return at
				}
				return 0
			}
			good := clean && s.zeros(stop, end, reach(clean2, stop2))
			good2 := good
			if !clean || !clean2 || stop2 != stop {
				good2 = clean2 && s.zeros(stop2, end, reach(clean, stop))
			}
			if good2 && !good || good2 == good && len(h2) > len(heads) {
				heads, stop, clean = h2, stop2, clean2
			}
		}
	}
	return heads, stop, clean, nil
}

// heads walks the frame headers from off to end, sizes read as syncsafe numbers in 2.4 when safe. It
// gives where they stopped, whether that was at padding or the end (clean), and whether a size
// would read differently as a plain number (odd). A 2.4 frame is unsynchronised when it says so, or
// every one is when unsync.
func (t *id3Tag) heads(s *id3Src, off, end int64, safe, unsync bool) (out []id3Head, stop int64, clean, odd bool, err error) {
	hl := int64(10)
	if t.major == 2 {
		hl = 6
	}
	for off+hl <= end {
		h, err := s.get(off, hl)
		if err != nil {
			return nil, 0, false, false, err
		}
		if h[0] == 0 {
			return out, off, true, odd, nil // padding
		}
		x := id3Head{off: off + hl}
		if t.major == 2 {
			x.id, x.n = string(h[:3]), int64(h[3])<<16|int64(h[4])<<8|int64(h[5])
		} else {
			x.id, x.st, x.fm = string(h[:4]), h[8], h[9]
			x.n = int64(binary.BigEndian.Uint32(h[4:8]))
			if t.major == 4 && safe {
				if (h[4]|h[5]|h[6]|h[7])&0x80 != 0 {
					return out, off, false, true, nil
				}
				if n := id3Syncsafe(h[4:8]); n != x.n {
					x.n, odd = n, true
				}
			}
		}
		if !id3ValidID(x.id) || x.n > end-x.off {
			return out, off, false, odd, nil
		}
		if len(out) == id3MaxFrames {
			return nil, 0, false, false, ErrCorrupt
		}
		x.dn, x.unsync = x.n, t.major == 4 && (unsync || x.fm&0x02 != 0)
		if x.unsync {
			x.dn = -1
		}
		out = append(out, x)
		off = x.off + x.n
	}
	return out, off, true, odd, nil
}

// headsUnsync finds the frames of a v2.2 or v2.3 tag unsynchronised whole, frame headers and all,
// undoing that as it reads: a frame's off and n are its body's place in the file, dn its length.
func (t *id3Tag) headsUnsync(s *id3Src, end int64, ext bool) ([]id3Head, int64, bool, error) {
	d := t.stream(s, 10, end-10, true)
	if ext && t.major == 3 {
		e, err := d.bytes(4)
		if err == nil {
			err = d.skip(int64(binary.BigEndian.Uint32(e)))
		}
		if err != nil {
			return nil, 0, false, ErrCorrupt
		}
	}
	hl := int64(10)
	if t.major == 2 {
		hl = 6
	}
	h := make([]byte, hl)
	var out []id3Head
	for {
		if err := d.settle(); err != nil {
			return nil, 0, false, err
		}
		at := d.raw
		b, err := d.left()
		if err == io.EOF || err == nil && b[0] == 0 {
			return out, at, true, nil // the end, or padding
		}
		if err != nil {
			return nil, 0, false, err
		}
		if _, err := io.ReadFull(d, h); err == io.EOF || err == io.ErrUnexpectedEOF {
			return out, at, true, nil // fewer bytes left than a header's
		} else if err != nil {
			return nil, 0, false, err
		}
		x := id3Head{unsync: true}
		if t.major == 2 {
			x.id, x.dn = string(h[:3]), int64(h[3])<<16|int64(h[4])<<8|int64(h[5])
		} else {
			x.id, x.st, x.fm, x.dn = string(h[:4]), h[8], h[9], int64(binary.BigEndian.Uint32(h[4:8]))
		}
		if !id3ValidID(x.id) || x.dn > end-d.raw {
			return out, at, false, nil
		}
		if len(out) == id3MaxFrames {
			return nil, 0, false, ErrCorrupt
		}
		if err := d.settle(); err != nil {
			return nil, 0, false, err
		}
		x.off = d.raw
		if err := d.skip(x.dn); err == io.ErrUnexpectedEOF {
			return out, at, false, nil
		} else if err != nil {
			return nil, 0, false, err
		}
		x.n = d.raw - x.off
		out = append(out, x)
	}
}

func id3ValidID(id string) bool {
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return id != ""
}

// id3IsText is whether id is a frame of text the fields read: T*, COMM, USLT.
func id3IsText(id string) bool { return id[0] == 'T' || id == "COMM" || id == "USLT" }

// frame reads the frame h heads: nil (and no error) for one saving leaves out. A frame is read when
// it is short, or text that isn't too long, and the tag's id3MaxHeld allows; else it stays in the file.
func (t *id3Tag) frame(s *id3Src, h id3Head) (*id3Frame, error) {
	f := &id3Frame{id: h.id, kept: true}
	// What comes before the body proper, as flagged: in 2.3 the inflated size, the encryption method
	// and the group; in 2.4 the group, the encryption method and the data length.
	var skip int64
	zipped := false
	switch t.major {
	case 2:
		f.id = id3From22[h.id]
	case 3:
		f.status = h.st & 0xE0
		if h.fm&0x40 != 0 {
			return nil, nil // encrypted
		}
		if zipped = h.fm&0x80 != 0; zipped {
			skip += 4
		}
		if h.fm&0x20 != 0 {
			skip++
		}
	case 4:
		f.status = h.st & 0x70
		if h.fm&0x04 != 0 {
			return nil, nil
		}
		zipped = h.fm&0x08 != 0
		if h.fm&0x40 != 0 {
			skip++
		}
		if h.fm&0x01 != 0 {
			skip += 4
		}
	}
	text := id3IsText(f.id)
	if zipped {
		return t.inflated(s, h, f, skip, text)
	}
	inPre := !h.unsync && h.off+h.n <= int64(len(s.pre))
	short := h.n-skip <= id3PicHead || text && h.n-skip <= id3MaxText
	if inPre || s.r == nil || short && t.held+h.n <= id3MaxHeld {
		b, err := t.body(s, h, !inPre)
		if err != nil {
			return nil, err
		}
		if int64(len(b)) <= skip {
			return nil, nil
		}
		if b = b[skip:]; t.major == 2 && f.id == "APIC" {
			if b = id3PicToAPIC(b); b == nil {
				return nil, nil
			}
		}
		f.data = b
		return f, nil
	}
	dn := h.dn
	if dn < 0 {
		// A 2.4 frame unsynchronised: its length undone is counted, not held.
		k, err := io.Copy(io.Discard, t.stream(s, h.off, h.n, true))
		if err != nil {
			return nil, err
		}
		dn = k
	}
	if dn <= skip {
		return nil, nil
	}
	f.lazy, f.unsync = true, h.unsync
	if h.unsync {
		f.off, f.raw, f.skip, f.n = h.off, h.n, skip, dn-skip
	} else {
		f.off, f.n = h.off+skip, h.n-skip
	}
	if f.id != "APIC" || t.held+id3PicHead > id3MaxHeld {
		if t.major == 2 && f.id == "APIC" {
			return nil, nil
		}
		return f, nil // kept, but past what is read (a picture without its head isn't shown)
	}
	head, err := t.peek(s, f, id3PicHead)
	if err != nil {
		return nil, err
	}
	if t.major != 2 {
		f.head = head
		return f, nil
	}
	// A v2.2 picture: its header (encoding, image format, type, description) made an APIC's is held,
	// the image left where it is.
	if len(head) < 5 {
		return nil, nil
	}
	be := false
	_, rest, ok := id3Cut(head[0], head[5:], &be)
	k := int64(len(head) - len(rest))
	if !ok || k >= f.n {
		return nil, nil
	}
	f.pre = id3PicToAPIC(head[:k])
	if f.unsync {
		f.skip += k
	} else {
		f.off += k
	}
	f.n -= k
	f.head = f.pre
	return f, nil
}

// body is the frame h heads's body, unsynchronisation undone; counted in t.held when held.
func (t *id3Tag) body(s *id3Src, h id3Head, held bool) ([]byte, error) {
	var b []byte
	var err error
	switch {
	case !h.unsync:
		b, err = s.get(h.off, h.n)
	case h.dn >= 0:
		b, err = t.stream(s, h.off, h.n, true).bytes(h.dn)
	default:
		b = make([]byte, h.n)
		var k int
		k, err = io.ReadFull(t.stream(s, h.off, h.n, true), b)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			err = nil
		}
		b = b[:k]
	}
	if held {
		t.held += int64(len(b))
	}
	return b, err
}

// peek is the first n bytes of what a lazy frame copies from the file (fewer when there are fewer).
func (t *id3Tag) peek(s *id3Src, f *id3Frame, n int64) ([]byte, error) {
	n = min(n, f.n)
	if !f.unsync {
		if f.off+n > int64(len(s.pre)) {
			t.held += n
		}
		return s.get(f.off, n)
	}
	t.held += n
	d := t.stream(s, f.off, f.raw, true)
	if err := d.skip(f.skip); err != nil {
		return nil, err
	}
	return d.bytes(n)
}

// inflated reads a compressed frame: text up to id3MaxText long, and only while the tag's id3MaxHeld
// allows. Anything else is left out: a frame compressed so isn't held, nor can it be written plain
// without inflating it whole.
func (t *id3Tag) inflated(s *id3Src, h id3Head, f *id3Frame, skip int64, text bool) (*id3Frame, error) {
	most := min(id3MaxText, id3MaxHeld-t.held)
	if !text || most <= 0 || h.n > id3MaxText {
		return nil, nil
	}
	b, err := t.body(s, h, false)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) <= skip {
		return nil, nil
	}
	size := int64(-1) // how long it says it is, inflated
	if t.major == 3 {
		size = int64(binary.BigEndian.Uint32(b))
	} else if h.fm&0x01 != 0 {
		size = id3Syncsafe(b[skip-4:])
	}
	if size > most {
		return nil, nil
	}
	if b, err = id3Inflate(b[skip:], most, size); err != nil {
		return nil, nil
	}
	t.held += int64(len(b))
	f.data = b
	return f, nil
}

// id3Inflate is a compressed frame's body; an error when it is empty or longer than most. size is how
// long the frame says it is, -1 when it doesn't.
func id3Inflate(b []byte, most, size int64) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if size > 0 {
		out.Grow(int(size) + bytes.MinRead)
	}
	_, err = out.ReadFrom(io.LimitReader(zr, most+1))
	if err == nil && (int64(out.Len()) > most || out.Len() == 0) {
		err = ErrCorrupt
	}
	return out.Bytes(), err
}

// id3From22 are v2.2's frame ids that have a v2.3 form, the body as it was but for PIC's. iTunes'
// own (TCP, TST…) are among them.
var id3From22 = map[string]string{
	"BUF": "RBUF", "CNT": "PCNT", "COM": "COMM", "CRA": "AENC", "ETC": "ETCO", "EQU": "EQUA",
	"GEO": "GEOB", "IPL": "IPLS", "MCI": "MCDI", "MLL": "MLLT", "PIC": "APIC", "POP": "POPM",
	"REV": "RVRB", "RVA": "RVAD", "SLT": "SYLT", "STC": "SYTC", "TAL": "TALB", "TBP": "TBPM",
	"TCM": "TCOM", "TCO": "TCON", "TCR": "TCOP", "TDA": "TDAT", "TDY": "TDLY", "TEN": "TENC",
	"TFT": "TFLT", "TIM": "TIME", "TKE": "TKEY", "TLA": "TLAN", "TLE": "TLEN", "TMT": "TMED",
	"TOA": "TOPE", "TOF": "TOFN", "TOL": "TOLY", "TOR": "TORY", "TOT": "TOAL", "TP1": "TPE1",
	"TP2": "TPE2", "TP3": "TPE3", "TP4": "TPE4", "TPA": "TPOS", "TPB": "TPUB", "TRC": "TSRC",
	"TRD": "TRDA", "TRK": "TRCK", "TSI": "TSIZ", "TSS": "TSSE", "TT1": "TIT1", "TT2": "TIT2",
	"TT3": "TIT3", "TXT": "TEXT", "TXX": "TXXX", "TYE": "TYER", "UFI": "UFID", "ULT": "USLT",
	"WAF": "WOAF", "WAR": "WOAR", "WAS": "WOAS", "WCM": "WCOM", "WCP": "WCOP", "WPB": "WPUB",
	"WXX": "WXXX", "TCP": "TCMP", "TST": "TSOT", "TSP": "TSOP", "TSA": "TSOA", "TS2": "TSO2",
	"TSC": "TSOC",
}

// id3PicToAPIC is a v2.2 PIC's body as an APIC's: its 3-letter image format becomes a MIME type.
func id3PicToAPIC(b []byte) []byte {
	if len(b) < 5 {
		return nil
	}
	f := strings.ToLower(strings.TrimRight(string(b[1:4]), "\x00 "))
	mime := map[string]string{"jpg": "image/jpeg", "png": "image/png"}[f]
	if mime == "" {
		mime = "image/" + f
	}
	out := make([]byte, 0, len(b)+len(mime))
	out = append(append(append(out, b[0]), mime...), 0)
	return append(out, b[4:]...)
}

// Text.

// id3Cut reads one string in encoding enc from the start of b: the string, what follows its
// terminator, and whether there was one. be carries a UTF-16 byte order from string to string.
func id3Cut(enc byte, b []byte, be *bool) (s string, rest []byte, ok bool) {
	if enc == 1 || enc == 2 {
		i := 0
		for i+1 < len(b) && (b[i] != 0 || b[i+1] != 0) {
			i += 2
		}
		if i+1 >= len(b) {
			return id3UTF16(enc, b, be), nil, false
		}
		return id3UTF16(enc, b[:i], be), b[i+2:], true
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return id3Text8(enc, b), nil, false
	}
	return id3Text8(enc, b[:i]), b[i+1:], true
}

func id3Text8(enc byte, b []byte) string {
	if enc == 3 {
		return strings.TrimPrefix(string(b), "\xEF\xBB\xBF") // a BOM some writers put first
	}
	return id3Latin1(b)
}

func id3Latin1(b []byte) string {
	n := len(b)
	for _, c := range b {
		if c >= 0x80 {
			n++
		}
	}
	var sb strings.Builder
	sb.Grow(n)
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// id3UTF16 decodes b: by its BOM, else in the order the string before had, else as its zero bytes
// suggest (ASCII's high bytes are zero).
func id3UTF16(enc byte, b []byte, be *bool) string {
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		*be, b = false, b[2:]
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		*be, b = true, b[2:]
	case enc == 2:
		*be = true
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if *be {
			u[i] = binary.BigEndian.Uint16(b[2*i:])
		} else {
			u[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
	}
	return string(utf16.Decode(u))
}

// id3Guess is the byte order a UTF-16 text without a BOM most likely has.
func id3Guess(b []byte) bool {
	even, odd := 0, 0
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] == 0 {
			even++
		}
		if b[i+1] == 0 {
			odd++
		}
	}
	return even > odd
}

// id3Values are the strings in b, a frame's text in encoding enc after whatever came before it.
func id3Values(enc byte, b []byte, be *bool) []string {
	var out []string
	for len(b) > 0 {
		s, rest, _ := id3Cut(enc, b, be)
		out = append(out, s)
		b = rest
	}
	return out
}

// id3Join is the values that aren't blank, joined as the Q2 joins them.
func id3Join(vs []string) string {
	var out []string
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, " / ")
}

// id3Enc is the encoding a frame of version v takes for ss: UTF-8 in 2.4; in 2.3, Latin-1 when it
// can hold them, else UTF-16.
func id3Enc(v byte, ss ...string) byte {
	if v == 4 {
		return 3
	}
	for _, s := range ss {
		for _, r := range s {
			if r > 0xFF {
				return 1
			}
		}
	}
	return 0
}

// id3Str is s in encoding enc (UTF-16 little-endian with a BOM), terminated when term.
func id3Str(enc byte, s string, term bool) []byte {
	var b []byte
	switch enc {
	case 0:
		b = make([]byte, 0, len(s))
		for _, r := range s {
			if r > 0xFF {
				r = '?'
			}
			b = append(b, byte(r))
		}
	case 1, 2:
		if enc == 1 {
			b = []byte{0xFF, 0xFE}
		}
		for _, u := range utf16.Encode([]rune(s)) {
			if enc == 1 {
				b = append(b, byte(u), byte(u>>8))
			} else {
				b = append(b, byte(u>>8), byte(u))
			}
		}
	default:
		b = []byte(s)
	}
	if term {
		b = append(b, 0)
		if enc == 1 || enc == 2 {
			b = append(b, 0)
		}
	}
	return b
}

// Frame bodies.

// id3Text is a text frame's values.
func id3Text(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	be := id3Guess(b[1:])
	return id3Values(b[0], b[1:], &be)
}

// id3Desc is a TXXX frame's description, the rest of it and the byte order its text has.
func id3Desc(b []byte) (desc string, rest []byte, be bool) {
	if len(b) == 0 {
		return "", nil, false
	}
	be = id3Guess(b[1:])
	desc, rest, _ = id3Cut(b[0], b[1:], &be)
	return desc, rest, be
}

// id3TXXX is a TXXX frame's description and values.
func id3TXXX(b []byte) (string, []string) {
	d, rest, be := id3Desc(b)
	if len(b) == 0 {
		return d, nil
	}
	return d, id3Values(b[0], rest, &be)
}

// id3Lang is a COMM or USLT frame's language, description and text.
func id3Lang(b []byte) (lang []byte, desc, text string, ok bool) {
	if len(b) < 4 {
		return nil, "", "", false
	}
	be := id3Guess(b[4:])
	d, rest, _ := id3Cut(b[0], b[4:], &be)
	return b[1:4], d, id3Join(id3Values(b[0], rest, &be)), true
}

// id3APIC is an APIC frame's MIME type, picture type and description, and where its image starts.
func id3APIC(b []byte) (mime string, typ byte, desc string, at int, ok bool) {
	if len(b) < 2 {
		return
	}
	i := bytes.IndexByte(b[1:], 0)
	if i < 0 || 2+i >= len(b) {
		return
	}
	mime, typ = id3Latin1(b[1:1+i]), b[2+i]
	be := false
	d, rest, ok := id3Cut(b[0], b[3+i:], &be)
	return mime, typ, d, len(b) - len(rest), ok
}

func id3TextBody(v byte, s string) []byte {
	enc := id3Enc(v, s)
	return append([]byte{enc}, id3Str(enc, s, false)...)
}

func id3LangBody(v byte, lang []byte, s string) []byte {
	enc := id3Enc(v, s)
	b := append([]byte{enc}, lang...)
	b = append(b, id3Str(enc, "", true)...)
	return append(b, id3Str(enc, s, false)...)
}

func id3APICBody(v byte, p *Picture) []byte {
	enc := byte(0)
	if p.Desc != "" {
		enc = id3Enc(v, p.Desc)
	}
	b := make([]byte, 0, len(p.Data)+len(p.MIME)+len(p.Desc)*2+8)
	b = append(append(append(b, enc), p.MIME...), 0, p.Type)
	b = append(b, id3Str(enc, p.Desc, true)...)
	return append(b, p.Data...)
}

// Fields.

// values are the first id frame's values; none when that is too long to read.
func (t *id3Tag) values(id string) []string {
	for _, f := range t.frames {
		if f.id == id && !f.lazy {
			if !f.readable() {
				return nil
			}
			return id3Text(f.data)
		}
	}
	return nil
}

func (t *id3Tag) text(id string) string { return id3Join(t.values(id)) }

// txxx is the values of the TXXX frame described desc (in any case).
func (t *id3Tag) txxx(desc string) string {
	for _, f := range t.frames {
		if f.id != "TXXX" || !f.readable() || len(f.data) == 0 {
			continue
		}
		if d, rest, be := id3Desc(f.data); strings.EqualFold(d, desc) {
			return id3Join(id3Values(f.data[0], rest, &be))
		}
	}
	return ""
}

// id3PlainLang is a matcher for the id (COMM, USLT) frames with an empty description.
func id3PlainLang(id string) func(*id3Frame) bool {
	return func(f *id3Frame) bool {
		if f.id != id || !f.readable() {
			return false
		}
		_, d, _, ok := id3Lang(f.data)
		return ok && d == ""
	}
}

func (t *id3Tag) langText(id string) string {
	m := id3PlainLang(id)
	for _, f := range t.frames {
		if m(f) {
			_, _, s, _ := id3Lang(f.data)
			return s
		}
	}
	return ""
}

// date is TDRC in 2.4, else TYER with TDAT's day and month; each falls back on the other.
func (t *id3Tag) date() string {
	tdrc, tyer := t.text("TDRC"), t.text("TYER")
	if d := t.text("TDAT"); len(tyer) == 4 && len(d) == 4 && id3Digits(d) {
		tyer += "-" + d[2:4] + "-" + d[0:2] // TDAT is DDMM
	}
	if t.major == 4 && tdrc != "" || tyer == "" {
		return tdrc
	}
	return tyer
}

func id3Digits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// id3Genre is a TCON value as a name: "(17)" and "17" are ID3v1's Rock, "(4)Eurodisco" is Disco and
// Eurodisco, "(RX)" a remix, "((name" a name starting with "(".
func id3Genre(v string) []string {
	var out []string
	for strings.HasPrefix(v, "(") && !strings.HasPrefix(v, "((") {
		i := strings.IndexByte(v, ')')
		if i < 0 {
			break
		}
		ref := v[1:i]
		v = v[i+1:]
		switch ref {
		case "RX":
			out = append(out, "Remix")
		case "CR":
			out = append(out, "Cover")
		default:
			if n, err := strconv.Atoi(ref); err == nil && genreName(n) != "" {
				out = append(out, genreName(n))
			}
		}
	}
	v = strings.TrimPrefix(v, "(")
	if id3Digits(v) {
		n, _ := strconv.Atoi(v)
		v = genreName(n)
	}
	if v != "" && (len(out) == 0 || out[len(out)-1] != v) {
		out = append(out, v)
	}
	return out
}

func (t *id3Tag) genre() string {
	var out []string
	for _, v := range t.values("TCON") {
		out = append(out, id3Genre(strings.TrimSpace(v))...)
	}
	return id3Join(out)
}

// id3Also are the TXXX frames a field falls back on when its own frame is missing: ffmpeg writes a
// comment as TXXX "comment". Setting the field removes them.
var id3Also = map[Field][]string{AlbumArtist: {"ALBUMARTIST", "ALBUM ARTIST"}, Comment: {"COMMENT"}}

// fields are the tag's fields.
func (t *id3Tag) fields() map[Field]string {
	out := map[Field]string{}
	put := func(f Field, v string) {
		for _, d := range id3Also[f] {
			if v == "" {
				v = t.txxx(d)
			}
		}
		if v != "" {
			out[f] = v
		}
	}
	put(Title, t.text("TIT2"))
	put(Artist, t.text("TPE1"))
	put(Album, t.text("TALB"))
	put(AlbumArtist, t.text("TPE2"))
	for _, p := range id3Pairs {
		if vs := t.values(p.id); len(vs) > 0 {
			n, of := splitNum(vs[0])
			put(p.n, n)
			put(p.of, of)
		}
	}
	put(Date, t.date())
	put(Genre, t.genre())
	put(Composer, t.text("TCOM"))
	put(Comment, t.langText("COMM"))
	put(Lyrics, t.langText("USLT"))
	return out
}

// id3Pairs are the frames holding a number and its total: "3/12".
var id3Pairs = []struct {
	id    string
	n, of Field
}{{"TRCK", Track, TrackTotal}, {"TPOS", Disc, DiscTotal}}

// Changes.

func id3ByID(ids ...string) func(*id3Frame) bool {
	return func(f *id3Frame) bool {
		for _, id := range ids {
			if f.id == id {
				return true
			}
		}
		return false
	}
}

// set puts f where the first frame match takes was, removing the others it takes; with none, f goes
// with the new frames: at the end, before the pictures there. A nil f removes them all.
func (t *id3Tag) set(match func(*id3Frame) bool, f *id3Frame) {
	at := -1
	out := make([]*id3Frame, 0, len(t.frames)+1)
	for _, g := range t.frames {
		if match(g) {
			if at < 0 {
				at = len(out)
			}
			continue
		}
		out = append(out, g)
	}
	if f != nil {
		if at < 0 {
			for at = len(out); at > 0 && out[at-1].id == "APIC"; at-- {
			}
		}
		out = append(out, nil)
		copy(out[at+1:], out[at:])
		out[at] = f
	}
	t.frames = out
}

// setText makes s the text of the id frame (the first of match's), removing it when s is "".
func (t *id3Tag) setText(match func(*id3Frame) bool, id, s string) {
	var f *id3Frame
	if s != "" {
		f = &id3Frame{id: id, data: id3TextBody(t.out(), s)}
	}
	t.set(match, f)
}

// setLang makes s the text of the id (COMM, USLT) frame with an empty description, in that frame's
// language ("eng" for a new one); the others with an empty description go.
func (t *id3Tag) setLang(id, s string) {
	m, lang := id3PlainLang(id), []byte("eng")
	for _, f := range t.frames {
		if m(f) {
			lang, _, _, _ = id3Lang(f.data)
			break
		}
	}
	var f *id3Frame
	if s != "" {
		f = &id3Frame{id: id, data: id3LangBody(t.out(), lang, s)}
	}
	t.set(m, f)
}

// setDate writes TDRC in 2.4; in 2.3 TYER, which holds a year's 4 digits and nothing else, so a date
// that doesn't start with them has no year to write there. The other version's frames, and TDAT,
// TIME and TRDA, go.
func (t *id3Tag) setDate(s string) {
	if s == "" || t.out() == 4 {
		t.setText(id3ByID("TDRC", "TYER"), "TDRC", s)
		t.set(id3ByID("TYER", "TDAT", "TIME", "TRDA"), nil)
		return
	}
	year := ""
	if len(s) >= 4 && id3Digits(s[:4]) {
		year = s[:4]
	}
	t.setText(id3ByID("TYER", "TDRC"), "TYER", year)
	t.set(id3ByID("TDRC", "TDAT", "TIME", "TRDA"), nil)
}

// apply makes c's changes. A number and its total are one frame ("3/12", or "/12" for a total
// without its number): setting either keeps the other.
func (t *id3Tag) apply(c Changes) {
	cur := t.fields()
	text := func(id, s string) { t.setText(id3ByID(id), id, s) }
	for _, f := range Fields {
		s, ok := c.Set[f]
		if !ok {
			continue
		}
		switch f {
		case Title:
			text("TIT2", s)
		case Artist:
			text("TPE1", s)
		case Album:
			text("TALB", s)
		case AlbumArtist:
			text("TPE2", s)
		case Date:
			t.setDate(s)
		case Genre:
			text("TCON", s)
		case Composer:
			text("TCOM", s)
		case Comment:
			t.setLang("COMM", s)
		case Lyrics:
			t.setLang("USLT", s)
		}
		if also := id3Also[f]; also != nil {
			t.set(func(g *id3Frame) bool {
				if g.id != "TXXX" || g.lazy {
					return false
				}
				d, _, _ := id3Desc(g.data)
				for _, a := range also {
					if strings.EqualFold(d, a) {
						return true
					}
				}
				return false
			}, nil)
		}
		for _, p := range id3Pairs {
			if _, both := c.Set[p.n]; f != p.n && (f != p.of || both) {
				continue
			}
			n, ok := c.Set[p.n]
			if !ok {
				n = cur[p.n]
			}
			of, ok := c.Set[p.of]
			if !ok {
				of = cur[p.of]
			}
			if of != "" {
				n += "/" + of
			}
			text(p.id, n)
		}
	}
	if c.Picture != nil || c.RemovePicture {
		t.set(id3ByID("APIC"), nil)
	}
	if c.Picture != nil {
		t.frames = append(t.frames, &id3Frame{id: "APIC", data: id3APICBody(t.out(), c.Picture)})
	}
}

// picture is the front cover, else the first picture; nil without. A picture the file holds
// unsynchronised is undone in memory, so one past MaxPicture isn't given.
func (t *id3Tag) picture() *picRef {
	var pick *id3Frame
	var mime, desc string
	var typ byte
	var at int64
	for _, f := range t.frames {
		if f.id != "APIC" {
			continue
		}
		b := f.data
		if f.lazy {
			b = f.head
		}
		m, ty, d, a, ok := id3APIC(b)
		k := int64(a - len(f.pre)) // where the image starts in what's left in the file
		switch {
		case !ok || m == "-->":
			continue
		case !f.lazy && a >= len(b):
			continue
		case f.lazy && (k < 0 || k >= f.n || f.unsync && f.n > MaxPicture):
			continue
		}
		if pick == nil || ty == 3 {
			pick, mime, typ, desc, at = f, m, ty, d, int64(a)
		}
		if ty == 3 {
			break
		}
	}
	switch {
	case pick == nil:
		return nil
	case !pick.lazy:
		return &picRef{MIME: mime, Type: typ, Desc: desc, Data: pick.data[at:]}
	case !pick.unsync:
		k := at - int64(len(pick.pre))
		return atFile(mime, typ, desc, t.src.base+pick.off+k, pick.n-k)
	}
	d := t.stream(t.src, pick.off, pick.raw, true)
	if d.skip(pick.skip) != nil {
		return nil
	}
	b, err := d.bytes(pick.n)
	if err != nil {
		return nil
	}
	return &picRef{MIME: mime, Type: typ, Desc: desc, Data: b[at-int64(len(pick.pre)):]}
}

// Writing.

// needed is the tag's length without padding.
func (t *id3Tag) needed() int64 {
	n := int64(10)
	for _, f := range t.frames {
		n += 10 + f.size()
	}
	return n
}

func (t *id3Tag) encode(minSize int) []byte {
	var b bytes.Buffer
	if n := max(t.needed(), int64(minSize)); n <= id3MaxSize {
		b.Grow(int(n))
	}
	if t.writeTo(&b, int64(minSize)) != nil {
		return nil
	}
	return b.Bytes()
}

// writeTo writes the tag, padded with zeros to minSize bytes when shorter. Lazy frames are copied
// from the tag's source.
func (t *id3Tag) writeTo(w io.Writer, minSize int64) error {
	total := max(t.needed(), minSize)
	return t.write(w, total, total)
}

// write writes the tag as total bytes long, its frames then zeros, stopping after upto bytes (no
// fewer than its frames take).
func (t *id3Tag) write(w io.Writer, total, upto int64) error {
	v, n := t.out(), t.needed()
	if total-10 > id3MaxSize {
		return ErrTooBig
	}
	if upto < n || upto > total {
		return ErrCorrupt
	}
	h := []byte{'I', 'D', '3', v, 0, 0, 0, 0, 0, 0}
	id3PutSyncsafe(h[6:], total-10)
	if _, err := w.Write(h); err != nil {
		return err
	}
	for _, f := range t.frames {
		fh := make([]byte, 10)
		copy(fh, f.id)
		if v == 4 {
			id3PutSyncsafe(fh[4:], f.size())
		} else {
			binary.BigEndian.PutUint32(fh[4:], uint32(f.size()))
		}
		fh[8] = f.status
		if _, err := w.Write(fh); err != nil {
			return err
		}
		var err error
		if f.lazy {
			err = t.copyOut(w, f)
		} else {
			_, err = w.Write(f.data)
		}
		if err != nil {
			return err
		}
	}
	zero := make([]byte, min(upto-n, 32<<10))
	for pad := upto - n; pad > 0; {
		k := min(pad, int64(len(zero)))
		if _, err := w.Write(zero[:k]); err != nil {
			return err
		}
		pad -= k
	}
	return nil
}

// copyOut writes a lazy frame's body: what is held of it, then the rest from the file, undone a piece
// at a time when it is unsynchronised there. Written over itself (patchTo), the frame is read through
// what the writing kept of it.
func (t *id3Tag) copyOut(w io.Writer, f *id3Frame) error {
	if _, err := w.Write(f.pre); err != nil {
		return err
	}
	r := t.src.r
	if a, ok := w.(*id3Ahead); ok {
		k := a.by[f]
		if k == nil {
			return ErrCorrupt
		}
		defer a.done(k)
		r = k
	}
	if !f.unsync {
		return copyRange(w, r, t.src.base+f.off, f.n)
	}
	d := t.stream(&id3Src{r: r, base: t.src.base}, f.off, f.raw, true)
	if err := d.skip(f.skip); err != nil {
		return err
	}
	_, err := io.CopyN(w, d, f.n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// fits is whether t can be written over the room bytes its old tag took, in place and a piece at a
// time (patchTo). A frame left in the file that moves towards the end is overtaken by the writing as
// it goes, so patchTo holds the bytes it moves by until they are copied: while that is no more than
// id3MaxLag in all, the frames stay in their order (a 2.2 tag's headers grow; a longer title before
// the cover pushes it along). Otherwise those kept stay in their order and a changed or new one goes
// in its place if it can, else after them all. t is left as it was when it doesn't fit.
func (t *id3Tag) fits(room int64) bool {
	if t.needed() > room {
		return false
	}
	if t.lag(t.frames) <= id3MaxLag {
		return true
	}
	// limit[i] is the furthest from the tag's start that frame i may start for the kept frames from it
	// on to be where they are or nearer the start.
	limit := make([]int64, len(t.frames)+1)
	limit[len(t.frames)] = 1 << 62
	for i := len(t.frames) - 1; i >= 0; i-- {
		f := t.frames[i]
		limit[i] = limit[i+1]
		if f.kept {
			limit[i] -= 10 + f.size()
			if f.lazy {
				limit[i] = min(limit[i], f.off-10-int64(len(f.pre)))
			}
		}
	}
	var order, after []*id3Frame
	at := int64(10)
	for i, f := range t.frames {
		if !f.kept && at+10+f.size() > limit[i+1] {
			after = append(after, f)
			continue
		}
		order = append(order, f)
		at += 10 + f.size()
	}
	order = append(order, after...)
	if t.lag(order) > id3MaxLag {
		return false
	}
	t.frames = order
	return true
}

// lag is how far, in all, frames written in this order from the tag's start put those left in the
// file past where they are now: what writing them over themselves holds while it copies them.
func (t *id3Tag) lag(frames []*id3Frame) int64 {
	at, n := int64(10), int64(0)
	for _, f := range frames {
		if f.lazy {
			n += max(0, at+10+int64(len(f.pre))-f.off)
		}
		at += 10 + f.size()
	}
	return n
}

// patchTo writes t over the room bytes at off in path its old tag took, in place, once fits said it
// can. Frames left in the file are copied there a piece at a time, through id3Ahead, so the writing
// never reads what it has written; the zeros after the new frames go only as far as the old tag held
// anything but zeros, and a little past.
func (t *id3Tag) patchTo(path string, off, room int64) error {
	n := room
	if t.src != nil {
		if t.src.base != off {
			return fmt.Errorf("tags: a tag read at %d written in place at %d", t.src.base, off)
		}
		n = min(room, max(t.needed(), t.used)+id3ZeroCheck)
	}
	if Stopping() {
		return ErrStopped
	}
	return patchWith(path, off, n, func(src *os.File, w io.Writer) error {
		return t.write(t.ahead(w, src, off), room, n)
	})
}

// ahead is w writing t over the tag at off in src, from its start, the frames t leaves there kept.
func (t *id3Tag) ahead(w io.Writer, src io.ReaderAt, off int64) *id3Ahead {
	a := &id3Ahead{w: w, by: map[*id3Frame]*id3Kept{}}
	for _, f := range t.frames {
		if f.lazy {
			k := &id3Kept{r: src, base: off, start: f.off, end: f.off + f.span(), at: f.off}
			a.keep = append(a.keep, k)
			a.by[f] = k
		}
	}
	sort.Slice(a.keep, func(i, j int) bool { return a.keep[i].start < a.keep[j].start })
	return a
}

// id3Ahead writes a tag over itself, from its start, without overtaking what it has still to copy:
// before the writing reaches a byte of a frame left in the file that isn't copied yet, that byte is
// read and held, as it was, until the copying takes it. fits keeps what is held small.
type id3Ahead struct {
	w    io.Writer
	at   int64      // where the next byte written goes, from the tag's start
	keep []*id3Kept // the frames left in the file, by where they are
	lo   int        // those before it are copied
	by   map[*id3Frame]*id3Kept
}

func (a *id3Ahead) Write(p []byte) (int, error) {
	to := a.at + int64(len(p))
	for _, k := range a.keep[a.lo:] {
		if k.start >= to {
			break
		}
		if err := k.fill(to); err != nil {
			return 0, err
		}
	}
	n, err := a.w.Write(p)
	a.at += int64(n)
	return n, err
}

// done lets k go once its frame is copied.
func (a *id3Ahead) done(k *id3Kept) {
	k.done, k.b = true, nil
	for a.lo < len(a.keep) && a.keep[a.lo].done {
		a.lo++
	}
}

// id3Kept is the bytes, start to end from the tag's start, that copying a frame left in the file
// reads from it, in order: at is how far that has got, b[i:j] the bytes from there on that the
// writing has reached, as they were.
type id3Kept struct {
	r          io.ReaderAt
	base       int64
	start, end int64
	at         int64
	b          []byte
	i, j       int
	done       bool
}

// fill holds the bytes before to that it doesn't hold yet and the copying hasn't read.
func (k *id3Kept) fill(to int64) error {
	have := k.at + int64(k.j-k.i)
	to = min(to, k.end)
	if k.done || to <= have {
		return nil
	}
	n := int(to - have)
	if k.j+n > len(k.b) {
		live := k.j - k.i
		if live+n > len(k.b) {
			b := make([]byte, max(2*len(k.b), live+n))
			copy(b, k.b[k.i:k.j])
			k.b = b
		} else {
			copy(k.b, k.b[k.i:k.j])
		}
		k.i, k.j = 0, live
	}
	if m, err := k.r.ReadAt(k.b[k.j:k.j+n], k.base+have); m < n {
		if err == nil || err == io.EOF {
			err = ErrCorrupt
		}
		return err
	}
	k.j += n
	return nil
}

// ReadAt reads the bytes in order, from at: those held, then the file's, which the writing hasn't
// reached.
func (k *id3Kept) ReadAt(p []byte, off int64) (int, error) {
	if k.done || off-k.base != k.at {
		return 0, fmt.Errorf("tags: a frame left in the file read out of order (%d, at %d)", off-k.base, k.at)
	}
	n := copy(p, k.b[k.i:k.j])
	if k.i += n; k.i == k.j {
		k.i, k.j = 0, 0
	}
	var err error
	if n < len(p) {
		var m int
		m, err = k.r.ReadAt(p[n:], off+int64(n))
		n += m
	}
	k.at += int64(n)
	return n, err
}
