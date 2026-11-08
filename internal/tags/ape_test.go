package tags

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// apeItemOf is an item: its value's length, flags, key, a NUL and the value.
func apeItemOf(key string, flags uint32, value []byte) []byte {
	h := make([]byte, 8)
	binary.LittleEndian.PutUint32(h, uint32(len(value)))
	binary.LittleEndian.PutUint32(h[4:], flags)
	return append(append(append(h, key...), 0), value...)
}

// apeTagOf is a tag of the items: APEv2 with a header, or APEv1 (version 1000) with a footer only.
func apeTagOf(version uint32, items ...[]byte) []byte {
	body := bytes.Join(items, nil)
	edge := func(flags uint32) []byte {
		b := make([]byte, 32)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], version)
		binary.LittleEndian.PutUint32(b[12:], uint32(len(body)+32))
		binary.LittleEndian.PutUint32(b[16:], uint32(len(items)))
		binary.LittleEndian.PutUint32(b[20:], flags)
		return b
	}
	if version == 1000 {
		return append(body, edge(0)...)
	}
	return append(append(edge(apeHasHeader|apeIsHeader), body...), edge(apeHasHeader)...)
}

// apeAudio is "MAC " and n bytes standing in for its header and frames.
func apeAudio(n int) []byte { return append([]byte("MAC \x96\x0f"), iffAudio(n)...) }

// apeOpen is p's bytes and tag, which must be sound and APEv2.
func apeOpen(t *testing.T, p string) (*apeFile, []byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	a, err := readAPE(bytes.NewReader(b), int64(len(b)))
	if err != nil || a.odd != "" || a.version != 2000 || a.length() != a.end-a.start {
		t.Fatalf("tag %v %+v", err, a)
	}
	return a, b
}

func apeKeysOf(a *apeFile) string {
	var out []string
	for _, it := range a.items {
		out = append(out, it.key)
	}
	return strings.Join(out, ",")
}

func TestAPE(t *testing.T) {
	audio := apeAudio(5000)
	gain := apeItemOf("REPLAYGAIN_TRACK_GAIN", 0, []byte("-6.20 dB"))
	tag := apeTagOf(2000, apeItemOf("Title", 0, []byte("Old")), apeItemOf("ARTIST", 0, []byte("One\x00Two")),
		gain, apeItemOf("ALBUM ARTIST", 0, []byte("Various")), apeItemOf("Track", 0, []byte("3/12")),
		apeItemOf("Year", 0, []byte("2004")), apeItemOf("Cover Art (Front)", apeBinary, append([]byte("front.jpg\x00"), testJPEG...)))
	v1 := v1Tag("Old", "One", "", "2004", "", 3, 255)
	p := iffTemp(t, "a.ape", bytes.Join([][]byte{audio, tag, v1}, nil))
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "APE (APEv2)" || !info.Writable || info.Fields[Title] != "Old" || info.Fields[Artist] != "One / Two" ||
		info.Fields[AlbumArtist] != "Various" || info.Fields[TrackTotal] != "12" || info.Fields[Date] != "2004" ||
		info.Picture == nil || info.Picture.Width != 60 || info.Picture.Size != len(testJPEG) {
		t.Fatalf("info %#v %#v", info, info.Picture)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, testJPEG) {
		t.Fatalf("picture %v", err)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Nëw", AlbumArtist: "Someone", TrackTotal: "13", Genre: "Jazz"}}); err != nil {
		t.Fatal(err)
	}
	a, b := apeOpen(t, p)
	if !bytes.Equal(b[:len(audio)], audio) || a.start != int64(len(audio)) || !bytes.Equal(b[len(b)-128:len(b)-125], []byte("TAG")) {
		t.Fatal("the audio moved or changed, or ID3v1 went")
	}
	if apeKeysOf(a) != "Title,ARTIST,REPLAYGAIN_TRACK_GAIN,Album Artist,Track,Year,Cover Art (Front),Genre" ||
		!bytes.Equal(b[a.items[2].at:a.items[2].off+a.items[2].n], gain) {
		t.Fatalf("items %s", apeKeysOf(a))
	}
	info, _ = Read(p)
	if info.Fields[Title] != "Nëw" || info.Fields[AlbumArtist] != "Someone" || info.Fields[Track] != "3" ||
		info.Fields[TrackTotal] != "13" || info.Fields[Genre] != "Jazz" || info.Picture == nil {
		t.Fatalf("fields %#v", info.Fields)
	}
	if f := id3v1Fields(b[len(b)-128:]); f[Title] != "Nëw" || f[Genre] != "Jazz" || f[Track] != "3" {
		t.Fatalf("ID3v1 %#v", f)
	}
	// A new picture replaces the old; removing it leaves none.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x10\x00\x00\x00\x10\x08\x02\x00\x00\x00")
	if err := Write(p, Changes{Picture: &Picture{Data: png}}); err != nil {
		t.Fatal(err)
	}
	if a, _ = apeOpen(t, p); !strings.HasSuffix(apeKeysOf(a), ",Genre,Cover Art (Front)") {
		t.Fatalf("items %s", apeKeysOf(a))
	}
	if pic, err := ReadPicture(p); err != nil || pic.MIME != "image/png" || !bytes.Equal(pic.Data, png) {
		t.Fatalf("picture %v", err)
	}
	if err := Write(p, Changes{RemovePicture: true, Set: map[Field]string{Track: ""}}); err != nil {
		t.Fatal(err)
	}
	if a, _ = apeOpen(t, p); apeKeysOf(a) != "Title,ARTIST,REPLAYGAIN_TRACK_GAIN,Album Artist,Year,Genre" {
		t.Fatalf("items %s", apeKeysOf(a))
	}
}

func TestAPENoTag(t *testing.T) {
	audio := apeAudio(3001)
	p := iffTemp(t, "b.ape", audio)
	if info, _ := Read(p); info.Format != "APE (no tag yet)" || !info.Writable {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "One", Disc: "2", Lyrics: "la\nla"}}); err != nil {
		t.Fatal(err)
	}
	a, b := apeOpen(t, p)
	if !bytes.Equal(b[:len(audio)], audio) || a.end != int64(len(b)) || apeKeysOf(a) != "Title,Disc,Lyrics" {
		t.Fatalf("items %s", apeKeysOf(a))
	}
	if info, _ := Read(p); info.Format != "APE (APEv2)" || info.Fields[Lyrics] != "la\nla" || info.Fields[Disc] != "2" {
		t.Fatalf("info %#v", info)
	}
}

func TestAPEOldTags(t *testing.T) {
	audio := apeAudio(100)
	// ID3v1 only: a new APEv2 tag starts with its fields.
	p := iffTemp(t, "c.ape", append(append([]byte(nil), audio...), v1Tag("V1 Title", "V1 Artist", "", "1999", "", 7, 8)...))
	if info, _ := Read(p); info.Format != "APE (ID3v1)" || info.Fields[Title] != "V1 Title" || info.Fields[Genre] != "Jazz" {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Album: "Record"}}); err != nil {
		t.Fatal(err)
	}
	a, b := apeOpen(t, p)
	if apeKeysOf(a) != "Title,Artist,Track,Year,Genre,Album" || a.end != int64(len(b)-128) {
		t.Fatalf("items %s", apeKeysOf(a))
	}
	// APEv1: read, and saved as APEv2.
	p = iffTemp(t, "d.ape", append(append([]byte(nil), audio...), apeTagOf(1000, apeItemOf("Title", 0, []byte("Caf\xe9")))...))
	if info, _ := Read(p); info.Format != "APE (APEv1)" || info.Fields[Title] != "Café" || len(info.Notes) != 1 {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Artist: "x"}}); err != nil {
		t.Fatal(err)
	}
	if a, b := apeOpen(t, p); apeKeysOf(a) != "Title,Artist" || !bytes.Equal(b[:len(audio)], audio) || string(a.items[0].value) != "Café" {
		t.Fatalf("items %s", apeKeysOf(a))
	}
}

func TestAPERefuses(t *testing.T) {
	tag := apeTagOf(2000, apeItemOf("Title", 0, []byte("x")))
	bad := append([]byte(nil), tag...)
	binary.LittleEndian.PutUint32(bad[len(bad)-16:], 5) // five items, one there
	for name, b := range map[string][]byte{
		"not MAC":   append([]byte("fLaC\x00\x00\x00\x22"), tag...),
		"damaged":   append(apeAudio(100), bad...),
		"too long":  append(apeAudio(10), tag[len(tag)-32:]...),
		"bad key":   append(apeAudio(100), apeTagOf(2000, apeItemOf("Ti\x01le", 0, []byte("x")))...),
		"bad flags": append(apeAudio(100), tag[:len(tag)-32]...),
	} {
		if name == "bad flags" {
			b = append(b, tag[:32]...) // a header where the footer goes
		}
		p := iffTemp(t, "x.ape", b)
		info, err := Read(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Writable || len(info.Notes) != 1 || name == "not MAC" && info.Fields[Title] != "x" {
			t.Fatalf("%s: %#v", name, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "y"}}); err != ErrReadOnly {
			t.Fatalf("%s: write %v", name, err)
		}
	}
	b := append(apeAudio(300), apeTagOf(2000, apeItemOf("Title", 0, []byte("x")), apeItemOf("Cover Art (Back)", apeBinary, append([]byte("b.jpg\x00"), testJPEG...)))...)
	for n := 0; n < len(b)-1; n++ {
		if info, _, err := (apeCodec{}).read(bytes.NewReader(b[:n]), int64(n)); err == nil && info.Format == "APE (APEv2)" {
			t.Fatalf("cut to %d of %d: read as whole", n, len(b))
		}
	}
	if info, err := Read(iffTemp(t, "z.ape", b)); err != nil || info.Picture == nil {
		t.Fatalf("a back cover alone: %v %#v", err, info)
	}
}

// apeFooter is a lone footer: version 2000, size and count as given, no header.
func apeFooter(size, count uint32) []byte {
	b := make([]byte, 32)
	copy(b, "APETAGEX")
	binary.LittleEndian.PutUint32(b[8:], 2000)
	binary.LittleEndian.PutUint32(b[12:], size)
	binary.LittleEndian.PutUint32(b[16:], count)
	return b
}

// A tag whose items don't fill it as its footer says is damaged, and a save leaves the file as it was:
// a footer's size past its items once had a save cut the end of the audio, and a count short of them
// drop the items past it.
func TestAPENotFilled(t *testing.T) {
	audio := apeAudio(4000)
	items := [][]byte{apeItemOf("Title", 0, []byte("T")), apeItemOf("Artist", 0, []byte("A")), apeItemOf("Album", 0, []byte("Keep Me"))}
	short := apeTagOf(2000, items...)
	binary.LittleEndian.PutUint32(short[len(short)-16:], 2) // count 2 of 3, footer and header
	binary.LittleEndian.PutUint32(short[16:], 2)
	long := apeTagOf(2000, items[:2]...)
	binary.LittleEndian.PutUint32(long[len(long)-20:], uint32(len(long)-32+200)) // 200 bytes past them
	header := apeTagOf(2000, items...)
	binary.LittleEndian.PutUint32(header[16:], 2) // the header's count isn't the footer's
	v1 := v1Tag("T", "A", "", "", "", 0, 255)
	for name, b := range map[string][]byte{
		"count 0, size 1000":  append(append([]byte(nil), audio...), apeFooter(1000, 0)...),
		"count 0, size 33":    append(append([]byte(nil), audio...), apeFooter(33, 0)...),
		"count 0, then ID3v1": bytes.Join([][]byte{audio, apeFooter(500, 0), v1}, nil),
		"count short":         append(append([]byte(nil), audio...), short...),
		"size long":           append(append([]byte(nil), audio...), long...),
		"header disagrees":    append(append([]byte(nil), audio...), header...),
	} {
		p := iffTemp(t, "a.ape", b)
		info, err := Read(p)
		if err != nil || info.Writable || len(info.Notes) != 1 || !strings.Contains(info.Notes[0], "damaged") {
			t.Fatalf("%s: %v %#v", name, err, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "Edited"}}); err != ErrReadOnly {
			t.Fatalf("%s: write %v", name, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
			t.Fatalf("%s: changed", name)
		}
	}
	// An empty tag (a footer alone, count 0) is a tag.
	b := append(append([]byte(nil), audio...), apeFooter(32, 0)...)
	p := iffTemp(t, "b.ape", b)
	if info, _ := Read(p); !info.Writable || info.Format != "APE (APEv2)" {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != nil {
		t.Fatal(err)
	}
	if a, after := apeOpen(t, p); !bytes.Equal(after[:len(audio)], audio) || a.start != int64(len(audio)) || apeKeysOf(a) != "Title" {
		t.Fatalf("items %s at %d", apeKeysOf(a), a.start)
	}
}

// Reading a tag costs a bounded number of items and of text: 16K empty items, or 10 MB of text, once
// cost megabytes (half a million items, or 40 MB of text, went past the Q2's 32 MB).
func TestAPEBounded(t *testing.T) {
	audio := apeAudio(2000)
	many := make([][]byte, 1<<14)
	for i := range many {
		many[i] = apeItemOf("K", 0, nil)
	}
	p := iffTemp(t, "many.ape", append(append([]byte(nil), audio...), apeTagOf(2000, many...)...))
	var info *Info
	if n := iffAllocs(func() { info, _ = Read(p) }); n > 1<<20 || info == nil || info.Writable {
		t.Fatalf("%d KB allocated, %#v", n>>10, info)
	}
	if info, _ := Read(iffTemp(t, "some.ape", append(append([]byte(nil), audio...), apeTagOf(2000, many[:apeMaxItems]...)...))); !info.Writable {
		t.Fatalf("%d items: %#v", apeMaxItems, info)
	}
	// Ten 1 MB texts no field is read from aren't read; ten a field falls back on are, until 4 MB of
	// text is (three of them here), the rest left in the file. A save keeps them all as they were.
	for _, key := range []string{"Notes", "UnsyncedLyrics"} {
		big := [][]byte{apeItemOf("Title", 0, []byte("Old")), apeItemOf("Lyrics", 0, []byte("la"))}
		for i := 0; i < 10; i++ {
			big = append(big, apeItemOf(key, 0, bytes.Repeat([]byte{'a' + byte(i)}, 1<<20)))
		}
		big = append(big, apeItemOf("Album", 0, []byte("Record")))
		p = iffTemp(t, "text.ape", append(append([]byte(nil), audio...), apeTagOf(2000, big...)...))
		most := uint64(1 << 20)
		if key != "Notes" {
			most = 4 << 20
		}
		n := iffAllocs(func() { info, _ = Read(p) })
		if n > most || info == nil || !info.Writable || info.Fields[Title] != "Old" || info.Fields[Lyrics] != "la" || info.Fields[Album] != "Record" {
			t.Fatalf("%s: %d KB allocated, or the fields wrong", key, n>>10)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != nil {
			t.Fatal(err)
		}
		a, b := apeOpen(t, p)
		if !bytes.Equal(b[:len(audio)], audio) || len(a.items) != len(big) {
			t.Fatalf("%s: %d items", key, len(a.items))
		}
		for i, it := range a.items[1:] {
			if !bytes.Equal(b[it.at:it.off+it.n], big[i+1]) {
				t.Fatalf("%s: item %d changed", key, i+1)
			}
		}
		if info, _ := Read(p); info.Fields[Title] != "New" || info.Fields[Album] != "Record" {
			t.Fatalf("%s: title %q", key, info.Fields[Title])
		}
	}
}

// apeValues is each item's value by key, the first of a key, as the file at b holds it.
func apeValues(a *apeFile, b []byte) map[string]string {
	out := map[string]string{}
	for _, it := range a.items {
		if _, ok := out[it.key]; !ok {
			out[it.key] = string(b[it.off : it.off+it.n])
		}
	}
	return out
}

// A save never makes a tag of more items than a read takes: one of apeMaxItems that a change would add
// an item to is refused and left as it was (a save once made 1025, and the next read called the app's
// own tag damaged); a change that adds none still saves, and one short of the cap saves to it.
func TestAPEItemCap(t *testing.T) {
	audio := apeAudio(2000)
	items := make([][]byte, apeMaxItems)
	items[0] = apeItemOf("Title", 0, []byte("Old"))
	for i := 1; i < len(items); i++ {
		items[i] = apeItemOf("K", 0, nil)
	}
	b := append(append([]byte(nil), audio...), apeTagOf(2000, items...)...)
	p := iffTemp(t, "cap.ape", b)
	if info, err := Read(p); err != nil || !info.Writable || info.Fields[Title] != "Old" {
		t.Fatalf("%v %#v", err, info)
	}
	for _, c := range []Changes{
		{Set: map[Field]string{Artist: "New"}},
		{Set: map[Field]string{Title: "New", Album: "New"}},
		{Picture: &Picture{Data: testJPEG}},
	} {
		if err := Write(p, c); err != ErrTooBig {
			t.Fatalf("%v: %v", c.Set, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
			t.Fatalf("%v: changed", c.Set)
		}
	}
	// A change of an item it has keeps the count, the items it keeps copied in one go: a copy each once
	// cost 32 KB apiece, 32 MB in all.
	var err error
	if n := iffAllocs(func() { err = Write(p, Changes{Set: map[Field]string{Title: "New"}}) }); err != nil || n > 8<<20 {
		t.Fatalf("%v, %d KB allocated", err, n>>10)
	}
	if a, after := apeOpen(t, p); len(a.items) != apeMaxItems || !bytes.Equal(after[:len(audio)], audio) {
		t.Fatalf("%d items", len(a.items))
	}
	if info, err := Read(p); err != nil || !info.Writable || info.Fields[Title] != "New" {
		t.Fatalf("%v %#v", err, info)
	}
	// One fewer: clearing the title, then a new field takes the last place.
	for _, c := range []Changes{{Set: map[Field]string{Title: ""}}, {Set: map[Field]string{Artist: "Saved"}}} {
		if err := Write(p, c); err != nil {
			t.Fatal(err)
		}
	}
	if a, _ := apeOpen(t, p); len(a.items) != apeMaxItems || a.items[len(a.items)-1].key != "Artist" {
		t.Fatalf("%d items, %s last", len(a.items), a.items[len(a.items)-1].key)
	}
	if info, err := Read(p); err != nil || !info.Writable || info.Fields[Artist] != "Saved" || info.Fields[Title] != "" {
		t.Fatalf("%v %#v", err, info)
	}
}

// The text budget goes to the shortest values first: four 1 MB field texts before a track number and a
// title once left those two unread, shown empty, and setting the track total then deleted the track.
func TestAPEShortTextsFirst(t *testing.T) {
	audio := apeAudio(2000)
	items := [][]byte{
		apeItemOf("Lyrics", 0, bytes.Repeat([]byte("l"), 1<<20)),
		apeItemOf("UnsyncedLyrics", 0, bytes.Repeat([]byte("u"), 1<<20)),
		apeItemOf("Comment", 0, bytes.Repeat([]byte("c"), 1<<20)),
		apeItemOf("Composer", 0, bytes.Repeat([]byte("m"), 1<<20)),
		apeItemOf("Track", 0, []byte("3")),
		apeItemOf("Title", 0, []byte("Song")),
	}
	b := append(append([]byte(nil), audio...), apeTagOf(2000, items...)...)
	p := iffTemp(t, "t.ape", b)
	// Still at most apeMaxTexts of text read: the two short ones and three of the long, the last left.
	a, err := readAPE(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	text := int64(0)
	for _, it := range a.items {
		if it.read {
			read, text = append(read, it.key), text+int64(len(it.value))
		}
	}
	if strings.Join(read, ",") != "Lyrics,UnsyncedLyrics,Comment,Track,Title" || text > apeMaxTexts {
		t.Fatalf("read %v, %d bytes", read, text)
	}
	info, err := Read(p)
	if err != nil || !info.Writable || info.Fields[Track] != "3" || info.Fields[Title] != "Song" ||
		info.Fields[Lyrics] != strings.Repeat("l", 1<<20) || info.Fields[Comment] != strings.Repeat("c", 1<<20) || info.Fields[Composer] != "" {
		t.Fatalf("%v: the fields wrong", err)
	}
	if err := Write(p, Changes{Set: map[Field]string{TrackTotal: "12"}}); err != nil {
		t.Fatal(err)
	}
	a, b = apeOpen(t, p)
	if apeKeysOf(a) != "Lyrics,UnsyncedLyrics,Comment,Composer,Track,Title" || !bytes.Equal(b[:len(audio)], audio) {
		t.Fatalf("items %s", apeKeysOf(a))
	}
	for i, it := range a.items {
		want := items[i]
		if it.key == "Track" {
			want = apeItemOf("Track", 0, []byte("3/12"))
		}
		if !bytes.Equal(b[it.at:it.off+it.n], want) {
			t.Fatalf("item %s changed", it.key)
		}
	}
	if info, _ := Read(p); info.Fields[Track] != "3" || info.Fields[TrackTotal] != "12" || info.Fields[Title] != "Song" {
		t.Fatalf("fields %q %q %q", info.Fields[Track], info.Fields[TrackTotal], info.Fields[Title])
	}
}

// An item never read (a text past apeMaxText, one past the text budget, or not text) goes only when its
// own field is set: setting the other half of a number leaves it as it was and writes that half in an
// item of its own, where it once deleted it.
func TestAPEUnreadKept(t *testing.T) {
	audio := apeAudio(2000)
	long := "3" + strings.Repeat(" ", apeMaxText) // past apeMaxText
	mb := "3" + strings.Repeat(" ", 1<<20-1)      // 1 MB, past the budget after four others
	big := func(key string) []byte { return apeItemOf(key, 0, bytes.Repeat([]byte{key[0]}, 1<<20)) }
	for _, tc := range []struct {
		name   string
		items  [][]byte
		set    map[Field]string
		keys   string
		values map[string]string // items' values after
		fields map[Field]string  // what a read shows after
	}{
		{"track too long, total set",
			[][]byte{apeItemOf("Title", 0, []byte("T")), apeItemOf("Track", 0, []byte(long)), apeItemOf("Album", 0, []byte("A"))},
			map[Field]string{TrackTotal: "12"}, "Title,Track,Album,TrackTotal",
			map[string]string{"Track": long, "TrackTotal": "12"}, map[Field]string{Title: "T", TrackTotal: "12", Album: "A"}},
		{"track past the budget, total set",
			[][]byte{big("Lyrics"), big("Comment"), big("Composer"), big("UnsyncedLyrics"), apeItemOf("TrackNumber", 0, []byte(mb))},
			map[Field]string{TrackTotal: "12"}, "Lyrics,Comment,Composer,UnsyncedLyrics,TrackNumber,TrackTotal",
			map[string]string{"TrackNumber": mb, "TrackTotal": "12"}, map[Field]string{TrackTotal: "12"}},
		{"total too long, track set",
			[][]byte{apeItemOf("Track", 0, []byte("3/10")), apeItemOf("TotalTracks", 0, []byte(long))},
			map[Field]string{Track: "5"}, "Track,TotalTracks",
			map[string]string{"Track": "5/10", "TotalTracks": long}, map[Field]string{Track: "5", TrackTotal: "10"}},
		{"disc not text, total set",
			[][]byte{apeItemOf("Disc", apeBinary, []byte("1/2")), apeItemOf("Title", 0, []byte("T"))},
			map[Field]string{DiscTotal: "3"}, "Disc,Title,DiscTotal",
			map[string]string{"Disc": "1/2", "DiscTotal": "3"}, map[Field]string{Title: "T", DiscTotal: "3"}},
		{"track too long, track set: replaced",
			[][]byte{apeItemOf("Track", 0, []byte(long)), apeItemOf("Title", 0, []byte("T"))},
			map[Field]string{Track: "7"}, "Track,Title",
			map[string]string{"Track": "7"}, map[Field]string{Track: "7", Title: "T"}},
		{"both read, total set",
			[][]byte{apeItemOf("Track", 0, []byte("3")), apeItemOf("TrackTotal", 0, []byte("10"))},
			map[Field]string{TrackTotal: "12"}, "Track",
			map[string]string{"Track": "3/12"}, map[Field]string{Track: "3", TrackTotal: "12"}},
		// A read number item holds the total ("3/10"): the new total goes into it, not into a TrackTotal
		// item its "/10" would hide, though another number item is too long to read.
		{"read track holds the total, another unread, total set",
			[][]byte{apeItemOf("Track", 0, []byte("3/10")), apeItemOf("TrackNumber", 0, []byte(long))},
			map[Field]string{TrackTotal: "12"}, "Track",
			map[string]string{"Track": "3/12"}, map[Field]string{Track: "3", TrackTotal: "12"}},
	} {
		p := iffTemp(t, "u.ape", append(append([]byte(nil), audio...), apeTagOf(2000, tc.items...)...))
		if err := Write(p, Changes{Set: tc.set}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		a, b := apeOpen(t, p)
		if apeKeysOf(a) != tc.keys || !bytes.Equal(b[:len(audio)], audio) {
			t.Fatalf("%s: items %s", tc.name, apeKeysOf(a))
		}
		got := apeValues(a, b)
		for k, v := range tc.values {
			if got[k] != v {
				t.Fatalf("%s: %s is %.20q, want %.20q", tc.name, k, got[k], v)
			}
		}
		info, err := Read(p)
		if err != nil || !info.Writable {
			t.Fatalf("%s: %v %#v", tc.name, err, info)
		}
		for _, f := range []Field{Title, Album, Track, TrackTotal, Disc, DiscTotal} {
			if info.Fields[f] != tc.fields[f] {
				t.Fatalf("%s: %s is %q, want %q", tc.name, f, info.Fields[f], tc.fields[f])
			}
		}
	}
}

// writeTo copies kept items side by side in one go, and only those: an item dropped, or a new one, between
// them splits the copy, and the tag is the one made from the items as they end up.
func TestAPEWriteRuns(t *testing.T) {
	var items [][]byte
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		items = append(items, apeItemOf(k, 0, []byte(k+k+k)))
	}
	b := append(apeAudio(100), apeTagOf(2000, items...)...)
	fresh := apeItemOf("New", 0, []byte("value"))
	for _, tc := range []struct {
		name string
		edit func(a *apeFile)
		want [][]byte
	}{
		{"none", func(a *apeFile) {}, items},
		{"middle dropped", func(a *apeFile) { a.items = append(a.items[:2], a.items[3:]...) },
			[][]byte{items[0], items[1], items[3], items[4]}},
		{"ends dropped", func(a *apeFile) { a.items = a.items[1:4] }, items[1:4]},
		{"new in the middle", func(a *apeFile) {
			it := &apeItem{key: "New", value: []byte("value"), read: true}
			a.items = append(a.items[:2], append([]*apeItem{it}, a.items[2:]...)...)
		}, [][]byte{items[0], items[1], fresh, items[2], items[3], items[4]}},
		{"reordered", func(a *apeFile) { a.items[1], a.items[3] = a.items[3], a.items[1] },
			[][]byte{items[0], items[3], items[2], items[1], items[4]}},
	} {
		a, err := readAPE(bytes.NewReader(b), int64(len(b)))
		if err != nil || len(a.items) != 5 {
			t.Fatalf("%v %d", err, len(a.items))
		}
		tc.edit(a)
		var out bytes.Buffer
		if err := a.writeTo(&out, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		if want := apeTagOf(2000, tc.want...); !bytes.Equal(out.Bytes(), want) || int64(out.Len()) != a.length() {
			t.Fatalf("%s: wrote\n%q\nwant\n%q", tc.name, out.Bytes(), want)
		}
	}
}
