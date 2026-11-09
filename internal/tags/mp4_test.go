package tags

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unicode/utf16"
)

// atom is an MP4 atom of typ around parts.
func mp4Atom(typ string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	b := binary.BigEndian.AppendUint32(make([]byte, 0, n), uint32(n))
	b = append(b, typ...)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func mp4BE32(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func mp4DataAtom(typ uint32, v []byte) []byte { return mp4Atom("data", mp4BE32(typ, 0), v) }

func mp4TextItem(key string, vs ...string) []byte {
	var ds [][]byte
	for _, v := range vs {
		ds = append(ds, mp4DataAtom(1, []byte(v)))
	}
	return mp4Atom(key, ds...)
}

var mp4SMPB = mp4Atom("----", mp4Atom("mean", mp4BE32(0), []byte("com.apple.iTunes")), mp4Atom("name", mp4BE32(0), []byte("iTunSMPB")),
	mp4DataAtom(1, []byte(" 00000000 00000840 000001C0 0000000000013C00")))

var mp4JPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, bytes.Repeat([]byte{7}, 3000)...)

// mp4Fix describes a small MP4: a sound track with three chunks of "audio", a chapter text track with
// two when chapters is set, and the tags in moov/udta/meta/ilst.
type mp4Fix struct {
	moovFirst, co64, chapters, bigMdat bool
	bigMoov                            bool // moov's size in 64 bits
	noUdta, noMeta, noIlst, qtMeta     bool
	noChpl                             bool // udta without chpl, so a free atom can follow meta
	items                              [][]byte
	metaFree, udtaFree, topFree        int  // free atoms after ilst, after meta, after moov
	freeFill                           byte // what the free atoms hold
	codec                              string
	extra                              []uint32 // more 32-bit offsets for the sound track
	after                              []byte   // top-level atoms at the end
}

func (o mp4Fix) chunks() [][]byte {
	var cs [][]byte
	for i := 0; i < 3; i++ {
		cs = append(cs, bytes.Repeat([]byte{byte(0x10 + i)}, 100+i))
	}
	if o.chapters {
		cs = append(cs, []byte("\x00\x05Intro"), []byte("\x00\x05Outro"))
	}
	return cs
}

func (o mp4Fix) trak(handler, codec string, offs []int64, extra []uint32) []byte {
	var co []byte
	if o.co64 {
		co = mp4BE32(0, uint32(len(offs)))
		for _, v := range offs {
			co = binary.BigEndian.AppendUint64(co, uint64(v))
		}
		co = mp4Atom("co64", co)
	} else {
		co = mp4BE32(0, uint32(len(offs)+len(extra)))
		for _, v := range offs {
			co = append(co, mp4BE32(uint32(v))...)
		}
		co = mp4Atom("stco", co, mp4BE32(extra...))
	}
	stbl := mp4Atom("stbl", mp4Atom("stsd", mp4BE32(0, 1), mp4Atom(codec, make([]byte, 28))), mp4Atom("stts", mp4BE32(0, 0)),
		mp4Atom("stsc", mp4BE32(0, 0)), mp4Atom("stsz", mp4BE32(0, 0, 0)), co)
	return mp4Atom("trak", mp4Atom("tkhd", make([]byte, 84)), mp4Atom("mdia", mp4Atom("mdhd", make([]byte, 24)),
		mp4Atom("hdlr", mp4BE32(0, 0), []byte(handler), make([]byte, 13)), mp4Atom("minf", mp4Atom("smhd", make([]byte, 8)),
			mp4Atom("dinf", mp4Atom("dref", mp4BE32(0, 0))), stbl)))
}

func (o mp4Fix) moov(offs []int64) []byte {
	codec := o.codec
	if codec == "" {
		codec = "mp4a"
	}
	n := 3
	parts := [][]byte{mp4Atom("mvhd", make([]byte, 100)), o.trak("soun", codec, offs[:n], o.extra)}
	if o.chapters {
		parts = append(parts, o.trak("text", "text", offs[n:], nil))
	}
	if !o.noUdta {
		var udta [][]byte
		if !o.noMeta {
			meta := [][]byte{mp4Atom("hdlr", mp4BE32(0, 0), []byte("mdirappl"), make([]byte, 9))}
			if !o.qtMeta {
				meta = append([][]byte{mp4BE32(0)}, meta...)
			}
			if !o.noIlst {
				meta = append(meta, mp4Atom("ilst", o.items...))
			}
			if o.metaFree > 0 {
				meta = append(meta, o.free(o.metaFree))
			}
			udta = append(udta, mp4Atom("meta", meta...))
		}
		if !o.noChpl {
			udta = append(udta, mp4Atom("chpl", make([]byte, 9)))
		}
		if o.udtaFree > 0 {
			udta = append(udta, o.free(o.udtaFree))
		}
		parts = append(parts, mp4Atom("udta", udta...))
	}
	return mp4Atom("moov", parts...)
}

// free is a free atom of n bytes in all.
func (o mp4Fix) free(n int) []byte { return mp4Atom("free", bytes.Repeat([]byte{o.freeFill}, n-8)) }

// wideMoov is moov, with a 64-bit size when bigMoov is set.
func (o mp4Fix) wideMoov(offs []int64) []byte {
	m := o.moov(offs)
	if !o.bigMoov {
		return m
	}
	b := append(mp4BE32(1), "moov"...)
	b = binary.BigEndian.AppendUint64(b, uint64(len(m)+8))
	return append(b, m[8:]...)
}

// make writes the file as name in a temp folder.
func (o mp4Fix) make(t *testing.T, name string) string {
	t.Helper()
	ftyp := mp4Atom("ftyp", []byte("M4A "), mp4BE32(0), []byte("M4A mp42isom"))
	cs := o.chunks()
	var mdat []byte
	if o.bigMdat {
		mdat = append(mp4BE32(1), "mdat"...)
		mdat = binary.BigEndian.AppendUint64(mdat, uint64(16+len(bytes.Join(cs, nil))))
	} else {
		mdat = mp4Atom("mdat")
		binary.BigEndian.PutUint32(mdat, uint32(8+len(bytes.Join(cs, nil))))
	}
	offs := make([]int64, len(cs))
	moov := o.wideMoov(offs) // its size, to place the chunks
	at := int64(len(ftyp) + len(mdat))
	if o.moovFirst {
		at += int64(len(moov) + o.topFree)
	}
	for i, c := range cs {
		offs[i] = at
		at += int64(len(c))
	}
	moov = o.wideMoov(offs)
	var top []byte
	if o.topFree > 0 {
		top = o.free(o.topFree)
	}
	var b bytes.Buffer
	b.Write(ftyp)
	if o.moovFirst {
		b.Write(moov)
		b.Write(top)
	}
	b.Write(mdat)
	b.Write(bytes.Join(cs, nil))
	if !o.moovFirst {
		b.Write(moov)
		b.Write(top)
	}
	b.Write(o.after)
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mp4Parse(t *testing.T, p string) (*mp4File, *os.File) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, _ := f.Stat()
	m, err := readMP4(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if m.trakErr != nil {
		t.Fatal(m.trakErr)
	}
	return m, f
}

// offsets are the chunk offsets of every track, in order.
func mp4Offsets(t *testing.T, p string) []int64 {
	t.Helper()
	m, f := mp4Parse(t, p)
	var out []int64
	for _, tb := range m.tables {
		b, err := readAt(f, tb.off, tb.n*tb.width())
		if err != nil {
			t.Fatal(err)
		}
		for ; len(b) > 0; b = b[tb.width():] {
			if tb.wide {
				out = append(out, int64(binary.BigEndian.Uint64(b)))
			} else {
				out = append(out, int64(binary.BigEndian.Uint32(b)))
			}
		}
	}
	return out
}

// checkChunks is whether every chunk offset still points at its chunk.
func mp4CheckChunks(t *testing.T, p string, o mp4Fix) {
	t.Helper()
	b, _ := os.ReadFile(p)
	if err := mp4Chunks(b, o); err != nil {
		t.Fatal(err)
	}
}

// mp4Chunks is why the file b's chunk offsets don't all point at o's chunks, nil when they do.
func mp4Chunks(b []byte, o mp4Fix) error {
	r := bytes.NewReader(b)
	m, err := readMP4(r, int64(len(b)))
	if err != nil {
		return err
	}
	if m.trakErr != nil {
		return m.trakErr
	}
	var offs []int64
	for _, tb := range m.tables {
		x, err := readAt(r, tb.off, tb.n*tb.width())
		if err != nil {
			return err
		}
		for ; len(x) > 0; x = x[tb.width():] {
			if tb.wide {
				offs = append(offs, int64(binary.BigEndian.Uint64(x)))
			} else {
				offs = append(offs, int64(binary.BigEndian.Uint32(x)))
			}
		}
	}
	cs := o.chunks()
	if len(offs) != len(cs) {
		return fmt.Errorf("%d offsets, %d chunks", len(offs), len(cs))
	}
	for i, c := range cs {
		if offs[i]+int64(len(c)) > int64(len(b)) || !bytes.Equal(b[offs[i]:offs[i]+int64(len(c))], c) {
			return fmt.Errorf("chunk %d isn't at %d any more", i, offs[i])
		}
	}
	return nil
}

func mp4Size(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func mp4MustWrite(t *testing.T, p string, c Changes) {
	t.Helper()
	if err := Write(p, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
}

func mp4MustRead(t *testing.T, p string) *Info {
	t.Helper()
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestMP4Read(t *testing.T) {
	u := utf16.Encode([]rune(string(rune(0xfeff)) + "Ålbum Artist")) // after a byte order mark
	ub := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(ub[2*i:], c)
	}
	o := mp4Fix{moovFirst: true, chapters: true, items: [][]byte{
		mp4TextItem("\xa9too", "Lavf61"), mp4TextItem("\xa9nam", "Title"), mp4TextItem("\xa9ART", "A", "B"),
		mp4Atom("trkn", mp4DataAtom(0, []byte{0, 0, 0, 3, 0, 12, 0, 0})), mp4Atom("disk", mp4DataAtom(0, []byte{0, 0, 0, 1, 0, 2})),
		mp4Atom("gnre", mp4DataAtom(0, []byte{0, 18})), mp4Atom("aART", mp4DataAtom(2, ub)), mp4TextItem("\xa9lyr", "One\nTwo"),
		mp4TextItem("\xa9day", "2004-03-22"), mp4SMPB, mp4Atom("covr", mp4DataAtom(13, mp4JPEG), mp4DataAtom(14, []byte("x"))),
	}}
	p := o.make(t, "a.m4a")
	info := mp4MustRead(t, p)
	want := map[Field]string{Title: "Title", Artist: "A / B", Track: "3", TrackTotal: "12", Disc: "1",
		DiscTotal: "2", Genre: "Rock", AlbumArtist: "Ålbum Artist", Lyrics: "One\nTwo", Date: "2004-03-22"}
	for f, v := range want {
		if info.Fields[f] != v {
			t.Errorf("%s = %q, want %q", f, info.Fields[f], v)
		}
	}
	if len(info.Fields) != len(want) || info.Format != "M4A (AAC)" || !info.Writable {
		t.Fatalf("info %#v", info)
	}
	if info.Picture == nil || info.Picture.Width != 60 || info.Picture.Size != len(mp4JPEG) {
		t.Fatalf("picture %#v", info.Picture)
	}
	pic, err := ReadPicture(p)
	if err != nil || !bytes.Equal(pic.Data, mp4JPEG) || pic.MIME != "image/jpeg" {
		t.Fatalf("ReadPicture %v", err)
	}
}

func TestMP4InPlace(t *testing.T) {
	o := mp4Fix{moovFirst: true, chapters: true, metaFree: 512,
		items: [][]byte{mp4TextItem("\xa9nam", "Old"), mp4SMPB, mp4Atom("cpil", mp4DataAtom(21, []byte{1}))}}
	p := o.make(t, "a.m4a")
	before := mp4Size(t, p)
	offs := mp4Offsets(t, p)
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "A longer new title", Album: "Álbum", Track: "7"}})
	if mp4Size(t, p) != before {
		t.Fatalf("size %d → %d: not in place", before, mp4Size(t, p))
	}
	info := mp4MustRead(t, p)
	if info.Fields[Title] != "A longer new title" || info.Fields[Album] != "Álbum" || info.Fields[Track] != "7" {
		t.Fatalf("fields %#v", info.Fields)
	}
	b, _ := os.ReadFile(p)
	if !bytes.Contains(b, mp4SMPB) || !bytes.Contains(b, mp4Atom("cpil", mp4DataAtom(21, []byte{1}))) {
		t.Fatal("other items not kept")
	}
	if got := mp4Offsets(t, p); !mp4Equal(got, offs) {
		t.Fatalf("offsets moved: %v → %v", offs, got)
	}
	mp4CheckChunks(t, p, o)
	// The free atom is smaller now, still right after ilst.
	m, _ := mp4Parse(t, p)
	kids := m.chain[2].kids
	if len(kids) != 3 || kids[1].typ != "ilst" || kids[2].typ != "free" || kids[2].size >= 512 {
		t.Fatalf("meta holds %#v", kids)
	}
	// Shrinking leaves a free atom too, without moving anything.
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "", Album: "", Track: ""}})
	if mp4Size(t, p) != before || len(mp4MustRead(t, p).Fields) != 0 {
		t.Fatal("not shrunk in place")
	}
	mp4CheckChunks(t, p, o)
}

func mp4Equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMP4RewriteShiftsOffsets(t *testing.T) {
	for _, co64 := range []bool{false, true} {
		o := mp4Fix{moovFirst: true, chapters: true, co64: co64, topFree: 8,
			items: [][]byte{mp4TextItem("\xa9nam", "Old"), mp4SMPB}}
		p := o.make(t, "a.m4b")
		before := mp4Size(t, p)
		mp4MustWrite(t, p, Changes{Set: map[Field]string{Artist: "Someone"}, Picture: &Picture{Data: mp4JPEG}})
		grown := mp4Size(t, p) - before
		if grown < int64(len(mp4JPEG)) {
			t.Fatalf("grew %d", grown)
		}
		mp4CheckChunks(t, p, o)
		info := mp4MustRead(t, p)
		if info.Fields[Title] != "Old" || info.Fields[Artist] != "Someone" || info.Picture == nil ||
			info.Format != "M4B (audiobook)" {
			t.Fatalf("info %#v", info)
		}
		if pic, _ := ReadPicture(p); pic == nil || !bytes.Equal(pic.Data, mp4JPEG) {
			t.Fatal("cover not read back")
		}
		// The rewrite left room after ilst: the next edit stays in place.
		mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "New", Comment: "Fine"}, RemovePicture: true})
		if mp4Size(t, p)-before != grown {
			t.Fatal("second edit not in place")
		}
		mp4CheckChunks(t, p, o)
		if info := mp4MustRead(t, p); info.Picture != nil || info.Fields[Comment] != "Fine" {
			t.Fatalf("info %#v", info)
		}
		b, _ := os.ReadFile(p)
		if !bytes.Contains(b, mp4SMPB) || !bytes.Contains(b, mp4Atom("chpl", make([]byte, 9))) {
			t.Fatal("iTunSMPB or chpl lost")
		}
	}
}

func TestMP4MoovLast(t *testing.T) {
	o := mp4Fix{chapters: true, items: [][]byte{mp4TextItem("\xa9nam", "Old")}}
	p := o.make(t, "a.m4a")
	offs := mp4Offsets(t, p)
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: strings.Repeat("Long ", 50)}})
	if got := mp4Offsets(t, p); !mp4Equal(got, offs) {
		t.Fatalf("offsets moved: %v → %v", offs, got)
	}
	mp4CheckChunks(t, p, o)
	if mp4MustRead(t, p).Fields[Title] != strings.TrimSpace(strings.Repeat("Long ", 50)) {
		t.Fatal("title")
	}
}

func TestMP4Create(t *testing.T) {
	for name, o := range map[string]mp4Fix{
		"no udta":         {moovFirst: true, noUdta: true},
		"no meta":         {moovFirst: true, noMeta: true},
		"no ilst":         {moovFirst: true, noIlst: true},
		"no ilst, free":   {noIlst: true, metaFree: 200},
		"quicktime meta":  {moovFirst: true, qtMeta: true, items: [][]byte{mp4TextItem("\xa9nam", "Old")}},
		"no meta, free":   {moovFirst: true, noMeta: true, udtaFree: 300},
		"no udta, free":   {moovFirst: true, noUdta: true, topFree: 400},
		"no udta, co64":   {moovFirst: true, noUdta: true, co64: true, chapters: true},
		"no udta, at end": {noUdta: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := o.make(t, "a.m4a")
			before := mp4Size(t, p)
			mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "Made", Track: "2", DiscTotal: "3"}})
			info := mp4MustRead(t, p)
			if info.Fields[Title] != "Made" || info.Fields[Track] != "2" || info.Fields[DiscTotal] != "3" ||
				info.Fields[Disc] != "" {
				t.Fatalf("fields %#v", info.Fields)
			}
			mp4CheckChunks(t, p, o)
			m, f := mp4Parse(t, p)
			if !m.chain[3].ok {
				t.Fatal("no ilst")
			}
			if h, _ := readAt(f, m.chain[2].kids[0].off, 20); string(h[4:8]) != "hdlr" || string(h[16:20]) != "mdir" {
				t.Fatalf("meta starts %q", h)
			}
			inPlace := o.metaFree+o.udtaFree+o.topFree > 0
			if inPlace != (mp4Size(t, p) == before) {
				t.Fatalf("in place %v, size %d → %d", inPlace, before, mp4Size(t, p))
			}
		})
	}
}

func TestMP4NumbersAndGenre(t *testing.T) {
	o := mp4Fix{moovFirst: true, items: [][]byte{mp4Atom("trkn", mp4DataAtom(0, []byte{0, 0, 0, 3, 0, 12, 0, 0})),
		mp4Atom("gnre", mp4DataAtom(0, []byte{0, 10})), mp4TextItem("\xa9nam", "a"), mp4TextItem("\xa9nam", "b")}}
	p := o.make(t, "a.m4a")
	if info := mp4MustRead(t, p); info.Fields[Genre] != "Metal" || info.Fields[Title] != "a / b" {
		t.Fatalf("fields %#v", info.Fields)
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Track: "4", Genre: "Jazz", Title: "c"}})
	info := mp4MustRead(t, p)
	if info.Fields[Track] != "4" || info.Fields[TrackTotal] != "12" || info.Fields[Genre] != "Jazz" ||
		info.Fields[Title] != "c" {
		t.Fatalf("fields %#v", info.Fields)
	}
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte("gnre")) || bytes.Count(b, []byte("\xa9nam")) != 1 {
		t.Fatal("gnre or the second title kept")
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{TrackTotal: ""}})
	if info := mp4MustRead(t, p); info.Fields[Track] != "4" || info.Fields[TrackTotal] != "" {
		t.Fatalf("fields %#v", info.Fields)
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Track: ""}})
	if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("trkn")) {
		t.Fatal("trkn kept")
	}
	mp4CheckChunks(t, p, o)
}

func TestMP4Formats(t *testing.T) {
	p := mp4Fix{codec: "alac", bigMdat: true, items: [][]byte{mp4TextItem("\xa9nam", "x")}}.make(t, "a.m4a")
	if info := mp4MustRead(t, p); info.Format != "M4A (ALAC)" || info.Fields[Title] != "x" {
		t.Fatalf("info %#v", info)
	}
	if info := mp4MustRead(t, mp4Fix{}.make(t, "a.mp4")); info.Format != "MP4" {
		t.Fatalf("format %q", info.Format)
	}
	// A moov with a 64-bit size keeps it, in place and written again.
	o := mp4Fix{moovFirst: true, bigMoov: true, chapters: true, metaFree: 100,
		items: [][]byte{mp4TextItem("\xa9nam", "x")}}
	p = o.make(t, "w.m4a")
	before := mp4Size(t, p)
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "In place"}})
	if mp4Size(t, p) != before {
		t.Fatal("not in place")
	}
	mp4MustWrite(t, p, Changes{Picture: &Picture{Data: mp4JPEG}})
	if m, _ := mp4Parse(t, p); m.top[1].typ != "moov" || m.top[1].hdr != 16 {
		t.Fatalf("moov %#v", m.top[1])
	}
	if info := mp4MustRead(t, p); info.Fields[Title] != "In place" || info.Picture == nil {
		t.Fatalf("info %#v", info)
	}
	mp4CheckChunks(t, p, o)
	// An mdat of size 0 runs to the end.
	o = mp4Fix{moovFirst: true, items: [][]byte{mp4TextItem("\xa9nam", "x")}}
	p = o.make(t, "b.m4a")
	b, _ := os.ReadFile(p)
	m, _ := mp4Parse(t, p)
	mdat := m.top[len(m.top)-1]
	binary.BigEndian.PutUint32(b[mdat.off:], 0)
	os.WriteFile(p, b, 0o644)
	mp4MustWrite(t, p, Changes{Picture: &Picture{Data: mp4JPEG}})
	mp4CheckChunks(t, p, o)
}

func TestMP4Refusals(t *testing.T) {
	// A 32-bit offset that would pass 4 GB.
	o := mp4Fix{moovFirst: true, extra: []uint32{0xFFFFFF00}}
	p := o.make(t, "a.m4a")
	orig, _ := os.ReadFile(p)
	if err := Write(p, Changes{Picture: &Picture{Data: mp4JPEG}}); err != ErrTooBig {
		t.Fatalf("stco overflow: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, orig) {
		t.Fatal("changed")
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
	// Fragments after moov can't move, but room in place is used.
	o = mp4Fix{moovFirst: true, metaFree: 100, after: mp4Atom("moof", mp4Atom("mfhd", mp4BE32(0, 1)))}
	p = o.make(t, "a.m4a")
	if info := mp4MustRead(t, p); len(info.Notes) != 1 {
		t.Fatalf("notes %v", info.Notes)
	}
	if err := Write(p, Changes{Picture: &Picture{Data: mp4JPEG}}); err != errFragmented {
		t.Fatalf("fragments: %v", err)
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "Fits"}})
	// Not an MP4 at all, and one without moov.
	os.WriteFile(p, []byte("ID3\x04\x00\x00\x00\x00\x00\x00 and more bytes"), 0o644)
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("ID3: %v", err)
	}
	os.WriteFile(p, mp4Atom("ftyp", []byte("M4A "), mp4BE32(0)), 0o644)
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("no moov: %v", err)
	}
}

// Over 2 GB: read, not written. The file is sparse, which other systems' temp folders may not make.
func TestMP4TooBig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a sparse file")
	}
	p := mp4Fix{items: [][]byte{mp4TextItem("\xa9nam", "Big")}}.make(t, "big.m4a")
	if err := os.Truncate(p, 3<<30); err != nil {
		t.Skip("no sparse file:", err)
	}
	if info := mp4MustRead(t, p); info.Writable || info.Fields[Title] != "Big" {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrTooBig {
		t.Fatalf("big: %v", err)
	}
}

// A cover that keeps its place isn't written, however big: a change after it, or one of the same
// size before it, is a few bytes in place. One over mp4MaxPatch that has to move means a rewrite
// (which an ending app can stop); one taken off is zeroed in place. None is held.
func TestMP4BigCover(t *testing.T) {
	big := append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, make([]byte, 9<<20)...)
	covr := mp4Atom("covr", mp4DataAtom(13, big))
	nam, cmt := mp4TextItem("\xa9nam", "Old"), mp4TextItem("\xa9cmt", "Short")
	for _, tc := range []struct {
		name    string
		items   [][]byte
		c       Changes
		inPlace bool
		most    int // the bytes an in-place save writes
	}{
		{"a longer title before it", [][]byte{nam, covr}, Changes{Set: map[Field]string{Title: "A longer title"}}, false, 0},
		{"a longer title after it", [][]byte{covr, nam}, Changes{Set: map[Field]string{Title: "A longer title"}}, true, 1 << 10},
		{"a title of the same length before it", [][]byte{nam, covr}, Changes{Set: map[Field]string{Title: "New"}}, true, 1 << 10},
		{"a longer comment after it", [][]byte{nam, covr, cmt}, Changes{Set: map[Field]string{Comment: "A much longer comment"}}, true, 1 << 10},
		{"an artist added", [][]byte{nam, covr, cmt}, Changes{Set: map[Field]string{Artist: "Someone"}}, true, 1 << 10},
		{"taken off", [][]byte{nam, covr, cmt}, Changes{RemovePicture: true}, true, 10 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := mp4Fix{moovFirst: true, metaFree: 200, items: tc.items}
			p := o.make(t, "a.m4a")
			before, _ := os.Stat(p)
			rec := &mp4Rec{left: -1, count: true}
			alloc := mp4Allocs(func() { rec.run(func() { mp4MustWrite(t, p, tc.c) }) })
			after, _ := os.Stat(p)
			if inPlace := os.SameFile(before, after) && after.Size() == before.Size(); inPlace != tc.inPlace {
				t.Fatalf("in place %v", inPlace)
			}
			if n := rec.wrote(); tc.inPlace && n > tc.most {
				t.Fatalf("%d bytes written in place", n)
			}
			if alloc > 2<<20 {
				t.Fatalf("%d KB allocated", alloc>>10)
			}
			pic, err := ReadPicture(p)
			if tc.c.RemovePicture && (pic != nil || err != nil) || !tc.c.RemovePicture && (err != nil || !bytes.Equal(pic.Data, big)) {
				t.Fatalf("cover %v", err)
			}
			info := mp4MustRead(t, p)
			for f, v := range tc.c.Set {
				if info.Fields[f] != v {
					t.Fatalf("%s = %q", f, info.Fields[f])
				}
			}
			if info.Fields[Title] == "" {
				t.Fatal("no title")
			}
			mp4CheckChunks(t, p, o)
		})
	}
}

// mp4Allocs is how many bytes f allocates.
func mp4Allocs(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// An in-place write is one stretch through one open file: the card can't go between two parts of
// it, leaving ilst's new items under its old size (a file Read and Write refuse).
func TestMP4InPlaceOnePatch(t *testing.T) {
	cover := append(append([]byte(nil), mp4JPEG...), make([]byte, 100<<10)...)
	o := mp4Fix{moovFirst: true, metaFree: 512, items: [][]byte{mp4TextItem("\xa9nam", "Song"),
		mp4TextItem("\xa9too", "Lavf61"), mp4Atom("covr", mp4DataAtom(13, cover)), mp4TextItem("\xa9cmt", "short")}}
	p := o.make(t, "a.m4a")
	before := mp4Size(t, p)
	defer func(f func() bool) { CardReady = f }(CardReady)
	calls := 0
	CardReady = func() bool { calls++; return calls <= 2 } // Write's look, then one write's
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Comment: "a much longer comment than it was"}})
	if mp4Size(t, p) != before {
		t.Fatal("not in place")
	}
	info := mp4MustRead(t, p)
	if info.Fields[Comment] != "a much longer comment than it was" || info.Fields[Title] != "Song" {
		t.Fatalf("fields %#v", info.Fields)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, cover) {
		t.Fatalf("cover %v", err)
	}
	mp4CheckChunks(t, p, o)
}

// In place, a kept cover is copied as the writing goes, never held, nor is the free room zeroed in
// one piece: moving towards the start, staying, or removed.
func TestMP4InPlaceStreams(t *testing.T) {
	cover := append(append([]byte(nil), mp4JPEG...), make([]byte, 3<<20)...)
	covr := mp4Atom("covr", mp4DataAtom(13, cover))
	for name, tc := range map[string]struct {
		items [][]byte
		c     Changes
	}{
		"cover moves up": {[][]byte{mp4TextItem("\xa9nam", "Old"), covr}, Changes{Set: map[Field]string{Title: "A longer title"}}},
		"cover stays":    {[][]byte{covr, mp4TextItem("\xa9nam", "Old")}, Changes{Set: map[Field]string{Title: "A longer title"}}},
		"cover removed":  {[][]byte{mp4TextItem("\xa9nam", "Old"), covr, mp4SMPB}, Changes{RemovePicture: true}},
	} {
		t.Run(name, func(t *testing.T) {
			o := mp4Fix{moovFirst: true, chapters: true, metaFree: 512, items: tc.items}
			p := o.make(t, "a.m4a")
			before := mp4Size(t, p)
			alloc := mp4Allocs(func() { mp4MustWrite(t, p, tc.c) })
			if mp4Size(t, p) != before {
				t.Fatal("not in place")
			}
			if alloc > 1<<20 {
				t.Fatalf("%d KB allocated for a %d KB cover", alloc>>10, len(cover)>>10)
			}
			info := mp4MustRead(t, p)
			if tc.c.RemovePicture {
				if b, _ := os.ReadFile(p); info.Picture != nil || !bytes.Contains(b, mp4SMPB) || bytes.Contains(b, []byte("covr")) {
					t.Fatal("cover not removed, or iTunSMPB lost")
				}
			} else if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, cover) || info.Fields[Title] != "A longer title" {
				t.Fatalf("cover %v, fields %#v", err, info.Fields)
			}
			mp4CheckChunks(t, p, o)
		})
	}
}

// chpl after a meta that would grow into the free room after moov: written in one pass, a save cut
// short would leave neither file, so the first save writes the file again, with room after ilst; the
// next is in place, by the steps that keep it readable throughout.
func TestMP4InPlaceHolds(t *testing.T) {
	o := mp4Fix{moovFirst: true, chapters: true, topFree: 400, items: [][]byte{mp4TextItem("\xa9nam", "Old"), mp4SMPB}}
	p := o.make(t, "a.m4b")
	before := mp4Size(t, p)
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "A much longer title than before", Artist: "Someone"}})
	if mp4Size(t, p) == before {
		t.Fatal("in place, in one pass")
	}
	again := mp4Size(t, p)
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Composer: "Someone Else"}})
	if mp4Size(t, p) != again {
		t.Fatal("the next save not in place")
	}
	info := mp4MustRead(t, p)
	if info.Fields[Title] != "A much longer title than before" || info.Fields[Artist] != "Someone" {
		t.Fatalf("fields %#v", info.Fields)
	}
	b, _ := os.ReadFile(p)
	if !bytes.Contains(b, mp4SMPB) || !bytes.Contains(b, mp4Atom("chpl", make([]byte, 9))) {
		t.Fatal("iTunSMPB or chpl lost")
	}
	if info := mp4MustRead(t, p); info.Fields[Composer] != "Someone Else" || info.Fields[Title] != "A much longer title than before" {
		t.Fatalf("fields %#v", info.Fields)
	}
	mp4CheckChunks(t, p, o)
}

// mp4Deep is a file whose track holds atoms nested depth deep, of the types the walk into a track
// goes through, one 8-byte header a level.
func mp4Deep(t *testing.T, depth int) string {
	t.Helper()
	n := 8 * depth
	b := mp4Atom("ftyp", []byte("M4A "), mp4BE32(0), []byte("M4A mp42isom"))
	b = append(binary.BigEndian.AppendUint32(b, uint32(16+n)), "moov"...)
	b = append(binary.BigEndian.AppendUint32(b, uint32(8+n)), "trak"...)
	for i := 0; i < depth; i++ {
		b = append(binary.BigEndian.AppendUint32(b, uint32(8*(depth-i))), []string{"mdia", "minf", "stbl"}[i%3]...)
	}
	p := filepath.Join(t.TempDir(), "deep.m4a")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A track's walk goes trak, mdia, minf, stbl and no deeper, so atoms nested a great many deep (a
// crafted file) don't grow the stack: past a few hundred thousand levels that killed the app.
func TestMP4DeepTrack(t *testing.T) {
	p := mp4Deep(t, 30000)
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // no collection, so a grown stack stays to be seen
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	grew := make(chan int64)
	var err error
	go func() {
		_, err = Read(p)
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		grew <- int64(after.StackInuse) - int64(before.StackInuse)
	}()
	if n := <-grew; n > 1<<20 {
		t.Fatalf("the stack grew %d KB", n>>10)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A list of more atoms than real files hold isn't read, nor are more top-level atoms or more atoms in
// all than mp4MaxTop and mp4MaxSeen: a few MB of empty atoms made Read allocate 180 MB.
func TestMP4ManyAtoms(t *testing.T) {
	frees := bytes.Repeat([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, 1<<17) // 1 MB of empty atoms
	p := mp4Fix{moovFirst: true, items: [][]byte{mp4TextItem("\xa9nam", "Hi"), frees}}.make(t, "a.m4a")
	var err error
	if alloc := mp4Allocs(func() { _, err = Read(p) }); err != ErrCorrupt || alloc > 2<<20 {
		t.Fatalf("in ilst: %v, %d KB allocated", err, alloc>>10)
	}
	// At the top level, after moov: the rest is kept as it is, and doesn't move.
	p = mp4Fix{moovFirst: true, metaFree: 100, items: [][]byte{mp4TextItem("\xa9nam", "Hi")}, after: frees}.make(t, "b.m4a")
	var info *Info
	if alloc := mp4Allocs(func() { info, err = Read(p) }); err != nil || alloc > 4<<20 {
		t.Fatalf("at the top: %v, %d KB allocated", err, alloc>>10)
	}
	if info.Fields[Title] != "Hi" || len(info.Notes) != 1 {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Picture: &Picture{Data: mp4JPEG}}); err != errFragmented {
		t.Fatalf("a cover that doesn't fit: %v", err)
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Title: "Fits"}})
	if mp4MustRead(t, p).Fields[Title] != "Fits" {
		t.Fatal("title")
	}
	// In all: a track of 3000 atoms of 30 each.
	var minfs []byte
	for i := 0; i < 3000; i++ {
		minfs = append(minfs, mp4Atom("minf", frees[:30*8])...)
	}
	b := append(mp4Atom("ftyp", []byte("M4A "), mp4BE32(0)), mp4Atom("moov", mp4Atom("trak", mp4Atom("mdia", minfs)))...)
	p = filepath.Join(t.TempDir(), "c.m4a")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := readMP4(f, int64(len(b)))
	if err != nil || m.trakErr != ErrCorrupt || m.seen > mp4MaxSeen {
		t.Fatalf("in all: %v, %#v", err, m)
	}
}

// Text is read up to mp4MaxText an item (mp4MaxLyrics for the lyrics), all its values together: 24
// values of 1 MB made a 25 MB field. What's cut can still be cleared.
func TestMP4LongText(t *testing.T) {
	long := []byte(strings.Repeat("la ", 100<<10/3))
	var lyrics [][]byte
	for i := 0; i < 8; i++ {
		lyrics = append(lyrics, mp4DataAtom(1, long))
	}
	o := mp4Fix{moovFirst: true, metaFree: 512, items: [][]byte{mp4Atom("\xa9lyr", lyrics...),
		mp4TextItem("\xa9nam", strings.Repeat("t", 10<<10), strings.Repeat("u", 10<<10)), mp4TextItem("\xa9ART", "Who")}}
	p := o.make(t, "a.m4a")
	var info *Info
	if alloc := mp4Allocs(func() { info = mp4MustRead(t, p) }); alloc > 4<<20 {
		t.Fatalf("%d KB allocated", alloc>>10)
	}
	if n := len(info.Fields[Lyrics]); n < 200<<10 || n > mp4MaxLyrics+8*3 {
		t.Fatalf("lyrics of %d bytes", n)
	}
	if n := len(info.Fields[Title]); n != mp4MaxText+3 || info.Fields[Artist] != "Who" {
		t.Fatalf("title of %d bytes, artist %q", n, info.Fields[Artist])
	}
	mp4MustWrite(t, p, Changes{Set: map[Field]string{Lyrics: ""}})
	if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("\xa9lyr")) || mp4MustRead(t, p).Fields[Artist] != "Who" {
		t.Fatal("lyrics not cleared")
	}
	mp4CheckChunks(t, p, o)
}

// mp4Rec stands in for the song opened for an in-place save: it makes each write and keeps a copy
// (unless count is set: then only the bytes are counted), and notes the syncs. Once left writes are
// made it makes no more, as if the app had died there; left < 0 lets them all through.
type mp4Rec struct {
	f      *os.File
	left   int
	count  bool
	n      int
	writes []mp4Wrote
	syncs  []int // how many writes came before each sync
}

type mp4Wrote struct {
	off int64
	b   []byte
}

var errCut = errors.New("cut short")

func (r *mp4Rec) WriteAt(b []byte, off int64) (int, error) {
	if r.left == 0 {
		return 0, errCut
	}
	r.left--
	r.n += len(b)
	if !r.count {
		r.writes = append(r.writes, mp4Wrote{off, bytes.Clone(b)})
	}
	return r.f.WriteAt(b, off)
}

func (r *mp4Rec) Sync() error {
	r.syncs = append(r.syncs, len(r.writes))
	return r.f.Sync()
}

func (r *mp4Rec) Close() error { return r.f.Close() }

// run runs f with the in-place saves going through r.
func (r *mp4Rec) run(f func()) {
	defer func(open func(string) (mp4Out, error)) { mp4Open = open }(mp4Open)
	mp4Open = func(p string) (mp4Out, error) {
		var err error
		r.f, err = os.OpenFile(p, os.O_WRONLY, 0)
		return r, err
	}
	f()
}

// wrote is how many bytes r wrote.
func (r *mp4Rec) wrote() int { return r.n }

// cuts calls f with each file the save r saw can leave, made from orig, when it is cut short: after
// each write, as when the app dies; and, as the card may keep some of the writes since the last sync
// and not others when the power goes, with each step's writes kept in part (every way for a few, else
// each alone and all but each), every step before it whole. The last call is the whole save.
func (r *mp4Rec) cuts(orig []byte, f func(name string, b []byte)) {
	b := make([]byte, len(orig))
	state := func(name string, ws ...[]mp4Wrote) {
		copy(b, orig)
		for _, w := range ws {
			for _, x := range w {
				copy(b[x.off:], x.b)
			}
		}
		f(name, b)
	}
	for k := range r.writes {
		state(fmt.Sprintf("after %d of %d writes", k, len(r.writes)), r.writes[:k])
	}
	from := 0
	for s, to := range r.syncs {
		step := r.writes[from:to]
		var picks [][]bool
		if n := len(step); n <= 6 {
			for set := 0; set < 1<<n; set++ {
				pick := make([]bool, n)
				for i := range pick {
					pick[i] = set>>i&1 == 1
				}
				picks = append(picks, pick)
			}
		} else {
			for i := 0; i < n; i++ {
				one, all := make([]bool, n), make([]bool, n)
				for j := range all {
					all[j] = j != i
				}
				one[i] = true
				picks = append(picks, one, all)
			}
		}
		for _, pick := range picks {
			var part []mp4Wrote
			for i, w := range step {
				if pick[i] {
					part = append(part, w)
				}
			}
			state(fmt.Sprintf("step %d of %d with writes %v of it", s+1, len(r.syncs), pick), r.writes[:from], part)
		}
		from = to
	}
	state("the whole save", r.writes)
}

// mp4Look is what the file b reads as: its fields, and its cover's length (0 without one).
func mp4Look(b []byte) (map[Field]string, int64, error) {
	info, pic, err := mp4Codec{}.read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, 0, err
	}
	if pic == nil {
		return info.Fields, 0, nil
	}
	return info.Fields, pic.Len, nil
}

func mp4SameFields(a, b map[Field]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// mp4CutItems is an ilst like the one ffmpeg writes with a cover (ours adds the free room after it):
// a title, the encoder, a cover of 150 KB, which takes three writes to move, and a comment after it.
func mp4CutItems() [][]byte {
	cover := append(append([]byte(nil), mp4JPEG...), bytes.Repeat([]byte{9}, 150<<10)...)
	return [][]byte{mp4TextItem("\xa9nam", "Song Two"), mp4TextItem("\xa9too", "Lavf61.7.100"),
		mp4Atom("covr", mp4DataAtom(13, cover)), mp4TextItem("\xa9cmt", "short")}
}

// What a save cut short leaves reads, every value the old one or the new (none torn); with cutKeeps,
// nothing the old tags and the new both have is missing; with cutWhole, it reads as the old tags or
// the new.
const (
	cutReads = iota
	cutKeeps
	cutWhole
)

// mp4CutChanges are edits of mp4CutItems, and what a save of each cut short leaves.
var mp4CutChanges = []struct {
	name string
	c    Changes
	cut  int
}{
	{"artist added", Changes{Set: map[Field]string{Artist: "Someone"}}, cutWhole},
	{"title of the same length", Changes{Set: map[Field]string{Title: "Sung Two"}}, cutWhole},
	{"track, genre and lyrics added", Changes{Set: map[Field]string{Track: "4", TrackTotal: "12", Genre: "Jazz",
		Lyrics: "line one\nline two"}}, cutWhole},
	{"comment taken off", Changes{Set: map[Field]string{Comment: ""}}, cutWhole},
	{"album added, title of the same length", Changes{Set: map[Field]string{Album: "An Album", Title: "Sung Two"}}, cutKeeps},
	{"longer comment", Changes{Set: map[Field]string{Comment: "a much longer comment than it was"}}, cutReads},
	{"shorter comment", Changes{Set: map[Field]string{Comment: "x"}}, cutReads},
	{"longer title", Changes{Set: map[Field]string{Title: "A Rather Longer Song Title"}}, cutReads},
	{"cover taken off", Changes{RemovePicture: true}, cutReads},
	{"another cover", Changes{Picture: &Picture{Data: append(append([]byte(nil), mp4JPEG...), make([]byte, 120<<10)...)}}, cutReads},
}

// A save in place cut short anywhere, by the app dying between two writes or the power going between
// two syncs, leaves a file that reads, every chunk where it was, and no value or cover torn. An
// addition, a value of the same length, or items taken off the end, reads as the old tags or the new
// ones; both at once, with each value the old or the new. Another edit reads as either, or without the
// items from the first one it changes on (the edited ones, and those it moves) until it ends. It was
// one stretch, which a death part way left unreadable.
func TestMP4CutShort(t *testing.T) {
	items := mp4CutItems()
	for _, fx := range []struct {
		name string
		o    mp4Fix
	}{
		{"free room after ilst", mp4Fix{moovFirst: true, chapters: true, metaFree: 4096, items: items}},
		{"free room after ilst, not zeroed", mp4Fix{chapters: true, metaFree: 4096, freeFill: 0xEE, items: items}},
		{"free room after meta", mp4Fix{moovFirst: true, noChpl: true, udtaFree: 4096, freeFill: 0xEE, items: items}},
		{"free room after moov", mp4Fix{moovFirst: true, noChpl: true, topFree: 4096, items: items}},
	} {
		for _, tc := range mp4CutChanges {
			t.Run(fx.name+", "+tc.name, func(t *testing.T) { mp4CheckCuts(t, fx.o, tc.c, tc.cut) })
		}
	}
	// A cover of 300 KB, in several writes, added where the free room holds old bytes.
	o := mp4Fix{moovFirst: true, chapters: true, metaFree: 400 << 10, freeFill: 0xEE, items: [][]byte{
		mp4TextItem("\xa9nam", "Song Two"), mp4TextItem("\xa9too", "Lavf61.7.100"), mp4TextItem("\xa9cmt", "short")}}
	cover := append(append([]byte(nil), mp4JPEG...), bytes.Repeat([]byte{3}, 300<<10)...)
	t.Run("a cover added", func(t *testing.T) { mp4CheckCuts(t, o, Changes{Picture: &Picture{Data: cover}}, cutWhole) })
}

// mp4CheckCuts makes c to a song made as o, in place, and checks every file a cut in that save can
// leave reads, as cut says.
func mp4CheckCuts(t *testing.T, o mp4Fix, c Changes, cut int) {
	t.Helper()
	p := o.make(t, "a.m4a")
	orig, _ := os.ReadFile(p)
	oldF, oldPic, err := mp4Look(orig)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	rec := &mp4Rec{left: -1}
	rec.run(func() { mp4MustWrite(t, p, c) })
	if after, _ := os.Stat(p); !os.SameFile(before, after) || after.Size() != before.Size() {
		t.Fatal("not in place")
	}
	saved, _ := os.ReadFile(p)
	newF, newPic, err := mp4Look(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.syncs) == 0 || rec.syncs[len(rec.syncs)-1] != len(rec.writes) {
		t.Fatalf("not synced at the end: syncs %v, %d writes", rec.syncs, len(rec.writes))
	}
	n := 0
	rec.cuts(orig, func(name string, b []byte) {
		n++
		f, pic, err := mp4Look(b)
		if err == nil {
			err = mp4Chunks(b, o)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for k, v := range f {
			if v != oldF[k] && v != newF[k] {
				t.Fatalf("%s: %s = %q", name, k, v)
			}
		}
		if pic != 0 && pic != oldPic && pic != newPic {
			t.Fatalf("%s: a cover of %d bytes", name, pic)
		}
		for k := range oldF {
			if cut >= cutKeeps && newF[k] != "" && f[k] == "" {
				t.Fatalf("%s: no %s", name, k)
			}
		}
		if cut >= cutKeeps && oldPic != 0 && newPic != 0 && pic == 0 {
			t.Fatalf("%s: no cover", name)
		}
		old, now := mp4SameFields(f, oldF) && pic == oldPic, mp4SameFields(f, newF) && pic == newPic
		if cut == cutWhole && !old && !now {
			t.Fatalf("%s: neither the old tags nor the new: %q, cover %d", name, f, pic)
		}
		if name == "the whole save" && !bytes.Equal(b, saved) {
			t.Fatal("the writes made aren't the save")
		}
	})
	t.Logf("%d writes, %d syncs, %d bytes; %d files a cut leaves read", len(rec.writes), len(rec.syncs), rec.wrote(), n)
}

// A save that stops after any number of writes (the writing fails, as when the card goes) leaves a
// song that reads, and that the next save changes.
func TestMP4CutShortThenSaved(t *testing.T) {
	o := mp4Fix{moovFirst: true, chapters: true, metaFree: 4096, items: mp4CutItems()}
	for _, tc := range mp4CutChanges {
		t.Run(tc.name, func(t *testing.T) {
			p := o.make(t, "a.m4a")
			orig, _ := os.ReadFile(p)
			all := &mp4Rec{left: -1}
			all.run(func() { mp4MustWrite(t, p, tc.c) })
			for k := 0; k < len(all.writes); k++ {
				if err := os.WriteFile(p, orig, 0o644); err != nil {
					t.Fatal(err)
				}
				var err error
				(&mp4Rec{left: k}).run(func() { err = Write(p, tc.c) })
				if !errors.Is(err, errCut) {
					t.Fatalf("after %d writes: %v", k, err)
				}
				if _, err := Read(p); err != nil {
					t.Fatalf("after %d writes: %v", k, err)
				}
				mp4MustWrite(t, p, Changes{Set: map[Field]string{Comment: "saved again"}})
				if info := mp4MustRead(t, p); info.Fields[Comment] != "saved again" {
					t.Fatalf("after %d writes, saved again: %#v", k, info.Fields)
				}
				mp4CheckChunks(t, p, o)
			}
		})
	}
}

// In place, only what changes is written: an edit after a cover, an addition or a value of the same
// length is a few hundred bytes, where it was the whole 265 KB cover a save, in a few steps.
func TestMP4InPlaceWritesLittle(t *testing.T) {
	cover := append(append([]byte(nil), mp4JPEG...), bytes.Repeat([]byte{9}, 265<<10)...)
	o := mp4Fix{moovFirst: true, chapters: true, metaFree: 4096, items: [][]byte{mp4TextItem("\xa9nam", "Song Two"),
		mp4TextItem("\xa9too", "Lavf61.7.100"), mp4Atom("covr", mp4DataAtom(13, cover)), mp4TextItem("\xa9cmt", "short")}}
	p := o.make(t, "a.m4a")
	before, _ := os.Stat(p)
	for _, tc := range []struct {
		set          map[Field]string
		bytes, syncs int
	}{
		{map[Field]string{Comment: "x"}, 100, 5},
		{map[Field]string{Comment: "a much longer comment than it was"}, 200, 5},
		{map[Field]string{Title: "Sung Two"}, 8, 1},
		{map[Field]string{Track: "4", TrackTotal: "12", Genre: "Jazz", Lyrics: "line one\nline two"}, 200, 3},
		{map[Field]string{Artist: "Someone"}, 100, 3},
		{map[Field]string{Artist: "No One!"}, 7, 1},
	} {
		rec := &mp4Rec{left: -1}
		rec.run(func() { mp4MustWrite(t, p, Changes{Set: tc.set}) })
		if rec.wrote() > tc.bytes || len(rec.syncs) > tc.syncs {
			t.Errorf("%v: %d bytes in %d writes, %d syncs", tc.set, rec.wrote(), len(rec.writes), len(rec.syncs))
		}
		if after, _ := os.Stat(p); !os.SameFile(before, after) || after.Size() != before.Size() {
			t.Fatalf("%v: not in place", tc.set)
		}
		info := mp4MustRead(t, p)
		for f, v := range tc.set {
			if info.Fields[f] != v {
				t.Fatalf("%v: %s = %q", tc.set, f, info.Fields[f])
			}
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, cover) {
			t.Fatalf("%v: cover %v", tc.set, err)
		}
	}
	mp4CheckChunks(t, p, o)
}

// An in-place save doesn't start once File Browser is turning off: the song stays as it was.
func TestMP4InPlaceStopping(t *testing.T) {
	p := mp4Fix{moovFirst: true, metaFree: 512, items: [][]byte{mp4TextItem("\xa9nam", "Old")}}.make(t, "a.m4a")
	orig, _ := os.ReadFile(p)
	defer func(f func() bool) { Stopping = f }(Stopping)
	Stopping = func() bool { return true }
	if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != ErrStopped {
		t.Fatalf("stopping: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, orig) {
		t.Fatal("changed")
	}
}
