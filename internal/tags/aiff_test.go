package tags

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// iffAllocs is how many bytes f allocates.
func iffAllocs(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// iffChunkOf is a chunk as a file has it: its id, size and body, and the pad byte an odd one takes.
func iffChunkOf(big bool, id string, body []byte) []byte {
	h := make([]byte, 8)
	copy(h, id)
	if big {
		binary.BigEndian.PutUint32(h[4:], uint32(len(body)))
	} else {
		binary.LittleEndian.PutUint32(h[4:], uint32(len(body)))
	}
	b := append(h, body...)
	if len(body)&1 != 0 {
		b = append(b, 0)
	}
	return b
}

// iffMake is an AIFF (form "AIFF" or "AIFC") or a WAV ("WAVE") of the chunks, its size counting them.
func iffMake(form string, chunks ...[]byte) []byte {
	body := bytes.Join(chunks, nil)
	h := make([]byte, 12)
	copy(h[8:], form)
	if form == "WAVE" {
		copy(h, "RIFF")
		binary.LittleEndian.PutUint32(h[4:], uint32(4+len(body)))
	} else {
		copy(h, "FORM")
		binary.BigEndian.PutUint32(h[4:], uint32(4+len(body)))
	}
	return append(h, body...)
}

// iffAudio is n bytes standing in for samples.
func iffAudio(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + i>>9)
	}
	return b
}

func aiffComm() []byte {
	return []byte{0, 2, 0, 0, 0x10, 0, 0, 16, 0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0}
}

func iffTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// iffOpen is p's bytes and layout, which must be sound: its chunks fill it as its header says.
func iffOpen(t *testing.T, p string) (*iffFile, []byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ff, err := readIFF(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if ff.odd != "" || ff.u32(b[4:8]) != ff.end-8 {
		t.Fatalf("layout: %q, size field %d, chunks end at %d", ff.odd, ff.u32(b[4:8]), ff.end)
	}
	return ff, b
}

// iffBody is the body of the first chunk with id.
func iffBody(ff *iffFile, b []byte, id string) []byte {
	for _, c := range ff.chunks {
		if c.id == id {
			return b[c.off+8 : c.off+8+c.size]
		}
	}
	return nil
}

func iffIDs(ff *iffFile) string {
	var out []string
	for _, c := range ff.chunks {
		out = append(out, c.id)
	}
	return strings.Join(out, ",")
}

// iffTagOf is the ID3 tag in p's ID3 chunk.
func iffTagOf(t *testing.T, ff *iffFile, b []byte) *id3Tag {
	t.Helper()
	if ff.id3 == nil {
		t.Fatal("no ID3 chunk")
	}
	return mustParse(t, b[ff.id3.off+8:ff.id3.off+8+ff.id3.size])
}

func TestAIFFInPlace(t *testing.T) {
	gain := rawFrame(4, "TXXX", txxxBody("REPLAYGAIN_TRACK_GAIN", "-6.20 dB"))
	tag := rawTag(4, 0, 301, rawFrame(4, "TIT2", utf8z("Old")), gain, rawFrame(4, "TRCK", utf8z("3/9")))
	if len(tag)&1 == 0 {
		tag = rawTag(4, 0, 302, rawFrame(4, "TIT2", utf8z("Old")), gain, rawFrame(4, "TRCK", utf8z("3/9")))
	}
	audio := iffAudio(5001)
	p := iffTemp(t, "a.aiff", iffMake("AIFF", iffChunkOf(true, "COMM", aiffComm()),
		iffChunkOf(true, "SSND", append(make([]byte, 8), audio...)), iffChunkOf(true, "ID3 ", tag)))
	before := size(t, p)
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "AIFF (ID3v2.4)" || !info.Writable || info.Fields[Title] != "Old" || info.Fields[TrackTotal] != "9" {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "New Títle", Artist: "Ärtist ★"}}); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != before {
		t.Fatalf("size %d → %d: not in place", before, size(t, p))
	}
	ff, b := iffOpen(t, p)
	if iffIDs(ff) != "COMM,SSND,ID3 " || ff.id3.size != int64(len(tag)) || ff.id3.next != ff.id3.off+8+ff.id3.size+1 {
		t.Fatalf("chunks %s, ID3 chunk %+v", iffIDs(ff), ff.id3)
	}
	if !bytes.Equal(iffBody(ff, b, "SSND")[8:], audio) {
		t.Fatal("audio changed")
	}
	tg := iffTagOf(t, ff, b)
	if ids(tg) != "TIT2 TXXX TRCK TPE1" || !bytes.Equal(tg.frames[1].data, gain[10:]) {
		t.Fatalf("frames %s", ids(tg))
	}
	info, _ = Read(p)
	if info.Fields[Title] != "New Títle" || info.Fields[Artist] != "Ärtist ★" || info.Fields[Track] != "3" {
		t.Fatalf("fields %#v", info.Fields)
	}
}

func TestAIFFRewriteBeforeAudio(t *testing.T) {
	tag := rawTag(3, 0, 0, rawFrame(3, "TIT2", lat("Old")), rawFrame(3, "TPE1", lat("Someone")))
	audio := iffAudio(8000)
	name := iffChunkOf(true, "NAME", []byte("Old"))
	p := iffTemp(t, "b.aif", iffMake("AIFF", name, iffChunkOf(true, "COMM", aiffComm()),
		iffChunkOf(true, "ID3 ", tag), iffChunkOf(true, "SSND", append(make([]byte, 8), audio...))))
	info, _ := Read(p)
	if len(info.Notes) != 1 || !strings.HasPrefix(info.Notes[0], "It has an AIFF NAME chunk too") {
		t.Fatalf("notes %q", info.Notes)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "A longer title"}, Picture: &Picture{Data: testJPEG}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
	ff, b := iffOpen(t, p)
	if iffIDs(ff) != "NAME,COMM,ID3 ,SSND" || !bytes.Equal(b[12:12+len(name)], name) {
		t.Fatalf("chunks %s", iffIDs(ff))
	}
	if !bytes.Equal(iffBody(ff, b, "SSND")[8:], audio) {
		t.Fatal("audio changed")
	}
	tg := iffTagOf(t, ff, b)
	if tg.major != 3 || ff.id3.size != tg.needed()+iffSpare && ff.id3.size != tg.needed()+iffSpare+1 || ff.id3.size&1 != 0 {
		t.Fatalf("v2.%d, chunk %d for %d", tg.major, ff.id3.size, tg.needed())
	}
	info, _ = Read(p)
	if info.Format != "AIFF (ID3v2.3)" || info.Fields[Title] != "A longer title" || info.Fields[Artist] != "Someone" ||
		info.Picture == nil || info.Picture.Width != 60 {
		t.Fatalf("info %#v %#v", info, info.Picture)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, testJPEG) {
		t.Fatalf("picture %v", err)
	}
	// Now there is room: the next edit is in place.
	n := size(t, p)
	if err := Write(p, Changes{Set: map[Field]string{Album: "Record"}}); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != n {
		t.Fatal("not in place")
	}
}

// aiffNoTag is an AIFF-C with text chunks and an odd last chunk without its pad byte; its FORM size
// counts the missing byte when counted.
func aiffNoTag(t *testing.T, counted bool) (string, []byte) {
	audio := iffAudio(4001)
	b := iffMake("AIFC", iffChunkOf(true, "FVER", []byte{0xA2, 0x80, 0x51, 0x40}),
		iffChunkOf(true, "COMM", aiffComm()), iffChunkOf(true, "NAME", []byte("Name Here")),
		iffChunkOf(true, "AUTH", []byte("Auth\x00")), iffChunkOf(true, "ANNO", []byte("A note")),
		iffChunkOf(true, "ANNO", []byte("Another")), iffChunkOf(true, "SSND", append(make([]byte, 8), audio...)))
	b = b[:len(b)-1]
	if !counted {
		binary.BigEndian.PutUint32(b[4:], uint32(len(b)-8))
	}
	return iffTemp(t, "c.aifc", b), audio
}

func TestAIFFNoTag(t *testing.T) {
	for _, counted := range []bool{false, true} {
		p, audio := aiffNoTag(t, counted)
		info, err := Read(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Format != "AIFF-C (text chunks)" || !info.Writable || info.Fields[Title] != "Name Here" ||
			info.Fields[Artist] != "Auth" || info.Fields[Comment] != "A note" || len(info.Notes) != 1 ||
			!strings.HasPrefix(info.Notes[0], "It has AIFF NAME, AUTH and ANNO chunks only") {
			t.Fatalf("info %#v", info)
		}
		old, _ := os.ReadFile(p)
		if err := Write(p, Changes{Set: map[Field]string{Genre: "Jazz", Comment: ""}}); err != nil {
			t.Fatal(err)
		}
		ff, b := iffOpen(t, p)
		if iffIDs(ff) != "FVER,COMM,NAME,AUTH,ANNO,ANNO,SSND,ID3 " || ff.id3.off&1 != 0 ||
			int64(len(b)) != ff.id3.next || ff.end != int64(len(b)) {
			t.Fatalf("chunks %s at %+v, %d bytes", iffIDs(ff), ff.id3, len(b))
		}
		if !bytes.Equal(b[12:ff.id3.off-1], old[12:]) || b[ff.id3.off-1] != 0 {
			t.Fatal("the chunks before the new one changed, or no pad byte")
		}
		if !bytes.Equal(iffBody(ff, b, "SSND")[8:], audio) {
			t.Fatal("audio changed")
		}
		info, _ = Read(p)
		if info.Format != "AIFF-C (ID3v2.4)" || info.Fields[Title] != "Name Here" || info.Fields[Artist] != "Auth" ||
			info.Fields[Genre] != "Jazz" || info.Fields[Comment] != "" {
			t.Fatalf("info %#v", info)
		}
	}
}

func TestIFFRefuses(t *testing.T) {
	ssnd := iffChunkOf(true, "SSND", append(make([]byte, 8), iffAudio(300)...))
	comm := iffChunkOf(true, "COMM", aiffComm())
	long := iffMake("AIFF", comm, ssnd)
	binary.BigEndian.PutUint32(long[4:], uint32(len(long)+92))
	junk := append(iffMake("AIFF", comm, ssnd), 1, 2, 3, 4, 5)
	binary.BigEndian.PutUint32(junk[4:], uint32(len(junk)-8))
	for name, b := range map[string][]byte{
		"cut short":   long,
		"junk":        junk,
		"bad ID3":     iffMake("AIFF", comm, ssnd, iffChunkOf(true, "ID3 ", []byte("ID3\x04\x00\x00\x00\x00\x7f\x7f"))),
		"bad version": iffMake("AIFF", comm, ssnd, iffChunkOf(true, "ID3 ", rawTag(5, 0, 10))),
		"ID3 but odd": iffMake("AIFF", comm, iffChunkOf(true, "ID3 ", rawTag(3, 0, 10, rawFrame(3, "TIT2", lat("x")))), ssnd[:100]),
	} {
		p := iffTemp(t, "x.aiff", b)
		info, err := Read(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Writable || info.Format != "AIFF" && name != "ID3 but odd" || len(info.Notes) == 0 {
			t.Fatalf("%s: %#v", name, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrReadOnly {
			t.Fatalf("%s: write %v", name, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
			t.Fatalf("%s: changed", name)
		}
	}
	for _, b := range [][]byte{[]byte("FORM\x00\x00\x00\x04XXXX"), []byte("FORM\x00\x00"), []byte("FORM\x00\x00\x00\x02AIFF")} {
		if _, err := Read(iffTemp(t, "y.aiff", b)); err != ErrCorrupt {
			t.Fatalf("%q: %v", b, err)
		}
	}
}

// A file cut short anywhere reads without a panic, and is never one to write.
func TestIFFCutShort(t *testing.T) {
	tag := rawTag(3, 0, 20, rawFrame(3, "TIT2", lat("x")), rawFrame(3, "APIC", apicBody("image/jpeg", 3, "", testJPEG)))
	for _, b := range [][]byte{
		iffMake("AIFF", iffChunkOf(true, "COMM", aiffComm()), iffChunkOf(true, "SSND", iffAudio(301)), iffChunkOf(true, "ID3 ", tag)),
		iffMake("WAVE", wavFmt(), wavList("INAM", "Name"), iffChunkOf(false, "data", iffAudio(301)), iffChunkOf(false, "id3 ", tag)),
	} {
		for n := 0; n < len(b)-1; n += 3 {
			info, _, err := iffCodec{}.read(bytes.NewReader(b[:n]), int64(n))
			if err == nil && info.Writable {
				t.Fatalf("%.4s cut to %d of %d: writable", b, n, len(b))
			}
		}
	}
}

func TestAIFFWAVInside(t *testing.T) {
	data := iffChunkOf(false, "data", iffAudio(400))
	p := iffTemp(t, "w.aiff", iffMake("WAVE", iffChunkOf(false, "fmt ", make([]byte, 16)), data))
	info, err := Read(p)
	if err != nil || info.Format != "WAV (no tag yet)" || !info.Writable {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != nil {
		t.Fatal(err)
	}
	if ff, _ := iffOpen(t, p); iffIDs(ff) != "fmt ,data,id3 " {
		t.Fatalf("chunks %s", iffIDs(ff))
	}
}

func TestAIFFBigPicture(t *testing.T) {
	big := append(append([]byte(nil), testJPEG...), bytes.Repeat([]byte{0x5A}, 1500<<10)...)
	tag := rawTag(4, 0, 100, rawFrame(4, "TIT2", utf8z("Old")), rawFrame(4, "APIC", apicBody("image/jpeg", 3, "", big)))
	audio := iffAudio(3000)
	p := iffTemp(t, "big.aiff", iffMake("AIFF", iffChunkOf(true, "COMM", aiffComm()),
		iffChunkOf(true, "SSND", append(make([]byte, 8), audio...)), iffChunkOf(true, "ID3 ", tag)))
	n := size(t, p)
	f, _ := os.Open(p)
	ff, err := readIFF(f, n)
	f.Close()
	if err != nil || len(ff.tag.frames) != 2 || !ff.tag.frames[1].lazy {
		t.Fatalf("the picture was read: %v", err)
	}
	info, _ := Read(p)
	if info.Picture == nil || info.Picture.Size != len(big) {
		t.Fatalf("picture %#v", info.Picture)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != nil {
		t.Fatal(err)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, big) || size(t, p) != n {
		t.Fatalf("picture lost, or not in place: %v", err)
	}
	// Removing it leaves over 1 MB of zeros, written in pieces.
	if err := Write(p, Changes{RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	ff, b := iffOpen(t, p)
	tg := iffTagOf(t, ff, b)
	if size(t, p) != n || ids(tg) != "TIT2" || !bytes.Equal(iffBody(ff, b, "SSND")[8:], audio) {
		t.Fatalf("size %d → %d, frames %s", n, size(t, p), ids(tg))
	}
	if id3Len(b[ff.id3.off+8:]) != int64(len(tag)) || bytes.Count(iffBody(ff, b, "ID3 ")[tg.needed():], []byte{0}) != len(tag)-int(tg.needed()) {
		t.Fatal("the room isn't zeros, or the tag doesn't fill it")
	}
	if info, _ := Read(p); info.Picture != nil || info.Fields[Title] != "New" {
		t.Fatalf("info %#v", info)
	}
}

// A file of very many chunks costs a bounded walk, and isn't written: 64K empty chunks once took
// megabytes and seconds (a million, past the Q2's 32 MB).
func TestIFFManyChunks(t *testing.T) {
	chunks := [][]byte{wavFmt(), bytes.Repeat([]byte("junk\x00\x00\x00\x00"), 1<<16), iffChunkOf(false, "data", iffAudio(100))}
	b := iffMake("WAVE", chunks...)
	p := iffTemp(t, "many.wav", b)
	var info *Info
	var err error
	if n := iffAllocs(func() { info, err = Read(p) }); n > 1<<20 {
		t.Fatalf("%d KB allocated", n>>10)
	}
	if err != nil || info.Writable || len(info.Notes) != 1 || !strings.Contains(info.Notes[0], "too many chunks") {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrReadOnly {
		t.Fatalf("write %v", err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
		t.Fatal("changed")
	}
	// As many as a real file might have are fine (TestIFFChunkCapEdge has the cap's edge).
	b = iffMake("WAVE", wavFmt(), bytes.Repeat([]byte("junk\x00\x00\x00\x00"), iffMaxChunks-3), iffChunkOf(false, "data", iffAudio(100)))
	if info, err := Read(iffTemp(t, "some.wav", b)); err != nil || !info.Writable {
		t.Fatalf("%v %#v", err, info)
	}
}

// iffOfChunks is a WAV or an AIFF of k chunks: its format chunk, empty "junk" ones, the audio, and an
// ID3 chunk holding tag when there's one; for a WAV, a LIST INFO too when info is set.
func iffOfChunks(kind string, k int, tag []byte, info bool) []byte {
	big := kind == "aiff"
	chunks := [][]byte{wavFmt()}
	audio, form, id := iffChunkOf(false, "data", iffAudio(100)), "WAVE", "id3 "
	if big {
		chunks = [][]byte{iffChunkOf(true, "COMM", aiffComm())}
		audio, form, id = iffChunkOf(true, "SSND", append(make([]byte, 8), iffAudio(100)...)), "AIFF", "ID3 "
	}
	if info && !big {
		chunks = append(chunks, wavList("INAM", "Info Title"))
	}
	empty := k - len(chunks) - 1
	if tag != nil {
		empty--
	}
	chunks = append(chunks, bytes.Repeat([]byte("junk\x00\x00\x00\x00"), empty), audio)
	if tag != nil {
		chunks = append(chunks, iffChunkOf(big, id, tag))
	}
	return iffMake(form, chunks...)
}

// A save never makes a file of more chunks than a read takes: one of iffMaxChunks without an ID3 chunk
// is read-only (a save would add the chunk past the most read, and the next read wouldn't see the tag it
// wrote), one fewer saves to iffMaxChunks, and one with an ID3 chunk keeps its count, in place or written
// again.
func TestIFFChunkCapEdge(t *testing.T) {
	for _, kind := range []string{"wav", "aiff"} {
		// One short of the cap: the save adds the ID3 chunk, the last one a read takes.
		p := iffTemp(t, "e."+kind, iffOfChunks(kind, iffMaxChunks-1, nil, false))
		if err := Write(p, Changes{Set: map[Field]string{Title: "Saved"}}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if ff, _ := iffOpen(t, p); len(ff.chunks) != iffMaxChunks || ff.id3 == nil {
			t.Fatalf("%s: %d chunks", kind, len(ff.chunks))
		}
		if info, err := Read(p); err != nil || !info.Writable || info.Fields[Title] != "Saved" {
			t.Fatalf("%s: after %v %#v", kind, err, info)
		}

		// At the cap with no ID3 chunk: read-only up front, the older text still shown.
		b := iffOfChunks(kind, iffMaxChunks, nil, true)
		p = iffTemp(t, "f."+kind, b)
		info, err := Read(p)
		if err != nil || info.Writable || len(info.Notes) != 1 || info.Notes[0] != iffTooMany {
			t.Fatalf("%s: %v %#v", kind, err, info)
		}
		if kind == "wav" && info.Fields[Title] != "Info Title" {
			t.Fatalf("%s: fields %v", kind, info.Fields)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "Saved"}}); err != ErrReadOnly {
			t.Fatalf("%s: write %v", kind, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
			t.Fatalf("%s: changed", kind)
		}

		// At the cap with an ID3 chunk: a save replaces it where it was.
		b = iffOfChunks(kind, iffMaxChunks, rawTag(4, 0, 200, rawFrame(4, "TIT2", utf8z("Old"))), false)
		p = iffTemp(t, "g."+kind, b)
		before, _ := iffOpen(t, p)
		at := before.id3.off
		for i, c := range []Changes{
			{Set: map[Field]string{Title: "In Place"}},
			{Set: map[Field]string{Title: "Written Again", Lyrics: strings.Repeat("la ", 1000)}},
		} {
			if err := Write(p, c); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			ff, after := iffOpen(t, p)
			if len(ff.chunks) != iffMaxChunks || ff.id3 != &ff.chunks[iffMaxChunks-1] || ff.id3.off != at || !bytes.Equal(after[8:at], b[8:at]) {
				t.Fatalf("%s: %d chunks, or the chunks before the tag changed", kind, len(ff.chunks))
			}
			if inPlace := len(after) == len(b); inPlace != (i == 0) {
				t.Fatalf("%s: change %d in place %v", kind, i, inPlace)
			}
			if info, err := Read(p); err != nil || !info.Writable || info.Fields[Title] != c.Set[Title] {
				t.Fatalf("%s: after %v %#v", kind, err, info)
			}
		}
	}
}

// Only the first of each text chunk is read: a hundred empty NAMEs cost one.
func TestAIFFTextChunksOnce(t *testing.T) {
	chunks := [][]byte{iffChunkOf(true, "COMM", aiffComm())}
	for i := 0; i < 100; i++ {
		chunks = append(chunks, iffChunkOf(true, "NAME", make([]byte, 32<<10)))
	}
	chunks = append(chunks, iffChunkOf(true, "AUTH", []byte("Auth")), iffChunkOf(true, "SSND", make([]byte, 108)))
	p := iffTemp(t, "names.aiff", iffMake("AIFF", chunks...))
	var info *Info
	if n := iffAllocs(func() { info, _ = Read(p) }); n > 1<<20 || info == nil || info.Fields[Artist] != "Auth" || info.Fields[Title] != "" {
		t.Fatalf("%d KB allocated, %#v", n>>10, info)
	}
}

// iffWithTag is a file of kind ("aiff", "wav", "dsf") whose ID3 tag is tag, after the audio.
func iffWithTag(t *testing.T, kind string, tag, audio []byte) string {
	switch kind {
	case "aiff":
		return iffTemp(t, "a.aiff", iffMake("AIFF", iffChunkOf(true, "COMM", aiffComm()),
			iffChunkOf(true, "SSND", append(make([]byte, 8), audio...)), iffChunkOf(true, "ID3 ", tag)))
	case "wav":
		return iffTemp(t, "a.wav", iffMake("WAVE", wavFmt(), iffChunkOf(false, "data", audio), iffChunkOf(false, "id3 ", tag)))
	}
	return iffTemp(t, "a.dsf", dsfMake(audio, tag, nil))
}

// iffRoom is where the ID3 tag of b (an AIFF, WAV or DSF) is, and the room it has.
func iffRoom(t *testing.T, b []byte) (int64, int64) {
	t.Helper()
	if string(b[:4]) == "DSD " {
		d, err := readDSF(bytes.NewReader(b), int64(len(b)))
		if err != nil || d.tag == nil {
			t.Fatalf("no tag: %v", err)
		}
		return d.ptr, d.tagLen
	}
	ff, err := readIFF(bytes.NewReader(b), int64(len(b)))
	if err != nil || ff.id3 == nil {
		t.Fatalf("no ID3 chunk: %v", err)
	}
	return ff.id3.off + 8, ff.id3.size
}

// iffFrameSet is tg's frame ids in order of id: which frames a tag has, wherever they are.
func iffFrameSet(tg *id3Tag) string {
	var out []string
	for _, f := range tg.frames {
		out = append(out, f.id)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// An in-place save of a song with a big cover streams the cover instead of holding it: a title edit
// of a 24 MB cover once took 27 MB. The written tag is what writeTo writes for its frames, the
// ReplayGain frame is as it was, and the audio is untouched.
func TestIFFInPlaceBigPicture(t *testing.T) {
	pic := append(append([]byte(nil), testJPEG...), iffAudio(6<<20)...)
	for _, kind := range []string{"aiff", "wav", "dsf"} {
		v, text := byte(4), utf8z
		if kind == "dsf" {
			v, text = 3, lat
		}
		gain := rawFrame(v, "TXXX", txxxBody("REPLAYGAIN_TRACK_GAIN", "-6.20 dB"))
		tag := rawTag(v, 0, 4096, rawFrame(v, "TIT2", text("Old")), gain, rawFrame(v, "TPE1", text("Someone")),
			rawFrame(v, "APIC", apicBody("image/jpeg", 3, "", pic)))
		audio := iffAudio(4096)
		p := iffWithTag(t, kind, tag, audio)
		n := size(t, p)
		old, _ := os.ReadFile(p)
		for i, e := range []struct {
			c     Changes
			ids   string
			title string
		}{
			{Changes{Set: map[Field]string{Album: "Record"}}, "APIC TALB TIT2 TPE1 TXXX", "Old"},
			{Changes{Set: map[Field]string{Title: "Older still"}}, "APIC TALB TIT2 TPE1 TXXX", "Older still"},
			{Changes{Set: map[Field]string{Artist: ""}}, "APIC TALB TIT2 TXXX", "Older still"},
			{Changes{Set: map[Field]string{Title: "Old"}}, "APIC TALB TIT2 TXXX", "Old"},
			{Changes{Set: map[Field]string{Title: "New"}}, "APIC TALB TIT2 TXXX", "New"},
			{Changes{RemovePicture: true}, "TALB TIT2 TXXX", "New"},
		} {
			var err error
			if a := iffAllocs(func() { err = Write(p, e.c) }); err != nil || a > 2<<20 {
				t.Fatalf("%s %d: %v, %d KB allocated", kind, i, err, a>>10)
			}
			b, _ := os.ReadFile(p)
			off, room := iffRoom(t, b)
			tg := mustParse(t, b[off:off+room])
			if int64(len(b)) != n || !bytes.Equal(b[:off], old[:off]) || !bytes.Equal(b[off+room:], old[off+room:]) {
				t.Fatalf("%s %d: not in place, or what isn't the tag changed", kind, i)
			}
			if iffFrameSet(tg) != e.ids || !bytes.Equal(tg.encode(int(room)), b[off:off+room]) {
				t.Fatalf("%s %d: frames %s, or not as writeTo writes them", kind, i, ids(tg))
			}
			for _, f := range tg.frames {
				if f.id == "TXXX" && !bytes.Equal(f.data, gain[10:]) {
					t.Fatalf("%s %d: the gain frame changed", kind, i)
				}
			}
			if at := bytes.Index(b, pic); e.c.RemovePicture != (at < 0) {
				t.Fatalf("%s %d: the picture is at %d", kind, i, at)
			}
			info, _ := Read(p)
			if info.Fields[Title] != e.title || info.Fields[Album] != "Record" || len(info.Notes) != 0 {
				t.Fatalf("%s %d: %#v", kind, i, info)
			}
		}
	}
}

// Frames left in the file on both sides of small ones that change: each lands whole, the tag's
// frames in a place a reader takes in order, whatever moves.
func TestIFFInPlaceTwoBigFrames(t *testing.T) {
	a := append([]byte("owner-a\x00"), iffAudio(100<<10)...)
	b := append([]byte("owner-b\x00"), bytes.Repeat([]byte{7, 1, 9}, 30<<10)...)
	tag := rawTag(4, 0, 3000, rawFrame(4, "TIT2", utf8z("Title")), rawFrame(4, "PRIV", a),
		rawFrame(4, "TPE1", utf8z("Artist")), rawFrame(4, "TALB", utf8z("Album")), rawFrame(4, "PRIV", b),
		rawFrame(4, "TCON", utf8z("Jazz")))
	p := iffWithTag(t, "wav", tag, iffAudio(1000))
	n := size(t, p)
	want := map[Field]string{Title: "Title", Artist: "Artist", Album: "Album", Genre: "Jazz"}
	for i, set := range []map[Field]string{
		{Artist: "A much longer artist"}, {Album: ""}, {Title: "T"}, {Genre: "Rock"}, {Album: "Back again", Artist: "A"},
		{Composer: "Someone", Title: "A title past what the first frame left"}, {Artist: "", Genre: ""},
	} {
		if err := Write(p, Changes{Set: set}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		for k, v := range set {
			want[k] = v
		}
		bs, _ := os.ReadFile(p)
		off, room := iffRoom(t, bs)
		tg := mustParse(t, bs[off:off+room])
		var privs [][]byte
		for _, f := range tg.frames {
			if f.id == "PRIV" {
				privs = append(privs, f.data)
			}
		}
		if int64(len(bs)) != n || len(privs) != 2 || !bytes.Equal(privs[0], a) || !bytes.Equal(privs[1], b) ||
			!bytes.Equal(tg.encode(int(room)), bs[off:off+room]) {
			t.Fatalf("%d: frames %s", i, ids(tg))
		}
		info, _ := Read(p)
		for _, k := range Fields {
			if info.Fields[k] != want[k] {
				t.Fatalf("%d: %s %q, want %q", i, k, info.Fields[k], want[k])
			}
		}
	}
}

// A frame left in the file that would have to move towards the end can't be written in place: the
// writing would overtake it. The file is written again instead.
func TestPatchID3Refuses(t *testing.T) {
	a := append([]byte("owner-a\x00"), iffAudio(100<<10)...)
	b := append([]byte("owner-b\x00"), iffAudio(90<<10)...)
	tag := rawTag(4, 0, 100, rawFrame(4, "TIT2", utf8z("Title")), rawFrame(4, "PRIV", a), rawFrame(4, "PRIV", b))
	p := iffWithTag(t, "aiff", tag, iffAudio(1000))
	old, _ := os.ReadFile(p)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ff, err := readIFF(f, int64(len(old)))
	if err != nil || len(ff.tag.frames) != 3 || !ff.tag.frames[1].lazy || !ff.tag.frames[2].lazy {
		t.Fatalf("%v: frames not left in the file", err)
	}
	tg, off, room := ff.tag, ff.id3.off+8, ff.id3.size
	tg.frames[1], tg.frames[2] = tg.frames[2], tg.frames[1]
	if err := patchID3(p, off, tg, room); err != errPatchID3 {
		t.Fatalf("patch %v", err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, old) {
		t.Fatal("changed")
	}
}

// Tags of every shape, frames left in the file among small ones, edited every way: each save, in
// place or not, keeps the big frames whole and the fields as set.
func TestIFFInPlaceShapes(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	word := func() string {
		if rnd.Intn(4) == 0 {
			return ""
		}
		return strings.Repeat("ab", rnd.Intn(12)) + "z"
	}
	frameOf := map[Field]string{Title: "TIT2", Artist: "TPE1", Album: "TALB", Genre: "TCON", Composer: "TCOM"}
	for i := 0; i < 50; i++ {
		v := byte(3 + rnd.Intn(2))
		var frames, bigs [][]byte
		want := map[Field]string{}
		for _, k := range rnd.Perm(len(Fields)) {
			f := Fields[k]
			if id := frameOf[f]; id != "" && rnd.Intn(3) > 0 {
				want[f] = strings.Repeat("x", 1+rnd.Intn(30))
				frames = append(frames, rawFrame(v, id, lat(want[f])))
			}
			if rnd.Intn(4) == 0 {
				n := 5<<10 + rnd.Intn(20<<10)
				if len(bigs) == 0 {
					n += 64 << 10 // past the first bytes read, so left in the file
				}
				big := append([]byte(fmt.Sprintf("owner-%d\x00", len(bigs))), iffAudio(n)...)
				bigs = append(bigs, big)
				frames = append(frames, rawFrame(v, "PRIV", big))
			}
		}
		kind := []string{"aiff", "wav", "dsf"}[i%3]
		p := iffWithTag(t, kind, rawTag(v, 0, rnd.Intn(200), frames...), iffAudio(999))
		for j := 0; j < 4; j++ {
			set := map[Field]string{}
			for f := range frameOf {
				if rnd.Intn(3) == 0 {
					set[f] = word()
					want[f] = set[f]
				}
			}
			if err := Write(p, Changes{Set: set}); err != nil {
				t.Fatalf("%d.%d: %v", i, j, err)
			}
			b, _ := os.ReadFile(p)
			off, room := iffRoom(t, b)
			tg := mustParse(t, b[off:off+room])
			var got [][]byte
			for _, f := range tg.frames {
				if f.id == "PRIV" {
					got = append(got, f.data)
				}
			}
			if len(got) != len(bigs) || len(tg.notes()) != 0 {
				t.Fatalf("%d.%d: frames %s", i, j, ids(tg))
			}
			for k := range got {
				if !bytes.Equal(got[k], bigs[k]) {
					t.Fatalf("%d.%d: big frame %d changed", i, j, k)
				}
			}
			info, _ := Read(p)
			for f := range frameOf {
				if info.Fields[f] != want[f] {
					t.Fatalf("%d.%d: %s %q, want %q", i, j, f, info.Fields[f], want[f])
				}
			}
		}
	}
}
