package tags

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// wavList is a LIST INFO chunk of id, value pairs, each value NUL-terminated.
func wavList(kv ...string) []byte {
	b := []byte("INFO")
	for i := 0; i+1 < len(kv); i += 2 {
		b = append(b, iffChunkOf(false, kv[i], append([]byte(kv[i+1]), 0))...)
	}
	return iffChunkOf(false, "LIST", b)
}

func wavFmt() []byte {
	return iffChunkOf(false, "fmt ", []byte{1, 0, 2, 0, 0x44, 0xAC, 0, 0, 0x10, 0xB1, 2, 0, 4, 0, 16, 0})
}

func TestWAVInPlace(t *testing.T) {
	list := wavList("INAM", "Info title", "IART", "Info artist", "ISFT", "Lavf60")
	audio := iffAudio(7001)
	tag := rawTag(3, 0, 200, rawFrame(3, "TIT2", lat("Old")), rawFrame(3, "TPE1", u16("Ärtist ★", false)))
	p := iffTemp(t, "a.wav", iffMake("WAVE", wavFmt(), list, iffChunkOf(false, "data", audio), iffChunkOf(false, "ID3 ", tag)))
	n := size(t, p)
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "WAV (ID3v2.3)" || !info.Writable || info.Fields[Title] != "Old" || info.Fields[Artist] != "Ärtist ★" ||
		len(info.Notes) != 1 || !strings.HasPrefix(info.Notes[0], "It has a RIFF INFO list too, which saving leaves as it is") {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Nëw", Track: "4"}}); err != nil {
		t.Fatal(err)
	}
	ff, b := iffOpen(t, p)
	if size(t, p) != n || iffIDs(ff) != "fmt ,LIST,data,ID3 " || !bytes.Equal(iffBody(ff, b, "data"), audio) ||
		!bytes.Equal(b[36:36+len(list)], list) {
		t.Fatalf("not in place, or a chunk changed: %s", iffIDs(ff))
	}
	info, _ = Read(p)
	if info.Fields[Title] != "Nëw" || info.Fields[Track] != "4" || info.Fields[Artist] != "Ärtist ★" {
		t.Fatalf("fields %#v", info.Fields)
	}
	// A picture doesn't fit: written again, the chunk keeps its id.
	if err := Write(p, Changes{Picture: &Picture{Data: testJPEG}}); err != nil {
		t.Fatal(err)
	}
	ff, b = iffOpen(t, p)
	if iffIDs(ff) != "fmt ,LIST,data,ID3 " || !bytes.Equal(iffBody(ff, b, "data"), audio) || int64(len(b)) != ff.id3.next {
		t.Fatalf("chunks %s", iffIDs(ff))
	}
	if tg := iffTagOf(t, ff, b); tg.major != 3 || ids(tg) != "TIT2 TPE1 TRCK APIC" {
		t.Fatalf("v2.%d %s", tg.major, ids(tg))
	}
}

func TestWAVInfoOnly(t *testing.T) {
	list := wavList("INAM", "Tëst", "IART", "\xC4rtist", "IPRT", "5/12", "ICRD", "2001", "IGNR", "", "ISFT", "Lavf")
	audio := iffAudio(999)
	p := iffTemp(t, "b.wav", iffMake("WAVE", wavFmt(), list, iffChunkOf(false, "data", audio)))
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Field]string{Title: "Tëst", Artist: "Ärtist", Track: "5", TrackTotal: "12", Date: "2001"}
	if info.Format != "WAV (RIFF INFO)" || !info.Writable || len(info.Fields) != len(want) || len(info.Notes) != 1 {
		t.Fatalf("info %#v", info)
	}
	for k, v := range want {
		if info.Fields[k] != v {
			t.Fatalf("%s: %q", k, info.Fields[k])
		}
	}
	if err := Write(p, Changes{Set: map[Field]string{Album: "Record", TrackTotal: "13"}}); err != nil {
		t.Fatal(err)
	}
	ff, b := iffOpen(t, p)
	if iffIDs(ff) != "fmt ,LIST,data,id3 " || !bytes.Equal(iffBody(ff, b, "data"), audio) ||
		!bytes.Equal(b[36:36+len(list)], list) || ff.id3.off != 36+int64(len(list))+8+1000 {
		t.Fatalf("chunks %s, id3 at %d", iffIDs(ff), ff.id3.off)
	}
	info, _ = Read(p)
	want[Album], want[TrackTotal] = "Record", "13"
	if info.Format != "WAV (ID3v2.4)" || len(info.Fields) != len(want) {
		t.Fatalf("info %#v", info)
	}
	for k, v := range want {
		if info.Fields[k] != v {
			t.Fatalf("%s: %q", k, info.Fields[k])
		}
	}
}

func TestWAVTrailing(t *testing.T) {
	audio := iffAudio(2001)
	tail := v1Tag("Trailing", "", "", "", "", 0, 255)
	b := append(iffMake("WAVE", wavFmt(), iffChunkOf(false, "data", audio)), tail...)
	p := iffTemp(t, "c.wav", b)
	if info, _ := Read(p); info.Format != "WAV (no tag yet)" || !info.Writable {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != nil {
		t.Fatal(err)
	}
	ff, after := iffOpen(t, p)
	if iffIDs(ff) != "fmt ,data,id3 " || !bytes.Equal(iffBody(ff, after, "data"), audio) ||
		!bytes.Equal(after[ff.end:], tail) || ff.id3.next != ff.end {
		t.Fatalf("chunks %s, end %d of %d", iffIDs(ff), ff.end, len(after))
	}
}

func TestWAVRF64(t *testing.T) {
	audio := iffAudio(600)
	ds64 := make([]byte, 28)
	tag := rawTag(4, 0, 10, rawFrame(4, "TIT2", utf8z("Big")))
	chunks := [][]byte{iffChunkOf(false, "ds64", ds64), wavFmt(), iffChunkOf(false, "data", audio), iffChunkOf(false, "id3 ", tag)}
	b := iffMake("WAVE", chunks...)
	copy(b, "RF64")
	binary.LittleEndian.PutUint32(b[4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint64(b[20:], uint64(len(b)-8))
	binary.LittleEndian.PutUint64(b[28:], uint64(len(audio)))
	at := 12 + len(chunks[0]) + len(chunks[1])
	binary.LittleEndian.PutUint32(b[at+4:], 0xFFFFFFFF)
	p := iffTemp(t, "d.wav", b)
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "WAV (ID3v2.4)" || info.Writable || info.Fields[Title] != "Big" || !strings.Contains(info.Notes[0], "RF64") {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrReadOnly {
		t.Fatalf("write %v", err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
		t.Fatal("changed")
	}
}

func TestWAVSecondTag(t *testing.T) {
	one := rawTag(4, 0, 50, rawFrame(4, "TIT2", utf8z("One")))
	two := rawTag(4, 0, 50, rawFrame(4, "TIT2", utf8z("Two")))
	p := iffTemp(t, "e.wav", iffMake("WAVE", wavFmt(), iffChunkOf(false, "data", iffAudio(10)),
		iffChunkOf(false, "id3 ", one), iffChunkOf(false, "ID3 ", two)))
	info, _ := Read(p)
	if info.Fields[Title] != "One" || !strings.Contains(strings.Join(info.Notes, "|"), "second ID3 chunk") {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Uno"}}); err != nil {
		t.Fatal(err)
	}
	ff, b := iffOpen(t, p)
	if iffTagOf(t, ff, b).text("TIT2") != "Uno" || !bytes.Equal(iffBody(ff, b, "ID3 "), two) {
		t.Fatal("the wrong tag changed")
	}
}

// An INFO item whose size has its top bit set is where the list ends, not a panic: on the Q2 (and
// GOARCH=386) an int is 32 bits, and that size turned negative.
func TestWAVInfoHugeItem(t *testing.T) {
	for _, n := range []uint32{0x80000000, 0xFFFFFFF0, 0xFFFFFFF8, 0xFFFFFFFF, 0x7FFFFFFF, 9} {
		item := []byte("IART\x00\x00\x00\x00abcdabcd")
		binary.LittleEndian.PutUint32(item[4:], n)
		list := append([]byte("INFO"), iffChunkOf(false, "INAM", []byte("Good\x00"))...)
		list = iffChunkOf(false, "LIST", append(list, item...))
		audio := iffAudio(100)
		p := iffTemp(t, "a.wav", iffMake("WAVE", wavFmt(), list, iffChunkOf(false, "data", audio)))
		info, err := Read(p)
		if err != nil || info.Format != "WAV (RIFF INFO)" || !info.Writable || info.Fields[Title] != "Good" || info.Fields[Artist] != "" {
			t.Fatalf("%#x: %v %#v", n, err, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Album: "x"}}); err != nil {
			t.Fatalf("%#x: %v", n, err)
		}
		ff, b := iffOpen(t, p)
		if !bytes.Equal(iffBody(ff, b, "data"), audio) || !bytes.Equal(b[36:36+len(list)], list) {
			t.Fatalf("%#x: audio or the list changed", n)
		}
	}
}

// Only a LIST of kind INFO is read whole: the others, however many, cost four bytes each.
func TestWAVOtherLists(t *testing.T) {
	chunks := [][]byte{wavFmt()}
	for i := 0; i < 100; i++ {
		chunks = append(chunks, iffChunkOf(false, "LIST", append([]byte("adtl"), make([]byte, 32<<10)...)))
	}
	chunks = append(chunks, wavList("INAM", "Name"), iffChunkOf(false, "data", iffAudio(100)))
	p := iffTemp(t, "b.wav", iffMake("WAVE", chunks...))
	var info *Info
	if n := iffAllocs(func() { info, _ = Read(p) }); n > 1<<20 || info == nil || info.Fields[Title] != "Name" {
		t.Fatalf("%d KB allocated, %#v", n>>10, info)
	}
}
