package tags

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// MP4 (m4a, m4b, mp4): a tree of atoms, each a 32-bit size (1: a 64-bit one follows the type; 0: to the
// end) and a 4-byte type. The tags are moov/udta/meta/ilst, an item atom per tag (©nam, trkn, covr…)
// holding data atoms. A write rebuilds ilst: the items it doesn't change stay as they were, in their
// order; a changed one of the same size takes its old place, any other follows them. When a free atom
// right after ilst, meta, udta or moov has the room, the new tags take it and nothing else moves: the
// bytes that change are written in place, a few steps each on the card before the next, so that the
// file reads at every point (see plan), and what keeps its place (a cover) isn't written at all;
// otherwise the file is written again with 4 KB of free space after ilst for next time, and when the
// sample data comes after moov every chunk offset (stco, co64) of every track, chapter tracks too,
// moves with it. Missing udta, meta and ilst atoms are made. A read looks at a bounded number of atoms
// and bytes of text, so a crafted file can't make it big.

func init() { register(mp4Codec{}, ".m4a", ".m4b", ".mp4") }

type mp4Codec struct{}

const (
	mp4MaxSize   = math.MaxInt32 // past 2 GB: the Q2 doesn't play it, and it isn't changed
	mp4Padding   = 4 << 10       // the free atom a rewrite leaves after ilst
	mp4MaxPatch  = 8 << 20       // the most an in-place write writes, zeros apart; past it, a rewrite (an ending app stops it)
	mp4MaxHold   = 1 << 20       // the most of the file's own bytes an in-place write holds (those moving on)
	mp4MaxText   = 16 << 10      // the most text read for an item, all its values together
	mp4MaxLyrics = 256 << 10     // the same for the lyrics
	mp4MaxAtoms  = 1 << 12       // the most atoms one list holds; past it, it isn't read
	mp4MaxTop    = 1 << 14       // the most top-level atoms read; what's past them is kept as it is
	mp4MaxSeen   = 1 << 16       // the most atoms one read looks at
)

var errFragmented = errors.New("tags: this MP4 is in fragments, so its tags can only change within the room they have")

// mp4Keys are the items the fields are kept in.
var mp4Keys = map[Field]string{Title: "\xa9nam", Artist: "\xa9ART", Album: "\xa9alb", AlbumArtist: "aART",
	Date: "\xa9day", Genre: "\xa9gen", Composer: "\xa9wrt", Comment: "\xa9cmt", Lyrics: "\xa9lyr"}

// mp4Order is the order new items go in, after the items the file has.
var mp4Order = []string{"\xa9nam", "\xa9ART", "\xa9alb", "aART", "trkn", "disk", "\xa9day", "\xa9gen",
	"\xa9wrt", "\xa9cmt", "\xa9lyr", "covr"}

// mp4Hdlr is a new meta's body before its ilst: version and flags, and iTunes' handler.
var mp4Hdlr = []byte("\x00\x00\x00\x00" + "\x00\x00\x00\x21hdlr\x00\x00\x00\x00\x00\x00\x00\x00mdirappl" +
	"\x00\x00\x00\x00\x00\x00\x00\x00\x00")

type mp4Box struct {
	typ       string
	off       int64 // its header's offset
	hdr, size int64 // the header's length (8, or 16 with a 64-bit size) and the whole atom's
}

func (b mp4Box) body() int64 { return b.off + b.hdr }
func (b mp4Box) end() int64  { return b.off + b.size }

// mp4Level is one of moov, udta, meta and ilst: the atom, unless ok is false, and the atoms in it
// (ilst's items), which start skip bytes into its body (meta's version and flags) and end at tail,
// where anything too short for an atom is (QuickTime ends some lists with a 32-bit 0).
type mp4Level struct {
	box  mp4Box
	ok   bool
	skip int64
	kids []mp4Box
	tail int64
}

// mp4Table is a track's chunk offsets: n of them from off, 32-bit (stco) or 64-bit (co64).
type mp4Table struct {
	off, n int64
	wide   bool
}

func (t mp4Table) width() int64 {
	if t.wide {
		return 8
	}
	return 4
}

type mp4File struct {
	size    int64
	top     []mp4Box
	moov    int // in top
	brand   string
	chain   [4]mp4Level // moov, udta, meta, ilst
	tables  []mp4Table  // in file order
	trakErr error       // why the tracks couldn't be read, so their offsets can't be moved
	codec   string      // the first sound track's sample format: "mp4a", "alac"
	video   bool
	frag    bool // moof atoms: sample data the tracks don't list
	cmov    bool // a compressed moov
	seen    int  // the atoms the read has looked at
}

// mp4Name is whether t can be an atom's type.
func mp4Name(t string) bool {
	for i := 0; i < len(t); i++ {
		if c := t[i]; (c < 0x20 || c > 0x7e) && c != 0xa9 {
			return false
		}
	}
	return len(t) == 4
}

func mp4Free(t string) bool { return t == "free" || t == "skip" }

// mp4At is the atom at off, which must end by to.
func mp4At(r io.ReaderAt, off, to int64) (mp4Box, error) {
	h, err := readAt(r, off, 8)
	if err != nil {
		return mp4Box{}, err
	}
	b := mp4Box{typ: string(h[4:]), off: off, hdr: 8, size: int64(binary.BigEndian.Uint32(h))}
	switch b.size {
	case 0:
		b.size = to - off
	case 1:
		x, err := readAt(r, off+8, 8)
		if err != nil {
			return mp4Box{}, err
		}
		n := binary.BigEndian.Uint64(x)
		if n > math.MaxInt64 {
			return mp4Box{}, ErrCorrupt
		}
		b.hdr, b.size = 16, int64(n)
	}
	if b.size < b.hdr || b.size > to-off {
		return mp4Box{}, ErrCorrupt
	}
	return b, nil
}

// list is the atoms from from to to, and where the bytes too few for another one start; ErrCorrupt
// past mp4MaxAtoms of them, or once the read has looked at mp4MaxSeen atoms in all.
func (m *mp4File) list(r io.ReaderAt, from, to int64) ([]mp4Box, int64, error) {
	var out []mp4Box
	off := from
	for to-off >= 8 {
		if len(out) == mp4MaxAtoms || m.seen >= mp4MaxSeen {
			return nil, 0, ErrCorrupt
		}
		m.seen++
		b, err := mp4At(r, off, to)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, b)
		off = b.end()
	}
	return out, off, nil
}

// level reads the atom b as one of the chain's levels.
func (m *mp4File) level(r io.ReaderAt, b mp4Box) (mp4Level, error) {
	lv := mp4Level{box: b, ok: true}
	if b.typ == "meta" && b.size-b.hdr >= 4 {
		// A full box (version and flags first), but QuickTime's has its atoms straight away.
		lv.skip = 4
		if h, err := readAt(r, b.body(), 8); err == nil && mp4Name(string(h[4:])) && binary.BigEndian.Uint32(h) >= 8 {
			lv.skip = 0
		}
	}
	var err error
	lv.kids, lv.tail, err = m.list(r, b.body()+lv.skip, b.end())
	return lv, err
}

func readMP4(r io.ReaderAt, size int64) (*mp4File, error) {
	m := &mp4File{size: size, moov: -1}
	for off := int64(0); size-off >= 8; {
		if len(m.top) == mp4MaxTop {
			if m.moov < 0 {
				return nil, ErrCorrupt
			}
			m.frag = true // what's past here isn't known, so it mustn't move
			break
		}
		m.seen++
		b, err := mp4At(r, off, size)
		if err == nil && !mp4Name(b.typ) {
			err = ErrCorrupt
		}
		if err != nil {
			if err != ErrCorrupt || m.moov < 0 {
				return nil, err
			}
			break // after moov: what's left is kept as it is
		}
		switch b.typ {
		case "ftyp":
			if h, err := readAt(r, b.body(), 4); err == nil {
				m.brand = string(h)
			}
		case "moov":
			if m.moov < 0 {
				m.moov = len(m.top)
			}
		case "moof":
			m.frag = true
		}
		m.top = append(m.top, b)
		off = b.end()
	}
	if m.moov < 0 {
		return nil, ErrCorrupt
	}
	var err error
	if m.chain[0], err = m.level(r, m.top[m.moov]); err != nil {
		return nil, err
	}
	for _, k := range m.chain[0].kids {
		switch k.typ {
		case "trak":
			if err := m.trak(r, k); err != nil && m.trakErr == nil {
				m.trakErr = err
			}
		case "cmov":
			m.cmov = true
		}
	}
	for k, want := range []string{"udta", "meta", "ilst"} {
		for _, b := range m.chain[k].kids {
			if b.typ == want {
				if m.chain[k+1], err = m.level(r, b); err != nil {
					return nil, err
				}
				break
			}
		}
		if !m.chain[k+1].ok {
			break
		}
	}
	return m, nil
}

// mp4Inner is the atom a track's walk goes into from each: trak, mdia, minf, stbl, no deeper.
var mp4Inner = map[string]string{"trak": "mdia", "mdia": "minf", "minf": "stbl"}

// trak notes a track's kind, its sample format and where its chunk offsets are.
func (m *mp4File) trak(r io.ReaderAt, trak mp4Box) error {
	var handler, format string
	var walk func(b mp4Box) error
	walk = func(b mp4Box) error {
		kids, _, err := m.list(r, b.body(), b.end())
		if err != nil {
			return err
		}
		for _, k := range kids {
			switch {
			case k.typ == mp4Inner[b.typ]:
				if err := walk(k); err != nil {
					return err
				}
			case k.typ == "hdlr" && b.typ == "mdia":
				if h, err := readAt(r, k.body()+8, 4); err == nil {
					handler = string(h)
				}
			case k.typ == "stsd" && b.typ == "stbl":
				if h, err := readAt(r, k.body()+12, 4); err == nil {
					format = string(h)
				}
			case (k.typ == "stco" || k.typ == "co64") && b.typ == "stbl":
				if len(m.tables) == mp4MaxAtoms {
					return ErrCorrupt
				}
				h, err := readAt(r, k.body(), 8)
				if err != nil {
					return err
				}
				t := mp4Table{off: k.body() + 8, n: int64(binary.BigEndian.Uint32(h[4:])), wide: k.typ == "co64"}
				if t.off+t.n*t.width() > k.end() {
					return ErrCorrupt
				}
				m.tables = append(m.tables, t)
			}
		}
		return nil
	}
	if err := walk(trak); err != nil {
		return err
	}
	switch handler {
	case "soun":
		if m.codec == "" {
			m.codec = format
		}
	case "vide":
		m.video = true
	}
	return nil
}

func (m *mp4File) format(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case ext == ".m4b" || m.brand == "M4B ":
		return "M4B (audiobook)"
	case ext == ".mp4" || m.video:
		return "MP4"
	case m.codec == "mp4a":
		return "M4A (AAC)"
	case m.codec == "alac":
		return "M4A (ALAC)"
	}
	return "M4A"
}

// mp4Data is an item's data atom: its type (1 UTF-8, 2 UTF-16, 13 JPEG, 14 PNG, 0 numbers) and where
// its value is.
type mp4Data struct {
	typ    uint32
	off, n int64
}

func (m *mp4File) datas(r io.ReaderAt, item mp4Box) []mp4Data {
	kids, _, err := m.list(r, item.body(), item.end())
	if err != nil {
		return nil
	}
	var out []mp4Data
	for _, k := range kids {
		if k.typ != "data" || k.size-k.hdr < 8 {
			continue
		}
		if h, err := readAt(r, k.body(), 4); err == nil {
			out = append(out, mp4Data{typ: binary.BigEndian.Uint32(h) & 0xffffff, off: k.body() + 8, n: k.end() - k.body() - 8})
		}
	}
	return out
}

// mp4Text is a text value: UTF-8 (types 1 and 4, and 0 when a tagger left it out) or UTF-16 (2, 5).
func mp4Text(typ uint32, b []byte) (string, bool) {
	switch typ {
	case 0, 1, 4:
		return string(b), true
	case 2, 5:
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(b[2*i:])
		}
		if len(u) > 0 && u[0] == 0xfeff {
			u = u[1:]
		}
		return string(utf16.Decode(u)), true
	}
	return "", false
}

// tags are the fields ilst holds, and its first cover.
func (m *mp4File) tags(r io.ReaderAt) (map[Field]string, *picRef) {
	out := map[Field]string{}
	if !m.chain[3].ok {
		return out, nil
	}
	text := map[string][]string{}
	left := map[string]int64{} // the text still read for an item: a long or repeated one is cut
	for _, k := range mp4Keys {
		left[k] = mp4MaxText
	}
	left[mp4Keys[Lyrics]] = mp4MaxLyrics
	var pic *picRef
	for _, it := range m.chain[3].kids {
		switch it.typ {
		case "trkn", "disk":
			n, of := Track, TrackTotal
			if it.typ == "disk" {
				n, of = Disc, DiscTotal
			}
			for _, d := range m.datas(r, it) {
				if d.n < 6 {
					continue
				}
				b, err := readAt(r, d.off, 6)
				if err != nil {
					continue
				}
				if v := binary.BigEndian.Uint16(b[2:]); v > 0 && out[n] == "" {
					out[n] = strconv.Itoa(int(v))
				}
				if v := binary.BigEndian.Uint16(b[4:]); v > 0 && out[of] == "" {
					out[of] = strconv.Itoa(int(v))
				}
				break
			}
		case "gnre":
			for _, d := range m.datas(r, it) {
				if d.n < 2 {
					continue
				}
				if b, err := readAt(r, d.off, 2); err == nil {
					if g := genreName(int(binary.BigEndian.Uint16(b)) - 1); g != "" && len(text["gnre"]) == 0 {
						text["gnre"] = []string{g}
					}
					break
				}
			}
		case "covr":
			if pic != nil {
				continue
			}
			for _, d := range m.datas(r, it) {
				if pic != nil {
					break
				}
				mime := map[uint32]string{13: "image/jpeg", 14: "image/png"}[d.typ]
				if mime == "" {
					if head, err := readAt(r, d.off, min(d.n, 64)); err == nil {
						mime = pictureInfo(head).MIME
					}
				}
				if mime != "" {
					pic = atFile(mime, 3, "", d.off, d.n)
				}
			}
		default:
			if left[it.typ] <= 0 {
				continue // not a text item, or one read as far as it goes
			}
			for _, d := range m.datas(r, it) {
				if left[it.typ] <= 0 {
					break
				}
				n := min(d.n, left[it.typ])
				b, err := readAt(r, d.off, n)
				if err != nil {
					continue
				}
				left[it.typ] -= n
				if v, ok := mp4Text(d.typ, b); ok && strings.TrimSpace(v) != "" {
					text[it.typ] = append(text[it.typ], strings.TrimSpace(v))
				}
			}
		}
	}
	for f, k := range mp4Keys {
		if vs := text[k]; len(vs) > 0 {
			out[f] = strings.Join(vs, " / ")
		}
	}
	if out[Genre] == "" && len(text["gnre"]) > 0 {
		out[Genre] = text["gnre"][0]
	}
	return out, pic
}

func (mp4Codec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	m, err := readMP4(r, size)
	if err != nil {
		return nil, nil, err
	}
	var name string
	if f, ok := r.(interface{ Name() string }); ok {
		name = f.Name()
	}
	fields, pic := m.tags(r)
	info := &Info{Format: m.format(name), Writable: size <= mp4MaxSize && !m.cmov, Fields: fields}
	switch {
	case m.cmov:
		info.Notes = append(info.Notes, "Its header is compressed, which this can't change")
	case size > mp4MaxSize:
		info.Notes = append(info.Notes, "It's over 2 GB, too big to change here")
	case m.frag:
		info.Notes = append(info.Notes, "It's an MP4 in fragments: its tags can change only within the room they have")
	}
	return info, pic, nil
}

// mp4Seg is a piece of what's written: data, else n bytes at off in the file, or n zero bytes when
// off is negative. slot, when not 0, is where the old bytes it replaces, of the same length, start:
// written there, it changes a value and nothing around it.
type mp4Seg struct {
	data         []byte
	off, n, slot int64
}

func (s mp4Seg) len() int64 {
	if s.data != nil {
		return int64(len(s.data))
	}
	return s.n
}

func segLen(segs []mp4Seg) int64 {
	var n int64
	for _, s := range segs {
		n += s.len()
	}
	return n
}

// mp4Span is the file's bytes from from to to, nil when there are none.
func mp4Span(from, to int64) []mp4Seg {
	if to <= from {
		return nil
	}
	return []mp4Seg{{off: from, n: to - from}}
}

// mp4Header is an atom's header: hdr is 16 for a 64-bit size.
func mp4Header(typ string, hdr, size int64) []byte {
	if hdr == 16 {
		b := make([]byte, 16)
		binary.BigEndian.PutUint32(b, 1)
		copy(b[4:], typ)
		binary.BigEndian.PutUint64(b[8:], uint64(size))
		return b
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(size))
	copy(b[4:], typ)
	return b
}

// mp4Item is an item atom holding one data atom of type typ: header, data header, then value, which
// isn't copied.
func mp4Item(key string, typ uint32, value []byte) []mp4Seg {
	n := int64(len(value))
	d := append(mp4Header("data", 8, 16+n), 0, 0, 0, 0, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(d[8:], typ)
	return []mp4Seg{{data: append(mp4Header(key, 8, 24+n), d...)}, {data: value}}
}

// mp4FreeAtom is a free atom of n bytes in all.
func mp4FreeAtom(n int64) []mp4Seg {
	return []mp4Seg{{data: mp4Header("free", 8, n)}, {off: -1, n: n - 8}}
}

// ilst is the new ilst atom, c made to cur: the items c doesn't change stay as they were, in their
// order. A changed one of the same size as an old copy takes that copy's place, so in place only its
// value is written; any other (its old copies gone) follows the kept items, so that in place nothing
// kept moves on towards the end of the file, where it would have to be held while the writing passes
// it. A number and its total are written together, so "3 of 12" doesn't lose its other half.
func (m *mp4File) ilst(cur map[Field]string, c Changes) []mp4Seg {
	repl := map[string][]mp4Seg{} // item → its new atom, nil to remove it
	for f, v := range c.Set {
		if k, ok := mp4Keys[f]; ok {
			repl[k] = nil
			if v != "" {
				repl[k] = mp4Item(k, 1, []byte(v))
			}
		}
	}
	if _, ok := c.Set[Genre]; ok {
		repl["gnre"] = nil // the old numbered genre
	}
	for _, p := range []struct {
		key   string
		n, of Field
		size  int
	}{{"trkn", Track, TrackTotal, 8}, {"disk", Disc, DiscTotal, 6}} {
		n, ok1 := c.Set[p.n]
		of, ok2 := c.Set[p.of]
		if !ok1 && !ok2 {
			continue
		}
		if !ok1 {
			n = cur[p.n]
		}
		if !ok2 {
			of = cur[p.of]
		}
		repl[p.key] = nil
		if n != "" || of != "" {
			a, _ := strconv.Atoi(n)
			b, _ := strconv.Atoi(of)
			v := make([]byte, p.size)
			binary.BigEndian.PutUint16(v[2:], uint16(a))
			binary.BigEndian.PutUint16(v[4:], uint16(b))
			repl[p.key] = mp4Item(p.key, 0, v)
		}
	}
	if c.Picture != nil {
		typ := uint32(13)
		if c.Picture.MIME == "image/png" {
			typ = 14
		}
		repl["covr"] = mp4Item("covr", typ, c.Picture.Data)
	} else if c.RemovePicture {
		repl["covr"] = nil
	}
	var body []mp4Seg
	placed := map[string]bool{}
	for _, it := range m.chain[3].kids {
		if s, ok := repl[it.typ]; ok {
			if s != nil && !placed[it.typ] && segLen(s) == it.size {
				placed[it.typ] = true
				body = append(body, mp4Slot(s, it.off)...)
			}
			continue
		}
		if k := len(body) - 1; k >= 0 && body[k].data == nil && body[k].off+body[k].n == it.off {
			body[k].n += it.size // one piece with the item before it
			continue
		}
		body = append(body, mp4Seg{off: it.off, n: it.size})
	}
	for _, k := range mp4Order {
		if !placed[k] {
			body = append(body, repl[k]...)
		}
	}
	lv := m.chain[3]
	hdr := int64(8)
	if lv.ok {
		hdr = lv.box.hdr
	}
	size := hdr + segLen(body)
	head := mp4Seg{data: mp4Header("ilst", hdr, size)}
	// The same size: its header as it is, unless that may say 0 (to meta's end), which a free atom
	// after it would join.
	if lv.ok && lv.box.size == size && (hdr == 16 || lv.box.end() < m.chain[2].box.end()) {
		head = mp4Seg{off: lv.box.off, n: hdr}
	}
	return append([]mp4Seg{head}, body...)
}

// mp4Slot is segs put in the place of old bytes of the same length from at.
func mp4Slot(segs []mp4Seg, at int64) []mp4Seg {
	out := make([]mp4Seg, len(segs))
	for i, s := range segs {
		s.slot = at
		at += s.len()
		out[i] = s
	}
	return out
}

// span is where the chain's level k atom is among its parent's atoms: kids[i:i+n], n 0 when it isn't
// there and goes in at i (before the free atoms that end the list); free atoms follow it, kids[i+n:
// i+n+free].
func (m *mp4File) span(k int) (kids []mp4Box, i, n, free int) {
	if k == 0 {
		kids, i, n = m.top, m.moov, 1
	} else {
		kids = m.chain[k-1].kids
		i = len(kids)
		if m.chain[k].ok {
			for j, b := range kids {
				if b.off == m.chain[k].box.off {
					i, n = j, 1
				}
			}
		} else {
			for i > 0 && mp4Free(kids[i-1].typ) {
				i--
			}
		}
	}
	for i+n+free < len(kids) && mp4Free(kids[i+n+free].typ) {
		free++
	}
	return kids, i, n, free
}

// at is where kids[i] of the chain's level k-1 starts: its tail past the last one.
func (m *mp4File) at(k int, kids []mp4Box, i int) int64 {
	switch {
	case i < len(kids):
		return kids[i].off
	case k == 0:
		return m.size
	}
	return m.chain[k-1].tail
}

// build is the chain's level k atom as it will be, the new ilst at the bottom; the free atoms that
// followed ilst in meta go (their room is the level's to use), and when pad > 0 a free atom of pad
// bytes takes their place.
func (m *mp4File) build(k int, ilst []mp4Seg, pad int64) []mp4Seg {
	if k == 3 {
		return ilst
	}
	inner := m.build(k+1, ilst, pad)
	if k == 2 && pad > 0 {
		inner = append(inner, mp4FreeAtom(pad)...)
	}
	lv := m.chain[k]
	typ := [...]string{"moov", "udta", "meta"}[k]
	if !lv.ok {
		var body []mp4Seg
		if k == 2 {
			body = append(body, mp4Seg{data: mp4Hdlr})
		}
		body = append(body, inner...)
		return append([]mp4Seg{{data: mp4Header(typ, 8, 8+segLen(body))}}, body...)
	}
	kids, i, n, free := m.span(k + 1)
	rest := i + n
	if k == 2 {
		rest += free
	}
	body := mp4Span(lv.box.body(), m.at(k+1, kids, i))
	body = append(body, inner...)
	body = append(body, mp4Span(m.at(k+1, kids, rest), lv.box.end())...)
	size := lv.box.hdr + segLen(body)
	head := mp4Seg{data: mp4Header(lv.box.typ, lv.box.hdr, size)}
	if size == lv.box.size {
		head = mp4Seg{off: lv.box.off, n: lv.box.hdr}
	}
	return append([]mp4Seg{head}, body...)
}

// mp4Run is one write of an in-place save: segs laid down from at, one after another.
type mp4Run struct {
	at   int64
	segs []mp4Seg
}

// mp4Patch is an in-place save: steps of writes, each step on the card (synced) before the next
// begins, so a save cut short anywhere leaves a file that reads (see plan).
type mp4Patch struct {
	steps [][]mp4Run
}

// step adds a step of runs, unless there are none.
func (p *mp4Patch) step(runs ...mp4Run) {
	if len(runs) > 0 {
		p.steps = append(p.steps, runs)
	}
}

// end is where the last byte p writes ends, 0 when it writes none.
func (p mp4Patch) end() int64 {
	var e int64
	for _, st := range p.steps {
		for _, r := range st {
			e = max(e, r.at+segLen(r.segs))
		}
	}
	return e
}

// inPlace is the write that puts the new ilst in the room the file has, ok false when it hasn't any:
// free atoms right after ilst, meta, udta or moov (or where a missing one goes), or what a smaller
// ilst leaves, which becomes a free atom.
func (m *mp4File) inPlace(r io.ReaderAt, ilst []mp4Seg) (p mp4Patch, ok bool, err error) {
	deepest := 0
	for deepest < 3 && m.chain[deepest+1].ok {
		deepest++
	}
	for k := min(deepest+1, 3); k >= 0; k-- {
		kids, i, n, free := m.span(k)
		at := m.at(k, kids, i)
		var old, slack int64
		if n == 1 {
			old = kids[i].size
		}
		for _, b := range kids[i+n : i+n+free] {
			slack += b.size
		}
		segs := m.build(k, ilst, 0)
		rem := old + slack - segLen(segs)
		if rem < 0 || rem > 0 && rem < 8 {
			continue
		}
		if rem > 0 {
			// The free atom's body is zeroed over what the old atoms held, up to the last old free
			// atom's body, which was free already.
			end := at + old
			if free > 0 {
				end = kids[i+n+free-1].body()
			}
			z := max(end-(at+segLen(segs)+8), 0)
			segs = append(segs, mp4Seg{data: mp4Header("free", 8, rem)}, mp4Seg{off: -1, n: z})
		}
		if p, ok, err = m.plan(r, k, at, at+old+slack, segs, ilst); ok || err != nil {
			return p, ok, err
		}
	}
	return mp4Patch{}, false, nil
}

// plan is the in-place write of segs, the chain's level k atom as it will be and the free atom after
// it, over the file from at to end, the room's end. Bytes of the file that keep their place aren't
// written. cut is where the new bytes first differ from the file's, but for the levels' sizes and
// values written over old ones of the same length (which change nothing around them, and are written
// with the new bytes); from cut on, all of it is new or moved. The steps, each on the card before the
// next, keep the file readable at every point:
//
//  1. the levels that run past cut end there, innermost first (what followed, the old items from cut
//     on and the free room, is left outside ilst as atoms of their own);
//  2. one free atom from cut to the end hides all that;
//  3. the new bytes go in behind it, past its header;
//  4. its header becomes the new bytes' first 8, which shows them as atoms outside ilst;
//  5. the levels take their new sizes, outermost first, which puts them in;
//  6. the room left over is zeroed, with the last step.
//
// So an addition reads as the old tags until step 5 and as the new ones after it, and an edit lacks
// the items from cut on only between steps 1 and 5. When a level holds other atoms after the next one
// in (chpl after meta, a track after udta), step 2 would hide them: then the bytes from cut go in one
// pass and the sizes after. ok is false when the bytes that move towards the end, held while the
// writing passes them, are over mp4MaxHold, or the bytes written, zeros apart, over mp4MaxPatch.
func (m *mp4File) plan(r io.ReaderAt, k int, at, end int64, segs, ilst []mp4Seg) (mp4Patch, bool, error) {
	pos := make([]int64, len(segs)+1)
	pos[0] = at
	for i, s := range segs {
		pos[i+1] = pos[i] + s.len()
	}
	var lvs []mp4Box // the levels from k down that the file has, outermost first
	for j := k; j <= 3 && m.chain[j].ok; j++ {
		lvs = append(lvs, m.chain[j].box)
	}
	keeps := func(i int) bool {
		s := segs[i]
		if s.data == nil && s.off == pos[i] || s.slot > 0 && s.slot == pos[i] {
			return true
		}
		for _, b := range lvs {
			if pos[i] == b.off && s.len() == b.hdr {
				return true // a level's header: its size is written apart
			}
		}
		return false
	}
	c := 0
	for c < len(segs) && keeps(c) {
		c++
	}
	cut := pos[c]
	z := len(segs) // the zeros that end the free atom, from z
	if z > c && segs[z-1].data == nil && segs[z-1].off < 0 {
		z--
	}
	var fill []mp4Run // values written over old ones of the same length (but for headers as they were)
	for i := 0; i < c; i++ {
		s := segs[i]
		if s.slot == 0 {
			continue
		}
		if n := s.len(); n <= 64 {
			if b, err := readAt(r, pos[i], n); err == nil && bytes.Equal(b, s.data) {
				continue
			}
		}
		fill = mp4Join(fill, pos[i], s)
	}
	heads := make([][]byte, len(lvs)) // the levels' headers as the file holds them
	for i, b := range lvs {
		h, err := readAt(r, b.off, b.hdr)
		if err != nil {
			return mp4Patch{}, false, err
		}
		heads[i] = h
	}
	hide := c < z
	for j := k; j < k+len(lvs) && j < 3; j++ {
		kids, i, n, _ := m.span(j + 1)
		for _, b := range kids[i+n:] {
			hide = hide && mp4Free(b.typ)
		}
	}
	if c < z && !hide {
		// A level holds atoms after the next one in (chpl after meta, as ffmpeg writes chapters): a
		// one-pass write cut short would leave neither file, so the file is written again (with room
		// after ilst, where every later save is in place by the steps).
		return mp4Patch{}, false, nil
	}
	var p mp4Patch
	var news []mp4Run
	var held int64
	var err error
	hidden := 0 // the steps after which the zeros' room is behind a free atom's header
	if hide {
		for i := len(lvs) - 1; i >= 0; i-- {
			if b := lvs[i]; b.end() > cut {
				heads[i] = mp4Header(b.typ, b.hdr, cut-b.off)
				p.step(mp4Run{at: b.off, segs: []mp4Seg{{data: heads[i]}}})
			}
		}
		cover := mp4Header("free", 8, end-cut)
		var was []byte
		if was, err = readAt(r, cut, 8); err != nil {
			return mp4Patch{}, false, err
		}
		if !bytes.Equal(was, cover) {
			p.step(mp4Run{at: cut, segs: []mp4Seg{{data: cover}}})
		}
		hidden = len(p.steps)
		// The first 8 bytes are read now: the writing may pass where the file holds them.
		var first bytes.Buffer
		if err = mp4Lay(&first, r, mp4Part(segs[c:z], 0, 8)); err != nil {
			return mp4Patch{}, false, err
		}
		if news, held, err = mp4Runs(r, cut+8, mp4Part(segs[c:z], 8, pos[z]-cut)); err != nil {
			return mp4Patch{}, false, err
		}
		p.step(append(fill, news...)...)
		if !bytes.Equal(first.Bytes(), cover) {
			p.step(mp4Run{at: cut, segs: []mp4Seg{{data: first.Bytes()}}})
		}
	} else if news, held, err = mp4Runs(r, cut, segs[c:z]); err != nil {
		return mp4Patch{}, false, err
	} else {
		p.step(append(fill, news...)...)
	}
	var written int64
	for _, rs := range [][]mp4Run{fill, news} {
		for _, rn := range rs {
			written += segLen(rn.segs)
		}
	}
	if held > mp4MaxHold || written > mp4MaxPatch {
		return mp4Patch{}, false, nil
	}
	for i, b := range lvs {
		if h := mp4Header(b.typ, b.hdr, segLen(m.build(k+i, ilst, 0))); !bytes.Equal(h, heads[i]) {
			p.step(mp4Run{at: b.off, segs: []mp4Seg{{data: h}}})
		}
	}
	// From step 2 on, the zeros land behind a free atom's header (the one that hides, then the new one),
	// so they go with the last step, unless that is step 1 or 2.
	if zs := segs[z:]; segLen(zs) > 0 {
		if n := len(p.steps) - 1; n >= hidden {
			p.steps[n] = append(p.steps[n], mp4Run{at: pos[z], segs: zs})
		} else {
			p.step(mp4Run{at: pos[z], segs: zs})
		}
	}
	return p, true, nil
}

// mp4Runs are the writes that lay segs down from at, leaving out the file's bytes that are in their
// place already. Those that stay or move towards the start are copied as the writing goes; those that
// move towards the end, which the writing would reach before they're read, are read first: held
// counts them, and past mp4MaxHold no more are read.
func mp4Runs(r io.ReaderAt, at int64, segs []mp4Seg) (runs []mp4Run, held int64, err error) {
	for _, s := range segs {
		n := s.len()
		if n > 0 && (s.data != nil || s.off != at) {
			if s.data == nil && s.off >= 0 && s.off < at {
				if held += n; held > mp4MaxHold {
					return nil, held, nil
				}
				b, err := readAt(r, s.off, n)
				if err != nil {
					return nil, held, err
				}
				s = mp4Seg{data: b}
			}
			runs = mp4Join(runs, at, s)
		}
		at += n
	}
	return runs, held, nil
}

// mp4Join is runs with s, laid down at at, added: to the last run when that ends there.
func mp4Join(runs []mp4Run, at int64, s mp4Seg) []mp4Run {
	if k := len(runs) - 1; k >= 0 && runs[k].at+segLen(runs[k].segs) == at {
		runs[k].segs = append(runs[k].segs, s)
		return runs
	}
	return append(runs, mp4Run{at: at, segs: []mp4Seg{s}})
}

// mp4Part is the bytes of segs from from to to.
func mp4Part(segs []mp4Seg, from, to int64) []mp4Seg {
	var out []mp4Seg
	var p int64
	for _, s := range segs {
		n := s.len()
		a, b := max(from-p, 0), min(to-p, n)
		p += n
		if a >= b {
			continue
		}
		switch {
		case s.data != nil:
			s.data = s.data[a:b]
		case s.off >= 0:
			s.off, s.n = s.off+a, b-a
		default:
			s.n = b - a
		}
		if s.slot > 0 {
			s.slot += a
		}
		out = append(out, s)
	}
	return out
}

// mp4Out is where an in-place save goes: the song, opened to write.
type mp4Out interface {
	io.WriterAt
	Sync() error
	Close() error
}

// mp4Open opens path for an in-place save; a test may stand in for it.
var mp4Open = func(path string) (mp4Out, error) { return os.OpenFile(path, os.O_WRONLY, 0) }

// apply makes the in-place save p to path through one open file, so the card can't go between its
// steps; an app that is ending doesn't start it.
func (p mp4Patch) apply(path string) error {
	if len(p.steps) == 0 {
		return nil
	}
	if !CardReady() {
		return ErrNoCard
	}
	if Stopping() {
		return ErrStopped
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
	if e := p.end(); e > st.Size() {
		return fmt.Errorf("tags: a patch past the end (%d > %d)", e, st.Size())
	}
	w, err := mp4Open(path)
	if err != nil {
		return err
	}
	err = p.lay(w, src)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// lay writes p's steps to w, the file's bytes in them copied from src, syncing after each.
func (p mp4Patch) lay(w mp4Out, src io.ReaderAt) error {
	bw := bufio.NewWriterSize(nil, 64<<10)
	for _, st := range p.steps {
		for _, r := range st {
			bw.Reset(&mp4Pen{w: w, off: r.at})
			if err := mp4Lay(bw, src, r.segs); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
		}
		if err := w.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// mp4Pen writes to w in order from off.
type mp4Pen struct {
	w   io.WriterAt
	off int64
}

func (p *mp4Pen) Write(b []byte) (int, error) {
	n, err := p.w.WriteAt(b, p.off)
	p.off += int64(n)
	return n, err
}

// mp4Lay writes segs, the file's bytes in them copied from src.
func mp4Lay(w io.Writer, src io.ReaderAt, segs []mp4Seg) error {
	for _, s := range segs {
		var err error
		switch {
		case s.data != nil:
			_, err = w.Write(s.data)
		case s.off < 0:
			err = mp4Zeros(w, s.n)
		default:
			err = copyRange(w, src, s.off, s.n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mp4Zeros writes n zero bytes, a piece at a time.
func mp4Zeros(w io.Writer, n int64) error {
	z := make([]byte, min(n, 32<<10))
	for n > 0 {
		k := min(n, int64(len(z)))
		if _, err := w.Write(z[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

func (mp4Codec) write(path string, c Changes) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > mp4MaxSize {
		return ErrTooBig
	}
	m, err := readMP4(f, st.Size())
	if err != nil {
		return err
	}
	if m.cmov {
		return ErrReadOnly
	}
	cur, _ := m.tags(f)
	ilst := m.ilst(cur, c)
	if !m.chain[3].ok && segLen(ilst) == 8 {
		return nil // nothing to add, and no tags to take away
	}
	p, ok, err := m.inPlace(f, ilst)
	if err != nil {
		return err
	}
	if !ok {
		return m.rewrite(path, ilst)
	}
	f.Close()
	return p.apply(path)
}

// rewrite writes the file again with the new moov, moving every chunk offset that points past it.
func (m *mp4File) rewrite(path string, ilst []mp4Seg) error {
	moov := m.top[m.moov]
	segs := m.build(0, ilst, mp4Padding)
	delta := segLen(segs) - moov.size
	newSize := m.size + delta
	if newSize > mp4MaxSize {
		return ErrTooBig
	}
	if delta != 0 && moov.end() < m.size {
		if m.trakErr != nil {
			return m.trakErr
		}
		if m.frag {
			return errFragmented
		}
	}
	return rewrite(path, newSize, func(src *os.File, w io.Writer) error {
		if err := copyRange(w, src, 0, moov.off); err != nil {
			return err
		}
		for _, s := range segs {
			var err error
			switch {
			case s.data != nil:
				_, err = w.Write(s.data)
			case s.off < 0:
				err = mp4Zeros(w, s.n)
			default:
				err = m.copyShifted(w, src, s.off, s.n, moov.end(), delta)
			}
			if err != nil {
				return err
			}
		}
		return copyRange(w, src, moov.end(), m.size-moov.end())
	})
}

// copyShifted copies n bytes at off, moving each chunk offset in them that points at from or past it
// by delta.
func (m *mp4File) copyShifted(w io.Writer, src io.ReaderAt, off, n, from, delta int64) error {
	end := off + n
	for _, t := range m.tables {
		tEnd := t.off + t.n*t.width()
		if delta == 0 || tEnd <= off || t.off >= end || t.n == 0 {
			continue
		}
		if t.off < off || tEnd > end {
			return ErrCorrupt
		}
		if err := copyRange(w, src, off, t.off-off); err != nil {
			return err
		}
		if err := t.shift(w, src, from, delta); err != nil {
			return err
		}
		off = tEnd
	}
	return copyRange(w, src, off, end-off)
}

// shift writes the table's offsets, those at from or past it moved by delta; ErrTooBig when a 32-bit
// one would overflow.
func (t mp4Table) shift(w io.Writer, src io.ReaderAt, from, delta int64) error {
	wd := t.width()
	for i := int64(0); i < t.n; {
		k := min(t.n-i, 8192)
		b, err := readAt(src, t.off+i*wd, k*wd)
		if err != nil {
			return err
		}
		for e := b; len(e) > 0; e = e[wd:] {
			if t.wide {
				if v := binary.BigEndian.Uint64(e); v >= uint64(from) {
					binary.BigEndian.PutUint64(e, v+uint64(delta))
				}
				continue
			}
			if v := int64(binary.BigEndian.Uint32(e)); v >= from {
				if v += delta; v > math.MaxUint32 || v < 0 {
					return ErrTooBig
				}
				binary.BigEndian.PutUint32(e, uint32(v))
			}
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		i += k
	}
	return nil
}
