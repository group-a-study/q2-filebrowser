package tags

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

// rawFrame is a v2.3 or v2.4 frame as a file has it: its size syncsafe in 2.4, its flags as given.
func rawFrame(major byte, id string, body []byte, flags ...byte) []byte {
	h := make([]byte, 10)
	copy(h, id)
	if major == 4 {
		id3PutSyncsafe(h[4:], int64(len(body)))
	} else {
		binary.BigEndian.PutUint32(h[4:], uint32(len(body)))
	}
	if len(flags) == 2 {
		h[8], h[9] = flags[0], flags[1]
	}
	return append(h, body...)
}

func rawFrame22(id string, body []byte) []byte {
	n := len(body)
	return append([]byte{id[0], id[1], id[2], byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

// rawTag is a tag of version major with header flags: the frames, then pad zeros.
func rawTag(major, flags byte, pad int, frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	body = append(body, make([]byte, pad)...)
	h := []byte{'I', 'D', '3', major, 0, flags, 0, 0, 0, 0}
	id3PutSyncsafe(h[6:], int64(len(body)))
	return append(h, body...)
}

func lat(s string) []byte   { return append([]byte{0}, s...) }
func utf8z(s string) []byte { return append([]byte{3}, s...) }

// u16 is s in UTF-16 with a BOM, its encoding byte first.
func u16(s string, be bool) []byte {
	b := []byte{1, 0xFF, 0xFE}
	if be {
		b[1], b[2] = 0xFE, 0xFF
	}
	for _, u := range utf16.Encode([]rune(s)) {
		if be {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return b
}

// langBody is a COMM or USLT body in Latin-1.
func langBody(lang, desc, text string) []byte {
	return append(append(append([]byte{0}, lang...), desc...), append([]byte{0}, text...)...)
}

// apicBody is an APIC body in Latin-1.
func apicBody(mime string, typ byte, desc string, data []byte) []byte {
	b := append(append([]byte{0}, mime...), 0, typ)
	return append(append(append(b, desc...), 0), data...)
}

func txxxBody(desc, val string) []byte { return append(append(lat(desc), 0), val...) }

var testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, bytes.Repeat([]byte{0xFF, 0xE0, 7}, 700)...)

func mustParse(t *testing.T, b []byte) *id3Tag {
	t.Helper()
	tag, err := parseID3(b)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func ids(t *id3Tag) string {
	var out []string
	for _, f := range t.frames {
		out = append(out, f.id)
	}
	return strings.Join(out, " ")
}

func TestID3v24Fields(t *testing.T) {
	smpb := rawFrame(4, "COMM", langBody("eng", "iTunSMPB", " 00000000 00000210 0000096C"))
	b := rawTag(4, 0, 64,
		rawFrame(4, "TIT2", utf8z("Ünïcode")),
		smpb,
		rawFrame(4, "TPE1", utf8z("A\x00B\x00")),
		rawFrame(4, "TALB", lat("Alb\xfcm")),
		rawFrame(4, "TPE2", u16("Bänd ★", false)),
		rawFrame(4, "TRCK", lat("3/12")),
		rawFrame(4, "TPOS", lat("1/2")),
		rawFrame(4, "TDRC", lat("2004-03-22")),
		rawFrame(4, "TCON", lat("17")),
		rawFrame(4, "TCOM", append([]byte{2}, 0, 'B', 0, 'a', 0, 'c', 0, 'h')),
		rawFrame(4, "COMM", langBody("fra", "", "Nice")),
		rawFrame(4, "USLT", langBody("eng", "", "line one\nline two")),
		rawFrame(4, "APIC", apicBody("image/png", 4, "back", []byte("\x89PNGback"))),
		rawFrame(4, "APIC", apicBody("image/jpeg", 3, "", testJPEG)),
	)
	tag := mustParse(t, b)
	want := map[Field]string{Title: "Ünïcode", Artist: "A / B", Album: "Albüm", AlbumArtist: "Bänd ★", Track: "3",
		TrackTotal: "12", Disc: "1", DiscTotal: "2", Date: "2004-03-22", Genre: "Rock", Composer: "Bach",
		Comment: "Nice", Lyrics: "line one\nline two"}
	if got := tag.fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields\n got %#v\nwant %#v", got, want)
	}
	p := tag.picture()
	if p == nil || p.Type != 3 || p.MIME != "image/jpeg" || !bytes.Equal(p.Data, testJPEG) || p.hasOffset {
		t.Fatalf("picture %#v", p)
	}
	if tag.format() != "ID3v2.4" || len(tag.notes()) != 0 {
		t.Fatalf("%s %v", tag.format(), tag.notes())
	}
	// Written again unchanged, it is the same bytes.
	if out := tag.encode(len(b)); !bytes.Equal(out, b) {
		t.Fatal("an unchanged tag came out different")
	}
}

func TestID3v23Text(t *testing.T) {
	b := rawTag(3, 0, 10,
		rawFrame(3, "TIT2", u16("Tïtle ★", true)),
		rawFrame(3, "TPE1", append(u16("One", false), append([]byte{0, 0}, u16("Two", true)[1:]...)...)),
		rawFrame(3, "TCON", lat("(4)Eurodisco")),
		rawFrame(3, "TYER", lat("2004")),
		rawFrame(3, "TDAT", lat("2203")),
		rawFrame(3, "TXXX", txxxBody("ALBUM ARTIST", "Various")),
		rawFrame(3, "COMM", langBody("eng", "iTunNORM", " 0000044E")),
		rawFrame(3, "TRCK", lat("05")),
		rawFrame(3, "TIT1", nil), // empty: dropped
	)
	tag := mustParse(t, b)
	want := map[Field]string{Title: "Tïtle ★", Artist: "One / Two", Genre: "Disco / Eurodisco", Date: "2004-03-22",
		AlbumArtist: "Various", Track: "5"}
	if got := tag.fields(); !reflect.DeepEqual(got, want) || len(tag.notes()) != 0 || len(tag.frames) != 8 {
		t.Fatalf("fields\n got %#v\nwant %#v\n%v %s", got, want, tag.notes(), ids(tag))
	}
	for v, want := range map[string]string{"(17)": "Rock", "(17)Rock": "Rock", "(RX)(CR)": "Remix / Cover",
		"((Twisted)": "(Twisted)", "Synthpop": "Synthpop", "147": "Synthpop", "(255)": ""} {
		if got := id3Join(id3Genre(v)); got != want {
			t.Errorf("genre %q: %q, want %q", v, got, want)
		}
	}
}

func TestID3v22(t *testing.T) {
	pic := append([]byte{0}, "JPG"...)
	pic = append(append(pic, 3, 'c', 0), testJPEG...)
	b := rawTag(2, 0, 20,
		rawFrame22("TT2", lat("Old")),
		rawFrame22("TP1", u16("Ärtist", false)),
		rawFrame22("CRM", []byte("owner\x00x\x00secret")),
		rawFrame22("TRK", lat("2/9")),
		rawFrame22("COM", langBody("eng", "", "hi")),
		rawFrame22("PIC", pic),
	)
	tag := mustParse(t, b)
	if f := tag.fields(); f[Title] != "Old" || f[Artist] != "Ärtist" || f[Track] != "2" || f[TrackTotal] != "9" ||
		f[Comment] != "hi" {
		t.Fatalf("fields %#v", f)
	}
	if p := tag.picture(); p == nil || p.MIME != "image/jpeg" || p.Desc != "c" || !bytes.Equal(p.Data, testJPEG) {
		t.Fatalf("picture %#v", p)
	}
	if n := strings.Join(tag.notes(), "|"); !strings.Contains(n, "ID3v2.3") || !strings.Contains(n, "CRM") {
		t.Fatalf("notes %q", n)
	}
	tag.apply(Changes{Set: map[Field]string{Title: "Néw"}})
	out := tag.encode(0)
	if out[3] != 3 {
		t.Fatalf("written as v2.%d", out[3])
	}
	again := mustParse(t, out)
	if ids(again) != "TIT2 TPE1 TRCK COMM APIC" {
		t.Fatalf("frames %s", ids(again))
	}
	if f := again.fields(); f[Title] != "Néw" || f[Artist] != "Ärtist" || f[Comment] != "hi" {
		t.Fatalf("fields %#v", f)
	}
	if p := again.picture(); p == nil || p.MIME != "image/jpeg" || !bytes.Equal(p.Data, testJPEG) {
		t.Fatal("picture lost")
	}
}

// unsyncAll unsynchronises b the simple way: a zero after every 0xFF.
func unsyncAll(b []byte) []byte {
	var out []byte
	for _, c := range b {
		out = append(out, c)
		if c == 0xFF {
			out = append(out, 0)
		}
	}
	return out
}

func TestID3v23Unsync(t *testing.T) {
	plain := rawTag(3, 0, 0,
		rawFrame(3, "TIT2", u16("ÿÿ title", false)), // ÿ is 0xFF in UTF-16 LE
		rawFrame(3, "APIC", apicBody("image/jpeg", 3, "", testJPEG)),
	)
	body := unsyncAll(plain[10:])
	b := append([]byte{'I', 'D', '3', 3, 0, 0x80, 0, 0, 0, 0}, body...)
	id3PutSyncsafe(b[6:], int64(len(body)))
	tag := mustParse(t, b)
	if f := tag.fields(); f[Title] != "ÿÿ title" {
		t.Fatalf("fields %#v", f)
	}
	if p := tag.picture(); p == nil || !bytes.Equal(p.Data, testJPEG) {
		t.Fatal("picture")
	}
	if out := tag.encode(0); !bytes.Equal(out, plain) || out[5] != 0 {
		t.Fatal("not written plain")
	}
}

func deflate(b []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(b)
	w.Close()
	return z.Bytes()
}

func TestID3v24FrameFlags(t *testing.T) {
	dli := func(n int) []byte { b := make([]byte, 4); id3PutSyncsafe(b, int64(n)); return b }
	apic := apicBody("image/jpeg", 3, "", testJPEG)
	album := lat("Compressed album")
	b := rawTag(4, 0, 0,
		rawFrame(4, "TIT2", append([]byte{0x81}, utf8z("Grouped")...), 0, 0x40),
		rawFrame(4, "APIC", append(dli(len(apic)), unsyncAll(apic)...), 0, 0x03),
		rawFrame(4, "TALB", append(dli(len(album)), deflate(album)...), 0, 0x09),
		rawFrame(4, "TPE1", append([]byte{1}, "secret"...), 0, 0x04),
		rawFrame(4, "TCOM", lat("Kept"), 0x40, 0),
	)
	tag := mustParse(t, b)
	if f := tag.fields(); f[Title] != "Grouped" || f[Album] != "Compressed album" || f[Artist] != "" || f[Composer] != "Kept" {
		t.Fatalf("fields %#v", f)
	}
	if p := tag.picture(); p == nil || !bytes.Equal(p.Data, testJPEG) {
		t.Fatal("picture")
	}
	if n := strings.Join(tag.notes(), "|"); !strings.Contains(n, "1 of its frames") {
		t.Fatalf("notes %q", n)
	}
	want := rawTag(4, 0, 0,
		rawFrame(4, "TIT2", utf8z("Grouped")),
		rawFrame(4, "APIC", apic),
		rawFrame(4, "TALB", album),
		rawFrame(4, "TCOM", lat("Kept"), 0x40, 0), // the status flag stays; format flags go
	)
	if out := tag.encode(0); !bytes.Equal(out, want) {
		t.Fatalf("written\n%q\nwant\n%q", out[:60], want[:60])
	}
}

func TestID3v23Compressed(t *testing.T) {
	title := u16("Squeezed ★", false)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(title)))
	b := rawTag(3, 0, 0, rawFrame(3, "TIT2", append(size, deflate(title)...), 0, 0x80))
	if f := mustParse(t, b).fields(); f[Title] != "Squeezed ★" {
		t.Fatalf("fields %#v", f)
	}
}

func TestID3iTunesSizes(t *testing.T) {
	// iTunes wrote 2.4 frame sizes as plain numbers: 300 is 00 00 01 2C, which reads as 172 syncsafe.
	long := strings.Repeat("x", 300-1)
	plain := func(id string, body []byte) []byte {
		h := make([]byte, 10)
		copy(h, id)
		binary.BigEndian.PutUint32(h[4:], uint32(len(body)))
		return append(h, body...)
	}
	b := rawTag(4, 0, 100,
		plain("TIT2", lat("One")),
		plain("TXXX", append(lat("LONG"), append([]byte{0}, long[5:]...)...)),
		plain("TALB", lat("Two")),
		plain("COMM", langBody("eng", "", strings.Repeat("c", 200))), // 00 00 00 D3: not syncsafe at all
		plain("TPE1", lat("Three")),
	)
	tag := mustParse(t, b)
	if f := tag.fields(); f[Title] != "One" || f[Album] != "Two" || f[Artist] != "Three" || len(f[Comment]) != 200 {
		t.Fatalf("fields %#v", f)
	}
	if len(tag.notes()) != 0 {
		t.Fatalf("notes %v", tag.notes())
	}
	again := mustParse(t, tag.encode(0)) // written back syncsafe
	if ids(again) != "TIT2 TXXX TALB COMM TPE1" {
		t.Fatalf("frames %s", ids(again))
	}
}

func TestID3ExtendedHeaderAndFooter(t *testing.T) {
	frames := rawFrame(4, "TIT2", lat("Hi"))
	body := append([]byte{0, 0, 0, 6, 1, 0}, frames...) // a 6-byte extended header, syncsafe, itself counted
	b := append([]byte{'I', 'D', '3', 4, 0, 0x50, 0, 0, 0, 0}, body...)
	id3PutSyncsafe(b[6:], int64(len(body)))
	b = append(b, '3', 'D', 'I', 4, 0, 0x50, b[6], b[7], b[8], b[9])
	if id3Len(b) != int64(len(b)) {
		t.Fatalf("length %d of %d", id3Len(b), len(b))
	}
	tag := mustParse(t, b)
	if tag.fields()[Title] != "Hi" {
		t.Fatal("v2.4")
	}
	out := tag.encode(len(b))
	if len(out) != len(b) || out[5] != 0 || !bytes.Equal(out[10:10+len(frames)], frames) {
		t.Fatalf("written %q", out)
	}
	// v2.3's extended header size leaves itself out.
	b = rawTag(3, 0x40, 0, []byte{0, 0, 0, 6, 0, 0, 0, 0, 0, 0}, rawFrame(3, "TIT2", lat("Hi")))
	if mustParse(t, b).fields()[Title] != "Hi" {
		t.Fatal("v2.3")
	}
}

func TestID3Apply(t *testing.T) {
	smpb := rawFrame(3, "COMM", langBody("eng", "iTunSMPB", " 00000000 00000210 0000096C 0000000000A0C3E4"))
	gain := rawFrame(3, "TXXX", txxxBody("REPLAYGAIN_TRACK_GAIN", "-6.20 dB"))
	b := rawTag(3, 0, 500,
		rawFrame(3, "TIT2", lat("Old")),
		smpb,
		gain,
		rawFrame(3, "TXXX", txxxBody("ALBUMARTIST", "Stale")),
		rawFrame(3, "TDRC", lat("1999")),
		rawFrame(3, "TPE1", lat("Artist")),
		rawFrame(3, "COMM", langBody("deu", "", "alt")),
		rawFrame(3, "APIC", apicBody("image/jpeg", 3, "", testJPEG)),
	)
	tag := mustParse(t, b)
	tag.apply(Changes{Set: map[Field]string{Title: "Ťitle", Album: "Album", AlbumArtist: "AA", Date: "2004-03-22",
		Comment: "new", Lyrics: "la\nla"}})
	out := tag.encode(0)
	again := mustParse(t, out)
	if got := ids(again); got != "TIT2 COMM TXXX TYER TPE1 COMM TALB TPE2 USLT APIC" {
		t.Fatalf("frames %s", got)
	}
	// 2.3 keeps the year alone: TYER holds 4 digits, and TDAT goes.
	want := map[Field]string{Title: "Ťitle", Artist: "Artist", Album: "Album", AlbumArtist: "AA", Date: "2004",
		Comment: "new", Lyrics: "la\nla"}
	if got := again.fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields\n got %#v\nwant %#v", got, want)
	}
	// iTunSMPB and the ReplayGain frame are where they were, byte for byte.
	tit2 := 10 + 10 + len(again.frames[0].data)
	if !bytes.Equal(out[tit2:tit2+len(smpb)], smpb) || !bytes.Equal(out[tit2+len(smpb):tit2+len(smpb)+len(gain)], gain) {
		t.Fatal("iTunSMPB or ReplayGain moved or changed")
	}
	// Latin-1 where it fits, else UTF-16 with a BOM; the comment keeps its language.
	if d := again.frames[0].data; d[0] != 1 || d[1] != 0xFF || d[2] != 0xFE {
		t.Fatalf("title % x", d[:3])
	}
	if d := again.frames[6].data; d[0] != 0 || string(d[1:]) != "Album" {
		t.Fatalf("album % x", d)
	}
	if lang, _, _, _ := id3Lang(again.frames[5].data); string(lang) != "deu" {
		t.Fatalf("comment language %q", lang)
	}

	// In 2.4: UTF-8 and TDRC.
	tag = mustParse(t, rawTag(4, 0, 0, rawFrame(4, "TYER", lat("1999")), rawFrame(4, "TDAT", lat("0101"))))
	tag.apply(Changes{Set: map[Field]string{Title: "Ťitle", Date: "2004"}})
	again = mustParse(t, tag.encode(0))
	if ids(again) != "TDRC TIT2" || again.frames[1].data[0] != 3 || again.fields()[Date] != "2004" {
		t.Fatalf("frames %s %#v", ids(again), again.fields())
	}
	// Clearing removes.
	again.apply(Changes{Set: map[Field]string{Title: "", Date: ""}})
	if len(again.frames) != 0 {
		t.Fatalf("frames %s", ids(again))
	}
}

func TestID3TXXXFallbacks(t *testing.T) {
	tag := mustParse(t, rawTag(3, 0, 0,
		rawFrame(3, "TXXX", txxxBody("comment", "from ffmpeg")),
		rawFrame(3, "TXXX", txxxBody("Album Artist", "Band")),
		rawFrame(3, "TXXX", txxxBody("REPLAYGAIN_TRACK_PEAK", "0.98")),
	))
	if f := tag.fields(); f[Comment] != "from ffmpeg" || f[AlbumArtist] != "Band" {
		t.Fatalf("fields %#v", f)
	}
	tag.apply(Changes{Set: map[Field]string{Comment: "mine", AlbumArtist: ""}})
	if ids(tag) != "TXXX COMM" || tag.fields()[Comment] != "mine" || tag.fields()[AlbumArtist] != "" {
		t.Fatalf("frames %s %#v", ids(tag), tag.fields())
	}
}

func TestID3NumberPairs(t *testing.T) {
	tag := mustParse(t, rawTag(4, 0, 0, rawFrame(4, "TRCK", lat("3/12")), rawFrame(4, "TPOS", lat("1"))))
	step := func(set map[Field]string, trck, tpos string) {
		t.Helper()
		tag.apply(Changes{Set: set})
		if got := tag.text("TRCK"); got != trck {
			t.Fatalf("TRCK %q, want %q", got, trck)
		}
		if got := tag.text("TPOS"); got != tpos {
			t.Fatalf("TPOS %q, want %q", got, tpos)
		}
	}
	step(map[Field]string{Track: "4"}, "4/12", "1")
	step(map[Field]string{DiscTotal: "2"}, "4/12", "1/2")
	step(map[Field]string{TrackTotal: ""}, "4", "1/2")
	step(map[Field]string{Track: "5", TrackTotal: "9"}, "5/9", "1/2")
	step(map[Field]string{Disc: ""}, "5/9", "/2") // the total stays without its number
	step(map[Field]string{DiscTotal: ""}, "5/9", "")
}

func TestID3TotalAlone(t *testing.T) {
	// A total set on a song without a number is kept, as "/12", and reads back.
	tag := mustParse(t, rawTag(4, 0, 0, rawFrame(4, "TIT2", lat("T"))))
	tag.apply(Changes{Set: map[Field]string{TrackTotal: "12", DiscTotal: "3"}})
	if tag.text("TRCK") != "/12" || tag.text("TPOS") != "/3" {
		t.Fatalf("TRCK %q TPOS %q", tag.text("TRCK"), tag.text("TPOS"))
	}
	again := mustParse(t, tag.encode(0))
	if f := again.fields(); f[TrackTotal] != "12" || f[DiscTotal] != "3" || f[Track] != "" || f[Disc] != "" {
		t.Fatalf("fields %#v", f)
	}
	again.apply(Changes{Set: map[Field]string{Track: "4"}})
	if again.text("TRCK") != "4/12" {
		t.Fatalf("TRCK %q", again.text("TRCK"))
	}
	// On a song: Write saves it, and a later change of the total alone keeps it too.
	p := mpegFixture(t, "n.mp3", rawTag(3, 0, 4096, rawFrame(3, "TIT2", lat("T"))), mpegAudio(4000))
	for _, set := range []map[Field]string{{TrackTotal: "12", DiscTotal: "3"}, {TrackTotal: "13"}} {
		if err := Write(p, Changes{Set: set}); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := Read(p); err != nil || info.Fields[TrackTotal] != "13" || info.Fields[DiscTotal] != "3" ||
		info.Fields[Track] != "" {
		t.Fatalf("%v %#v", err, info)
	}
}

func TestID3v23Date(t *testing.T) {
	// 2.3's TYER is the year's 4 digits alone; TDAT, TIME, TRDA and TDRC go.
	tag := mustParse(t, rawTag(3, 0, 0, rawFrame(3, "TYER", lat("1999")), rawFrame(3, "TDAT", lat("3112")),
		rawFrame(3, "TIME", lat("1230")), rawFrame(3, "TIT2", lat("T")), rawFrame(3, "TRDA", lat("x"))))
	if d := tag.fields()[Date]; d != "1999-12-31" {
		t.Fatalf("date %q", d)
	}
	tag.apply(Changes{Set: map[Field]string{Date: "2004-05-06"}})
	if ids(tag) != "TYER TIT2" || tag.text("TYER") != "2004" || tag.fields()[Date] != "2004" {
		t.Fatalf("frames %s, TYER %q", ids(tag), tag.text("TYER"))
	}
	// A date that doesn't start with a year has none to write.
	tag.apply(Changes{Set: map[Field]string{Date: "circa 1970"}})
	if ids(tag) != "TIT2" {
		t.Fatalf("frames %s", ids(tag))
	}
	// 2.4 keeps the whole date in TDRC.
	tag = mustParse(t, rawTag(4, 0, 0, rawFrame(4, "TIT2", lat("T"))))
	tag.apply(Changes{Set: map[Field]string{Date: "2004-05-06"}})
	if tag.text("TDRC") != "2004-05-06" {
		t.Fatalf("TDRC %q", tag.text("TDRC"))
	}
}

// id3Allocs is the bytes f allocates.
func id3Allocs(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// id3Noise is n bytes standing in for an image, 0xFF among them.
func id3Noise(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + i>>9)
	}
	return b
}

// unsyncTag is a v2.2 or v2.3 tag unsynchronised whole: the frames, then pad zeros.
func unsyncTag(major byte, pad int, frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	body = append(unsyncAll(body), make([]byte, pad)...)
	h := []byte{'I', 'D', '3', major, 0, 0x80, 0, 0, 0, 0}
	id3PutSyncsafe(h[6:], int64(len(body)))
	return append(h, body...)
}

func TestID3CompressedBomb(t *testing.T) {
	// A few KB of compressed frame that inflates to MBs: text is inflated only up to id3MaxText and the
	// tag's id3MaxHeld, anything else not at all; the frames past that are left out, and said to be.
	zipped := func(major byte, body []byte, size bool) []byte {
		var z bytes.Buffer
		w, _ := zlib.NewWriterLevel(&z, 9)
		w.Write(body)
		w.Close()
		if !size {
			return z.Bytes()
		}
		n := make([]byte, 4)
		if major == 3 {
			binary.BigEndian.PutUint32(n, uint32(len(body)))
		} else {
			id3PutSyncsafe(n, int64(len(body)))
		}
		return append(n, z.Bytes()...)
	}
	title := rawFrame(3, "TIT2", lat("x"))
	huge := append(lat("ALBUMARTIST\x00"), bytes.Repeat([]byte("A"), 3<<20)...)
	priv := append([]byte("owner\x00"), bytes.Repeat([]byte{7}, 3<<20)...)
	var privs, many [][]byte
	for i := 0; i < 8; i++ {
		privs = append(privs, rawFrame(3, "PRIV", zipped(3, priv, true), 0, 0x80))
	}
	for i := 0; i < 12; i++ { // each under id3MaxText, together past id3MaxHeld
		body := append(lat(fmt.Sprintf("T%d\x00", i)), bytes.Repeat([]byte("B"), 900<<10)...)
		many = append(many, rawFrame(3, "TXXX", zipped(3, body, true), 0, 0x80))
	}
	cases := []struct {
		name   string
		tag    []byte
		unread int
	}{
		{"v2.3 TXXX", rawTag(3, 0, 0, title, rawFrame(3, "TXXX", zipped(3, huge, true), 0, 0x80)), 1},
		{"v2.4 TPE1", rawTag(4, 0, 0, rawFrame(4, "TIT2", lat("x")),
			rawFrame(4, "TPE1", zipped(4, append(lat(""), bytes.Repeat([]byte("B"), 3<<20)...), true), 0, 0x09)), 1},
		{"v2.4 TXXX without its length", rawTag(4, 0, 0, rawFrame(4, "TIT2", lat("x")),
			rawFrame(4, "TXXX", zipped(4, huge, false), 0, 0x08)), 1},
		{"v2.3 PRIV", rawTag(3, 0, 0, append([][]byte{title}, privs...)...), 8},
		{"v2.3 many TXXX", rawTag(3, 0, 0, append([][]byte{title}, many...)...), 8},
	}
	for _, c := range cases {
		p := mpegFixture(t, "bomb.mp3", c.tag, mpegAudio(4000))
		var info *Info
		var err error
		n := id3Allocs(func() { info, err = Read(p) })
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		notes := strings.Join(info.Notes, "|")
		if n > 8<<20 || info.Fields[Title] != "x" || info.Fields[AlbumArtist] != "" || info.Fields[Artist] != "" ||
			!strings.Contains(notes, fmt.Sprintf("leaves out %d of its frames", c.unread)) {
			t.Fatalf("%s: %d MB allocated, %d fields, notes %q", c.name, n>>20, len(info.Fields), notes)
		}
	}
}

func TestID3BigFramesStayInTheFile(t *testing.T) {
	// A tag unsynchronised whole, a 2.4 frame unsynchronised, and a 2.2 picture (which changes form) are
	// read without holding the picture, and saved with it copied a piece at a time.
	cover := append(append([]byte(nil), testJPEG...), id3Noise(5<<20)...)
	small := append(append([]byte(nil), testJPEG...), id3Noise(300<<10)...)
	apic := func(img []byte) []byte { return apicBody("image/jpeg", 3, "", img) }
	pic := func(img []byte) []byte {
		return append(append([]byte{0}, "JPG"...), append([]byte{3, 'c', 0}, img...)...)
	}
	dli := func(n int) []byte { b := make([]byte, 4); id3PutSyncsafe(b, int64(n)); return b }
	cases := []struct {
		name   string
		tag    []byte
		img    []byte
		shown  bool   // the picture is shown before saving
		undone uint64 // the bytes reading may take to show it, held unsynchronised
	}{
		{"v2.3 unsynchronised", unsyncTag(3, 4096, rawFrame(3, "TIT2", lat("Old")), rawFrame(3, "APIC", apic(cover))), cover, false, 0},
		{"v2.3 unsynchronised, small", unsyncTag(3, 4096, rawFrame(3, "TIT2", lat("Old")), rawFrame(3, "APIC", apic(small))), small, true, 2 * uint64(len(small))},
		{"v2.4 frame unsynchronised", rawTag(4, 0, 4096, rawFrame(4, "TIT2", lat("Old")),
			rawFrame(4, "APIC", append(dli(len(apic(cover))), unsyncAll(apic(cover))...), 0, 0x03)), cover, false, 0},
		{"v2.2", rawTag(2, 0, 4096, rawFrame22("TT2", lat("Old")), rawFrame22("PIC", pic(cover))), cover, true, 0},
		{"v2.2 unsynchronised", unsyncTag(2, 4096, rawFrame22("TT2", lat("Old")), rawFrame22("PIC", pic(cover))), cover, false, 0},
	}
	audio := mpegAudio(5000)
	for _, c := range cases {
		p := mpegFixture(t, "big.mp3", c.tag, audio)
		var info *Info
		var err error
		if n := id3Allocs(func() { info, err = Read(p) }); err != nil || n > 1<<20+c.undone {
			t.Fatalf("%s: %v, %d KB allocated", c.name, err, n>>10)
		}
		if info.Fields[Title] != "Old" {
			t.Fatalf("%s: %#v", c.name, info.Fields)
		}
		if c.shown != (info.Picture != nil) || c.shown && info.Picture.Size != len(c.img) ||
			!c.shown && !strings.Contains(strings.Join(info.Notes, "|"), "too big to show") {
			t.Fatalf("%s: picture %#v, notes %q", c.name, info.Picture, info.Notes)
		}
		if c.shown {
			if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, c.img) {
				t.Fatalf("%s: picture read wrong (%v)", c.name, err)
			}
		}
		if n := id3Allocs(func() { err = Write(p, Changes{Set: map[Field]string{Title: "New"}}) }); err != nil ||
			n > 2<<20+c.undone {
			t.Fatalf("%s: %v, %d KB allocated", c.name, err, n>>10)
		}
		b, _ := os.ReadFile(p)
		tag, err := parseID3(b)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		pr := tag.picture()
		if b[5] != 0 || ids(tag) != "TIT2 APIC" || tag.fields()[Title] != "New" || len(tag.notes()) != 0 ||
			pr == nil || pr.MIME != "image/jpeg" || !bytes.Equal(pr.Data, c.img) || !bytes.Equal(b[id3Len(b):], audio) {
			t.Fatalf("%s: written v2.%d flags %x, frames %s, %#v, notes %v", c.name, b[3], b[5], ids(tag), tag.fields(), tag.notes())
		}
		if info, _ := Read(p); info.Picture == nil || info.Picture.Size != len(c.img) {
			t.Fatalf("%s: picture after saving %#v", c.name, info.Picture)
		}
	}
}

func TestID3ManyFrames(t *testing.T) {
	// More frames than any real tag has: refused, quickly.
	tiny := rawFrame(3, "PRIV", []byte("x"))
	p := mpegFixture(t, "many.mp3", rawTag(3, 0, 0, bytes.Repeat(tiny, id3MaxFrames+1)), mpegAudio(400))
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("read: %v", err)
	}
	p = mpegFixture(t, "some.mp3", rawTag(3, 0, 0, rawFrame(3, "TIT2", lat("T")), bytes.Repeat(tiny, 1000)), mpegAudio(400))
	if info, err := Read(p); err != nil || info.Fields[Title] != "T" {
		t.Fatalf("read: %v", err)
	}
	// v2.2 frames 2.3 has no place for, each id once: told apart without going over the list each
	// time, and the note names a few.
	var frames [][]byte
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for i := 0; i < 5000; i++ {
		id := string([]byte{"XYZQJK"[i/1296], abc[i/36%36], abc[i%36]})
		frames = append(frames, rawFrame22(id, []byte{1}))
	}
	frames = append(frames, frames[:100]...) // some again
	p = mpegFixture(t, "v22.mp3", rawTag(2, 0, 0, append([][]byte{rawFrame22("TT2", lat("T"))}, frames...)...), mpegAudio(400))
	var info *Info
	var err error
	n := id3Allocs(func() { info, err = Read(p) })
	notes := strings.Join(info.Notes, "|")
	if err != nil || n > 8<<20 || len(notes) > 300 || !strings.Contains(notes, "XAA, XAB, XAC, XAD, XAE and 4995 more frames") {
		t.Fatalf("%v, %d MB allocated, notes %q", err, n>>20, notes)
	}
}

// id3CountRA counts the bytes read through it.
type id3CountRA struct {
	r io.ReaderAt
	n int64
}

func (c *id3CountRA) ReadAt(b []byte, off int64) (int, error) {
	k, err := c.r.ReadAt(b, off)
	c.n += int64(k)
	return k, err
}

func TestID3PaddingReadOnce(t *testing.T) {
	// A frame of 128 bytes or more reads differently as a plain size, so the padding after the frames is
	// looked at to choose; only its first bytes are.
	lyrics := append([]byte{3, 'e', 'n', 'g', 0}, strings.Repeat("l", 200)...)
	data := append(rawTag(4, 0, 4<<20, rawFrame(4, "TIT2", utf8z("x")), rawFrame(4, "USLT", lyrics)), mpegAudio(4000)...)
	c := &id3CountRA{r: bytes.NewReader(data)}
	info, _, err := (mpegCodec{"MP3"}).read(c, int64(len(data)))
	if err != nil || len(info.Fields[Lyrics]) != 200 || c.n > 70<<10 {
		t.Fatalf("%v %#v, %d bytes read", err, info.Fields, c.n)
	}
}

func TestID3Pictures(t *testing.T) {
	tag := mustParse(t, rawTag(3, 0, 0,
		rawFrame(3, "APIC", apicBody("image/png", 0, "other", []byte("\x89PNG"))),
		rawFrame(3, "TIT2", lat("T")),
		rawFrame(3, "APIC", apicBody("image/jpeg", 4, "", testJPEG)),
	))
	if p := tag.picture(); p == nil || p.Type != 0 {
		t.Fatalf("no front cover: the first, got %#v", p)
	}
	tag.apply(Changes{Picture: &Picture{MIME: "image/jpeg", Type: 3, Data: testJPEG}, Set: map[Field]string{Album: "A"}})
	if ids(tag) != "TIT2 TALB APIC" {
		t.Fatalf("frames %s", ids(tag))
	}
	again := mustParse(t, tag.encode(0))
	if p := again.picture(); p == nil || p.Type != 3 || p.MIME != "image/jpeg" || !bytes.Equal(p.Data, testJPEG) {
		t.Fatalf("picture %#v", p)
	}
	again.apply(Changes{RemovePicture: true})
	if ids(again) != "TIT2 TALB" || again.picture() != nil {
		t.Fatalf("frames %s", ids(again))
	}
}

func TestID3EncodeAndJunk(t *testing.T) {
	tag := newID3()
	tag.apply(Changes{Set: map[Field]string{Title: "x"}})
	if n := tag.needed(); n != 10+10+2 || len(tag.encode(0)) != 22 {
		t.Fatalf("needed %d", n)
	}
	out := tag.encode(100)
	if len(out) != 100 || id3Len(out) != 100 || out[3] != 4 || !bytes.Equal(out[22:], make([]byte, 78)) {
		t.Fatalf("encoded %q", out)
	}
	// Bytes that aren't a frame end the tag; saving leaves them out.
	b := rawTag(3, 0, 0, rawFrame(3, "TIT2", lat("ok")), []byte("ab\x01cdefghijklmn"))
	tag = mustParse(t, b)
	if tag.fields()[Title] != "ok" || len(tag.notes()) != 1 {
		t.Fatalf("%#v %v", tag.fields(), tag.notes())
	}
	for _, bad := range [][]byte{[]byte("ID3\x05\x00\x00\x00\x00\x00\x00"), []byte("ID3\x03\x00\x00\x00\x00\x01\x00"),
		[]byte("ID3\x02\x00\x40\x00\x00\x00\x00")} {
		if _, err := parseID3(bad); err == nil {
			t.Fatalf("%q read", bad)
		}
	}
}

// A frame left in the file whose first bytes the copying passes over (in 2.4 a group byte and a data
// length, unsynchronised; a 2.2 picture's header) is copied whole when an in-place save moves it
// towards the end across where the writing flushes, 64 KB in: what the writing overtakes is held as
// it was until the copying reads it. Once those bytes were read after being written over.
func TestID3InPlaceSkippedBytes(t *testing.T) {
	img := append(append([]byte(nil), testJPEG...), id3Noise(100<<10)...)
	body := apicBody("image/jpeg", 3, "", img)
	dl := make([]byte, 4)
	id3PutSyncsafe(dl, int64(len(body)))
	raw := unsyncAll(append(append([]byte{0xFF}, dl...), body...))
	title := rawFrame(4, "TIT2", lat("abc"))
	fill := 65534 - (10 + len(title) + 10 + 10) // the picture's body starts 2 bytes before 64 KB
	priv := rawFrame(4, "PRIV", append([]byte("o\x00"), make([]byte, fill-2)...))
	tag := rawTag(4, 0, 100, title, priv, rawFrame(4, "APIC", raw, 0, 0x43))
	if at := bytes.Index(tag, raw); at != 65534 {
		t.Fatalf("body at %d", at)
	}
	audio := mpegAudio(4000)
	saved := func(what, p string, c Changes) {
		t.Helper()
		st, _ := os.Stat(p)
		if err := Write(p, c); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if now, _ := os.Stat(p); !os.SameFile(st, now) || now.Size() != st.Size() {
			t.Fatalf("%s: not in place", what)
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, img) {
			t.Fatalf("%s: the cover changed (%v)", what, err)
		}
	}
	for _, kind := range []string{"mp3", "aiff", "wav", "dsf"} {
		p := mpegFixture(t, "g.mp3", tag, audio)
		if kind != "mp3" {
			p = iffWithTag(t, kind, tag, audio)
		}
		saved(kind, p, Changes{Set: map[Field]string{Title: "abcdefgh"}})
		if info, _ := Read(p); info.Fields[Title] != "abcdefgh" {
			t.Fatalf("%s: %#v", kind, info.Fields)
		}
	}
	// A 2.2 tag unsynchronised whole, its picture's description UTF-16 (0xFF 0xFE, stored as 0xFF 0
	// 0xFE), the lyrics before it 20 bytes shorter: the picture's body starts where its 2.2 header
	// did, at each place around 64 KB.
	for shift := 0; shift < 40; shift++ {
		pic := append([]byte{1, 'J', 'P', 'G', 3, 0xFF, 0xFE, 'c', 0, 0, 0}, img...)
		old := 65480 + shift
		lyr := langBody("eng", "", strings.Repeat("l", old))
		p := mpegFixture(t, "u.mp3", unsyncTag(2, 200, rawFrame22("TT2", lat("abc")), rawFrame22("ULT", lyr),
			rawFrame22("PIC", pic)), audio)
		saved(fmt.Sprintf("2.2, %d", shift), p, Changes{Set: map[Field]string{Lyrics: strings.Repeat("l", old-20)}})
	}
}

// iTunes' plain 2.4 sizes, the last frame's body with a run of zeros where its size read as syncsafe
// would end it: the zeros that reading takes for padding are looked at as far as the plain reading's
// frames go, so the frame is read, and saved, whole. That looking goes no further than id3ZeroScan.
func TestID3iTunesZeroRun(t *testing.T) {
	plain := func(id string, body []byte) []byte {
		h := make([]byte, 10)
		copy(h, id)
		binary.BigEndian.PutUint32(h[4:], uint32(len(body)))
		return append(h, body...)
	}
	for _, c := range []struct {
		name         string
		at, zeros, n int
	}{
		{"in the first bytes", 6144, 5856, 12288},          // 00 00 30 00 reads as 6144 syncsafe
		{"past the first bytes", 51200, 100 << 10, 200704}, // 00 03 10 00 reads as 51200
	} {
		priv := append([]byte("owner\x00"), bytes.Repeat([]byte{0x55}, c.at-6)...)
		priv = append(priv, make([]byte, c.zeros)...)
		priv = append(priv, bytes.Repeat([]byte{0x66}, c.n-len(priv))...)
		p := mpegFixture(t, "it.mp3", rawTag(4, 0, 1024, plain("TIT2", lat("Plain sizes")), plain("PRIV", priv)),
			mpegAudio(4000))
		f, _ := os.Open(p)
		m, err := readMPEG(f, size(t, p))
		f.Close()
		if err != nil || ids(m.tag) != "TIT2 PRIV" || m.tag.frames[1].size() != int64(len(priv)) || len(m.tag.notes()) != 0 {
			t.Fatalf("%s: %v, frames %s, notes %v", c.name, err, ids(m.tag), m.tag.notes())
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "Plain sizez"}}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		tag := mustParse(t, b[:id3Len(b)])
		if ids(tag) != "TIT2 PRIV" || !bytes.Equal(tag.frames[1].data, priv) || tag.fields()[Title] != "Plain sizez" {
			t.Fatalf("%s: saved frames %s", c.name, ids(tag))
		}
	}
	// A 1 MB frame, syncsafe, whose size read as a plain number (4 MB) ends inside 8 MB of padding:
	// the zeros between are looked at up to id3ZeroScan, and the syncsafe reading kept.
	big := append([]byte("owner\x00"), id3Noise(1<<20-6)...)
	data := append(rawTag(4, 0, 8<<20, rawFrame(4, "TIT2", utf8z("x")), rawFrame(4, "PRIV", big)), mpegAudio(4000)...)
	c := &id3CountRA{r: bytes.NewReader(data)}
	m, err := readMPEG(c, int64(len(data)))
	if err != nil || ids(m.tag) != "TIT2 PRIV" || m.tag.frames[1].size() != int64(len(big)) || c.n > id3Pre+id3ZeroScan+64<<10 {
		t.Fatalf("%v, frames %s, %d KB read", err, ids(m.tag), c.n>>10)
	}
}

// An iTunes-made 2.2 tag whose cover is past the first 64 KB is saved in place as 2.3, frames in
// their order, though every header grows: the writing holds the few bytes the cover moves by, not the
// cover. So is one unsynchronised (its cover too big to show, so reading doesn't undo it in memory),
// and one in an AIFF or WAV chunk.
func TestID3v22InPlace(t *testing.T) {
	norm := langBody("eng", "iTunNORM", " 00000A3B 000009E4 0000507C 0000452F")
	frames := func(img []byte) [][]byte {
		return [][]byte{rawFrame22("TT2", lat("Old")), rawFrame22("TP1", lat("Artist")), rawFrame22("TAL", lat("Album")),
			rawFrame22("TRK", lat("3/12")), rawFrame22("TYE", lat("2004")), rawFrame22("COM", norm),
			rawFrame22("PIC", append([]byte{0, 'J', 'P', 'G', 3, 0}, img...))}
	}
	img := append(append([]byte(nil), testJPEG...), id3Noise(2<<20)...)
	big := append(append([]byte(nil), testJPEG...), id3Noise(MaxPicture+1)...)
	for _, c := range []struct {
		name, kind string
		tag, img   []byte
	}{
		{"mp3", "mp3", rawTag(2, 0, 2048, frames(img)...), img},
		{"mp3 unsynchronised", "mp3", unsyncTag(2, 2048, frames(big)...), big},
		{"aiff", "aiff", rawTag(2, 0, 2048, frames(img)...), img},
		{"wav", "wav", rawTag(2, 0, 2048, frames(img)...), img},
	} {
		audio := mpegAudio(4000)
		p := mpegFixture(t, "v22.mp3", c.tag, audio)
		if c.kind != "mp3" {
			p = iffWithTag(t, c.kind, c.tag, audio)
		}
		st, _ := os.Stat(p)
		var err error
		if n := id3Allocs(func() { err = Write(p, Changes{Set: map[Field]string{Title: "A longer title"}}) }); err != nil || n > 1<<20 {
			t.Fatalf("%s: %v, %d KB allocated", c.name, err, n>>10)
		}
		if now, _ := os.Stat(p); !os.SameFile(st, now) || now.Size() != st.Size() {
			t.Fatalf("%s: not in place", c.name)
		}
		info, err := Read(p)
		want := map[Field]string{Title: "A longer title", Artist: "Artist", Album: "Album", Track: "3", TrackTotal: "12", Date: "2004"}
		if err != nil || !strings.Contains(info.Format, "ID3v2.3") || !reflect.DeepEqual(info.Fields, want) || len(info.Notes) != 0 {
			t.Fatalf("%s: %v %#v", c.name, err, info)
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, c.img) {
			t.Fatalf("%s: the cover changed (%v)", c.name, err)
		}
		b, _ := os.ReadFile(p)
		at := bytes.Index(b, []byte("ID3\x03"))
		tag := mustParse(t, b[at:at+int(id3Len(b[at:]))])
		if ids(tag) != "TIT2 TPE1 TALB TRCK TYER COMM APIC" || !bytes.Equal(tag.frames[5].data, norm) {
			t.Fatalf("%s: frames %s", c.name, ids(tag))
		}
	}
}

// id3FF is n bytes of noise with many 0xFF in them, some before a byte unsynchronisation stuffs.
func id3FF(rnd *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rnd.Read(b)
	for i := 0; i < n; i += 1 + rnd.Intn(40) {
		b[i] = 0xFF
		if i+1 < n && rnd.Intn(2) == 0 {
			b[i+1] = byte(0xE0 + rnd.Intn(32))
		}
	}
	return b
}

// id3Big is a big frame id3Shape made: a picture's image, else the whole body.
type id3Big struct {
	pic  bool
	data []byte
}

// id3Shape is a random tag: its version, small frames and big ones (left in the file) of sizes around
// where reading and writing go 64 KB at a time, unsynchronised whole or by frame, frame flags and
// padding; and its big frames.
func id3Shape(rnd *rand.Rand) ([]byte, []id3Big) {
	major := byte(3 + rnd.Intn(2))
	if rnd.Intn(5) == 0 {
		major = 2
	}
	id22 := map[string]string{"TIT2": "TT2", "TALB": "TAL", "COMM": "COM", "USLT": "ULT", "TXXX": "TXX", "APIC": "PIC", "GEOB": "GEO"}
	var frames [][]byte
	var bigs []id3Big
	add := func(id string, body []byte, fm byte) {
		if major == 2 {
			frames = append(frames, rawFrame22(id22[id], body))
		} else {
			frames = append(frames, rawFrame(major, id, body, 0, fm))
		}
	}
	for n := 2 + rnd.Intn(6); n > 0; n-- {
		switch rnd.Intn(8) {
		case 0:
			add("TIT2", lat(strings.Repeat("t", 1+rnd.Intn(60))), 0)
		case 1:
			add("COMM", langBody("eng", "iTunSMPB", " 00000000 00000210 0000096C"), 0)
		case 2:
			add("TXXX", txxxBody("REPLAYGAIN_TRACK_GAIN", "-6.5 dB"), 0)
		case 3:
			add("USLT", langBody("eng", "", strings.Repeat("ly ", rnd.Intn(3000))), 0)
		case 4:
			add("TALB", lat(strings.Repeat("a", 1+rnd.Intn(20))), 0)
		default:
			sizes := []int{3 << 10, 60 << 10, 64 << 10, 70 << 10, 130 << 10}
			img := append(append([]byte(nil), testJPEG...), id3FF(rnd, sizes[rnd.Intn(len(sizes))]+rnd.Intn(5000))...)
			id, body := "GEOB", append([]byte("own\x00"), img...)
			big := id3Big{false, body}
			if rnd.Intn(2) == 0 {
				id, body, big = "APIC", apicBody("image/jpeg", 3, "", img), id3Big{true, img}
				if major == 2 {
					body = append([]byte{1, 'J', 'P', 'G', 3, 0xFF, 0xFE, 'c', 0, 0, 0}, img...)
				}
			}
			bigs = append(bigs, big)
			fm := byte(0)
			switch {
			case major == 4 && rnd.Intn(4) == 1:
				fm, body = 0x02, unsyncAll(body)
			case major == 4 && rnd.Intn(3) == 1:
				dl := make([]byte, 4)
				id3PutSyncsafe(dl, int64(len(body)))
				fm, body = 0x43, unsyncAll(append(append([]byte{0xFF}, dl...), body...))
			case major > 2 && rnd.Intn(3) == 1:
				fm, body = 0x40, append([]byte{byte(rnd.Intn(256))}, body...)
				if major == 3 {
					fm = 0x20
				}
			}
			add(id, body, fm)
		}
	}
	pad := []int{0, 20, 300, 2048, 4096, 70000}[rnd.Intn(6)] + rnd.Intn(300)
	if major < 4 && rnd.Intn(3) == 0 {
		return unsyncTag(major, pad, frames...), bigs
	}
	return rawTag(major, 0, pad, frames...), bigs
}

// A save in place writes the very bytes the same tag would be written as elsewhere: what the writing
// overtakes, of the frames it copies from the file, is read before it is written over. Tags of every
// shape, edited every way; the big frames come back whole. Each is written in place a second time
// with nothing buffering the writing, so a byte overtaken shows at once.
func TestID3InPlaceSameAsWritten(t *testing.T) {
	rnd := rand.New(rand.NewSource(11))
	audio := append(mpegAudio(3000), v1Tag("v1", "", "", "1999", "", 0, 255)...)
	dir := t.TempDir()
	p, q, r := filepath.Join(dir, "a.mp3"), filepath.Join(dir, "b.mp3"), filepath.Join(dir, "c.mp3")
	// unbuffered writes tg over the tag of r in place as patchTo does, each byte as it comes.
	unbuffered := func(tg *id3Tag, room int64) ([]byte, error) {
		src, err := os.Open(r)
		if err != nil {
			return nil, err
		}
		defer src.Close()
		dst, err := os.OpenFile(r, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		n := min(room, max(tg.needed(), tg.used)+id3ZeroCheck)
		err = tg.write(tg.ahead(&atWriter{f: dst, end: n}, src, 0), room, n)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		b, _ := os.ReadFile(r)
		return b, err
	}
	inPlace, moved := 0, 0
	for i := 0; i < 200; i++ {
		raw, bigs := id3Shape(rnd)
		c := Changes{Set: map[Field]string{}}
		if rnd.Intn(2) == 0 {
			c.Set[Title] = strings.Repeat("T", rnd.Intn(80))
		}
		if rnd.Intn(3) == 0 {
			c.Set[Album] = strings.Repeat("Ä", rnd.Intn(30))
		}
		if rnd.Intn(3) == 0 {
			c.Set[Lyrics] = strings.Repeat("la\n", rnd.Intn(2000))
		}
		if rnd.Intn(4) == 0 {
			c.Set[Track] = fmt.Sprint(1 + rnd.Intn(30))
		}
		switch rnd.Intn(6) {
		case 0:
			c.RemovePicture = true
		case 1:
			c.Picture = &Picture{MIME: "image/jpeg", Type: 3, Data: testJPEG}
		}
		orig := append(append([]byte(nil), raw...), audio...)
		if os.WriteFile(p, orig, 0o644) != nil || os.WriteFile(q, orig, 0o644) != nil || os.WriteFile(r, orig, 0o644) != nil {
			t.Fatal("can't write")
		}
		// What the save should write: the same tag, read from the copy, laid out as fits lays it and
		// written out of place.
		f, err := os.Open(q)
		if err != nil {
			t.Fatal(err)
		}
		m, err := readMPEG(f, int64(len(orig)))
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		tg := m.tag
		tg.apply(c)
		fit := tg.fits(m.tagLen)
		want := tg.encode(int(m.tagLen))
		if !fit {
			want = tg.encode(int(tg.needed() + mpegSpare))
		}
		f.Close()
		if fit {
			if tg.lag(tg.frames) > 0 {
				moved++
			}
			if b, err := unbuffered(tg, m.tagLen); err != nil || !bytes.Equal(b[:m.tagLen], want) || !bytes.Equal(b[m.tagLen:], orig[m.tagLen:]) {
				t.Fatalf("%d: written unbuffered (%v), not as written elsewhere", i, err)
			}
		}
		st, _ := os.Stat(p)
		if err := (mpegCodec{"MP3"}).write(p, c); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		now, _ := os.Stat(p)
		b, _ := os.ReadFile(p)
		n := id3Len(b)
		if fit != (os.SameFile(st, now) && now.Size() == st.Size()) || !bytes.Equal(b[:n], want) ||
			!bytes.Equal(b[n:len(b)-128], orig[m.tagLen:len(orig)-128]) {
			t.Fatalf("%d: in place %v, not as written elsewhere, or the audio changed", i, fit)
		}
		if fit {
			inPlace++
		}
		got := mustParse(t, b[:n])
		var have [][]byte
		for _, fr := range got.frames {
			if fr.id == "GEOB" {
				have = append(have, fr.data)
			} else if _, _, _, at, ok := id3APIC(fr.data); fr.id == "APIC" && ok {
				have = append(have, fr.data[at:])
			}
		}
		for _, big := range bigs {
			if big.pic && (c.Picture != nil || c.RemovePicture) {
				continue
			}
			k := 0
			for k < len(have) && !bytes.Equal(have[k], big.data) {
				k++
			}
			if k == len(have) {
				t.Fatalf("%d: a big frame of %d bytes lost", i, len(big.data))
			}
			have = append(have[:k], have[k+1:]...)
		}
	}
	if inPlace < 100 || moved < 20 {
		t.Fatalf("only %d of 200 in place, %d of them moving a frame towards the end", inPlace, moved)
	}
}
