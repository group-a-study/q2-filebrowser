package tags

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mpegAudio is n bytes standing in for MPEG frames.
func mpegAudio(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	copy(b, []byte{0xFF, 0xFB, 0x90, 0x64})
	return b
}

// v1Tag is an ID3v1 tag; a track makes it ID3v1.1.
func v1Tag(title, artist, album, year, comment string, track, genre byte) []byte {
	b := make([]byte, 128)
	copy(b, "TAG")
	copy(b[3:33], title)
	copy(b[33:63], artist)
	copy(b[63:93], album)
	copy(b[93:97], year)
	copy(b[97:127], comment)
	if track > 0 {
		b[125], b[126] = 0, track
	}
	b[127] = genre
	return b
}

func mpegFixture(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, bytes.Join(parts, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mpegParts are a file's ID3v2 tag, what follows it up to the ID3v1 tag, and that tag.
func mpegParts(t *testing.T, p string) (tag *id3Tag, tagLen int64, audio, v1 []byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readMPEG(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return m.tag, m.tagLen, b[m.tagLen : len(b)-len(m.v1)], m.v1
}

func size(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestMP3InPlace(t *testing.T) {
	smpb := rawFrame(3, "COMM", langBody("eng", "iTunSMPB", " 00000000 00000210 0000096C"))
	audio := mpegAudio(20000)
	p := mpegFixture(t, "a.mp3", rawTag(3, 0, 4096, rawFrame(3, "TIT2", lat("Old")), smpb, rawFrame(3, "TPE1", lat("A"))),
		audio, v1Tag("Old", "A", "", "1999", "a comment thirty letters long", 0, 17))
	before := size(t, p)
	err := Write(p, Changes{Set: map[Field]string{Title: "Néw ★", Artist: "", Comment: "hello", Track: "7",
		Genre: "Synthpop"}})
	if err != nil {
		t.Fatal(err)
	}
	if after := size(t, p); after != before {
		t.Fatalf("size %d → %d: not in place", before, after)
	}
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "MP3 (ID3v2.3)" || !info.Writable || info.Fields[Title] != "Néw ★" || info.Fields[Artist] != "" ||
		info.Fields[Comment] != "hello" || info.Fields[Track] != "7" || info.Fields[Genre] != "Synthpop" {
		t.Fatalf("info %#v", info)
	}
	tag, _, got, v1 := mpegParts(t, p)
	if !bytes.Equal(got, audio) {
		t.Fatal("audio changed")
	}
	if ids(tag) != "TIT2 COMM TRCK TCON COMM" {
		t.Fatalf("frames %s", ids(tag))
	}
	b, _ := os.ReadFile(p)
	if i := bytes.Index(b, smpb); i != 10+10+len(tag.frames[0].data) {
		t.Fatalf("iTunSMPB at %d", i)
	}
	want := v1Tag("N\xe9w ?", "", "", "1999", "hello", 7, byte(len(genres)-1))
	if !bytes.Equal(v1, want) {
		t.Fatalf("ID3v1\n%q\nwant\n%q", v1, want)
	}
}

func TestMP3Rewrite(t *testing.T) {
	audio := mpegAudio(50000)
	p := mpegFixture(t, "b.mp3", rawTag(4, 0, 0, rawFrame(4, "TIT2", utf8z("Old"))), audio)
	if err := Write(p, Changes{Set: map[Field]string{Album: "Álbum"}, Picture: &Picture{Data: testJPEG}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "MP3 (ID3v2.4)" || info.Fields[Title] != "Old" || info.Fields[Album] != "Álbum" ||
		info.Picture == nil || info.Picture.Width != 60 || info.Picture.Height != 50 || info.Picture.Size != len(testJPEG) {
		t.Fatalf("info %#v %#v", info, info.Picture)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, testJPEG) || pic.Type != 3 {
		t.Fatalf("picture %v", err)
	}
	tag, tagLen, got, _ := mpegParts(t, p)
	if !bytes.Equal(got, audio) {
		t.Fatal("audio changed")
	}
	if tagLen-tag.needed() != mpegSpare || size(t, p) != tagLen+int64(len(audio)) {
		t.Fatalf("tag %d, needed %d", tagLen, tag.needed())
	}
	// The padding takes the next change in place.
	before := size(t, p)
	if err := Write(p, Changes{Set: map[Field]string{Artist: "Someone"}, RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != before {
		t.Fatal("not in place")
	}
	if info, _ := Read(p); info.Picture != nil || info.Fields[Artist] != "Someone" {
		t.Fatalf("info %#v", info)
	}
}

func TestMP3NoTag(t *testing.T) {
	audio := mpegAudio(3000)
	p := mpegFixture(t, "c.mp3", audio)
	info, err := Read(p)
	if err != nil || info.Format != "MP3 (no tag yet)" || !info.Writable || len(info.Fields) != 0 {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "First"}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := Read(p); info.Format != "MP3 (ID3v2.4)" || info.Fields[Title] != "First" {
		t.Fatalf("%#v", info)
	}
	tag, tagLen, got, _ := mpegParts(t, p)
	if !bytes.Equal(got, audio) || tagLen != tag.needed()+mpegSpare {
		t.Fatal("audio changed, or no padding")
	}
}

func TestMP3V1Only(t *testing.T) {
	audio := mpegAudio(3000)
	v1 := v1Tag("Song", "Band", "", "2001", "", 5, 17)
	p := mpegFixture(t, "d.mp3", audio, v1)
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "MP3 (ID3v1)" || info.Fields[Title] != "Song" || info.Fields[Track] != "5" ||
		info.Fields[Genre] != "Rock" || info.Fields[Date] != "2001" || len(info.Notes) != 1 {
		t.Fatalf("%#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Album: "Record"}}); err != nil {
		t.Fatal(err)
	}
	info, _ = Read(p)
	if info.Format != "MP3 (ID3v2.4)" || info.Fields[Title] != "Song" || info.Fields[Artist] != "Band" ||
		info.Fields[Album] != "Record" || info.Fields[Track] != "5" || info.Fields[Genre] != "Rock" || info.Fields[Date] != "2001" {
		t.Fatalf("%#v", info.Fields)
	}
	_, _, got, nv1 := mpegParts(t, p)
	if !bytes.Equal(got, audio) || !bytes.Equal(nv1, v1Tag("Song", "Band", "Record", "2001", "", 5, 17)) {
		t.Fatalf("audio changed, or ID3v1 %q", nv1)
	}
}

func TestID3v1Update(t *testing.T) {
	old := v1Tag("t", "a", "al", "1999", "", 3, 0)
	b := id3v1Update(old, map[Field]string{Title: "日本語 Ok", Album: strings.Repeat("abcdefghij", 4), Artist: "",
		Genre: "Polka-ish", Track: "", Date: ""})
	want := v1Tag("??? Ok", "", strings.Repeat("abcdefghij", 3), "", "", 0, 255)
	if !bytes.Equal(b, want) {
		t.Fatalf("\n%q\nwant\n%q", b, want)
	}
	// Read as UTF-8 when it is that (ffmpeg's), else as Latin-1.
	if f := id3v1Fields(v1Tag("T\xc3\xabst", "\xc4rtist", "", "", "", 0, 255)); f[Title] != "Tëst" || f[Artist] != "Ärtist" {
		t.Fatalf("%#v", f)
	}
	// A track on an ID3v1.0 tag cuts its comment to 28.
	old = v1Tag("", "", "", "", strings.Repeat("c", 30), 0, 255)
	b = id3v1Update(old, map[Field]string{Track: "12"})
	if f := id3v1Fields(b); f[Track] != "12" || f[Comment] != strings.Repeat("c", 28) {
		t.Fatalf("%#v", f)
	}
	b = id3v1Update(b, map[Field]string{Comment: strings.Repeat("d", 40), Genre: "rock"})
	if f := id3v1Fields(b); f[Track] != "12" || f[Comment] != strings.Repeat("d", 28) || f[Genre] != "Rock" {
		t.Fatalf("%#v", f)
	}
	b = id3v1Update(b, map[Field]string{Track: "", Comment: strings.Repeat("e", 40)})
	if f := id3v1Fields(b); f[Track] != "" || f[Comment] != strings.Repeat("e", 30) {
		t.Fatalf("%#v", f)
	}
}

func TestMP3OtherTags(t *testing.T) {
	second := rawTag(3, 0, 0, rawFrame(3, "TIT2", lat("Second")))
	ape := append([]byte("APETAGEX"), make([]byte, 24)...)
	audio := mpegAudio(1000)
	p := mpegFixture(t, "e.aac", rawTag(3, 0, 0, rawFrame(3, "TIT2", lat("First"))), second, audio, ape,
		v1Tag("First", "", "", "", "", 0, 255))
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(info.Notes, "|")
	if info.Format != "AAC (ID3v2.3)" || info.Fields[Title] != "First" || !strings.Contains(notes, "second ID3v2") ||
		!strings.Contains(notes, "APEv2") {
		t.Fatalf("%#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Changed"}}); err != nil {
		t.Fatal(err)
	}
	_, _, rest, v1 := mpegParts(t, p)
	if !bytes.Equal(rest, bytes.Join([][]byte{second, audio, ape}, nil)) || id3v1Fields(v1)[Title] != "Changed" {
		t.Fatal("the second tag, the audio or the APE tag changed")
	}
}

func TestMP3NotMPEG(t *testing.T) {
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), mpegAudio(500)...)
	p := mpegFixture(t, "f.mp3", wav)
	info, err := Read(p)
	if err != nil || info.Writable || len(info.Notes) != 1 || !strings.Contains(info.Notes[0], "WAV") {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrReadOnly {
		t.Fatalf("written: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, wav) {
		t.Fatal("changed")
	}
	// A tag that says it is longer than the file.
	p = mpegFixture(t, "g.mp3", []byte("ID3\x03\x00\x00\x00\x00\x7f\x7f"), mpegAudio(100))
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("read: %v", err)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrCorrupt {
		t.Fatalf("written: %v", err)
	}
}

func TestMP3BigPicture(t *testing.T) {
	// A cover bigger than the part of the tag read at once is left in the file, not read, until it's
	// written again.
	big := append(append([]byte(nil), testJPEG...), bytes.Repeat([]byte{0xAB}, 300<<10)...)
	audio := mpegAudio(5000)
	p := mpegFixture(t, "h.mp3", rawTag(3, 0, 1000, rawFrame(3, "APIC", apicBody("image/jpeg", 3, "", big)),
		rawFrame(3, "TIT2", lat("Old"))), audio)
	f, _ := os.Open(p)
	m, err := readMPEG(f, size(t, p))
	f.Close()
	if err != nil || !m.tag.frames[0].lazy || m.tag.frames[1].lazy || m.tag.fields()[Title] != "Old" {
		t.Fatalf("%v %#v", err, m.tag.frames)
	}
	info, err := Read(p)
	if err != nil || info.Picture == nil || info.Picture.Size != len(big) || info.Picture.Width != 60 {
		t.Fatalf("%v %#v", err, info)
	}
	check := func(what string) {
		t.Helper()
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, big) {
			t.Fatalf("%s: picture lost (%v)", what, err)
		}
		if _, _, got, _ := mpegParts(t, p); !bytes.Equal(got, audio) {
			t.Fatalf("%s: audio changed", what)
		}
	}
	before := size(t, p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != before {
		t.Fatal("not in place")
	}
	check("in place")
	if err := Write(p, Changes{Set: map[Field]string{Lyrics: strings.Repeat("la la\n", 400)}}); err != nil {
		t.Fatal(err)
	}
	if size(t, p) == before {
		t.Fatal("in place")
	}
	check("written again")
	if info, _ := Read(p); info.Fields[Title] != "New" || !strings.HasPrefix(info.Fields[Lyrics], "la la\nla la") {
		t.Fatalf("%#v", info.Fields)
	}
}

func TestMP3InPlaceStreams(t *testing.T) {
	// Saving in place copies a big cover where it goes a piece at a time, never holding it: a longer
	// title stays before the cover, which moves towards the end by what the title grew, and an early
	// iTunSMPB stays early.
	cover := append(append([]byte(nil), testJPEG...), id3Noise(8<<20)...)
	smpb := rawFrame(4, "COMM", langBody("eng", "iTunSMPB", " 00000000 00000210 0000096C"))
	audio := mpegAudio(4000)
	p := mpegFixture(t, "big.mp3", rawTag(4, 0, 4096, rawFrame(4, "TIT2", utf8z("Big")), smpb,
		rawFrame(4, "APIC", apicBody("image/jpeg", 3, "", cover))), audio)
	st, _ := os.Stat(p)
	write := func(c Changes, frames string) {
		t.Helper()
		var err error
		if n := id3Allocs(func() { err = Write(p, c) }); err != nil || n > 1<<20 {
			t.Fatalf("%v, %d KB allocated", err, n>>10)
		}
		if now, _ := os.Stat(p); !os.SameFile(st, now) || now.Size() != st.Size() {
			t.Fatal("not in place")
		}
		tag, tagLen, got, _ := mpegParts(t, p)
		if ids(tag) != frames || !bytes.Equal(got, audio) || len(tag.notes()) != 0 {
			t.Fatalf("frames %s, notes %v, or the audio changed", ids(tag), tag.notes())
		}
		// Past the frames, zeros to the end of the room.
		b, _ := os.ReadFile(p)
		if !bytes.Equal(b[tag.needed():tagLen], make([]byte, tagLen-tag.needed())) {
			t.Fatal("not zeros after the frames")
		}
	}
	write(Changes{Set: map[Field]string{Title: "Changed"}}, "TIT2 COMM APIC")
	if b, _ := os.ReadFile(p); bytes.Index(b, smpb) != 10+10+1+len("Changed") {
		t.Fatal("iTunSMPB moved on")
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, cover) {
		t.Fatalf("picture lost (%v)", err)
	}
	write(Changes{Set: map[Field]string{Title: "Short", Album: "A"}}, "TIT2 COMM TALB APIC")
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, cover) {
		t.Fatalf("picture lost (%v)", err)
	}
	// Removing the cover leaves its room as padding; a later edit doesn't hold that either.
	write(Changes{RemovePicture: true}, "TIT2 COMM TALB")
	write(Changes{Set: map[Field]string{Title: "Again"}}, "TIT2 COMM TALB")
	if info, _ := Read(p); info.Picture != nil || info.Fields[Title] != "Again" || info.Fields[Album] != "A" {
		t.Fatalf("%#v", info)
	}
}

func TestID3v1CutUTF8(t *testing.T) {
	// ffmpeg cuts a long UTF-8 value by bytes: the character cut short is dropped, not read as Latin-1.
	cut := string([]byte("a日本語のタイトルです。長い")[:30])
	if f := id3v1Fields(v1Tag(cut, "", "", "", "", 0, 255)); f[Title] != "a日本語のタイトルで" {
		t.Fatalf("%q", f[Title])
	}
	// Latin-1 that ends in an accent is still Latin-1.
	for in, want := range map[string]string{"Caf\xe9": "Café", strings.Repeat("x", 29) + "\xe9": strings.Repeat("x", 29) + "é",
		strings.Repeat("z", 28) + "\xc3\xc3": strings.Repeat("z", 28) + "ÃÃ", strings.Repeat("y", 28) + "é": strings.Repeat("y", 28) + "é"} {
		if f := id3v1Fields(v1Tag(in, "", "", "", "", 0, 255)); f[Title] != want {
			t.Fatalf("%q: %q, want %q", in, f[Title], want)
		}
	}
	// The first save starts the new ID3v2 tag with the value as read.
	p := mpegFixture(t, "v1.mp3", mpegAudio(3000), v1Tag(cut, "", "", "2001", "", 0, 255))
	if err := Write(p, Changes{Set: map[Field]string{Artist: "Y"}}); err != nil {
		t.Fatal(err)
	}
	if tag, _, _, _ := mpegParts(t, p); tag == nil || tag.text("TIT2") != "a日本語のタイトルで" || tag.text("TPE1") != "Y" {
		t.Fatalf("%#v", tag)
	}
}

func TestMP3InPlaceManyCovers(t *testing.T) {
	// Three big covers and 4 KB of padding: a title edit, a new cover in their place, then another
	// edit, each saved in place holding none of the covers left in the file; the old covers' room
	// becomes zeros.
	frames := [][]byte{rawFrame(3, "TIT2", lat("Old"))}
	var covers [][]byte
	for i := 0; i < 3; i++ {
		c := append(append([]byte(nil), testJPEG...), id3Noise(3<<20+i)...)
		covers = append(covers, c)
		frames = append(frames, rawFrame(3, "APIC", apicBody("image/jpeg", byte(3+i), fmt.Sprint(i), c)))
	}
	audio := mpegAudio(4000)
	p := mpegFixture(t, "three.mp3", rawTag(3, 0, 4096, frames...), audio)
	st, _ := os.Stat(p)
	cover := append(append([]byte(nil), testJPEG...), id3Noise(1<<20)...)
	for i, e := range []struct {
		c      Changes
		frames string
		pic    []byte
		most   uint64
	}{
		{Changes{Set: map[Field]string{Title: "A longer title"}}, "TIT2 APIC APIC APIC", covers[0], 1 << 20},
		{Changes{Picture: &Picture{MIME: "image/jpeg", Data: cover}}, "TIT2 APIC", cover, 1<<20 + 2*uint64(len(cover))},
		{Changes{Set: map[Field]string{Title: "Short"}}, "TIT2 APIC", cover, 1 << 20},
	} {
		var err error
		if n := id3Allocs(func() { err = Write(p, e.c) }); err != nil || n > e.most {
			t.Fatalf("%d: %v, %d KB allocated", i, err, n>>10)
		}
		if now, _ := os.Stat(p); !os.SameFile(st, now) || now.Size() != st.Size() {
			t.Fatalf("%d: not in place", i)
		}
		tag, tagLen, got, _ := mpegParts(t, p)
		if ids(tag) != e.frames || !bytes.Equal(got, audio) {
			t.Fatalf("%d: frames %s, or the audio changed", i, ids(tag))
		}
		if b, _ := os.ReadFile(p); !bytes.Equal(b[tag.needed():tagLen], make([]byte, tagLen-tag.needed())) {
			t.Fatalf("%d: not zeros after the frames", i)
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, e.pic) {
			t.Fatalf("%d: the cover isn't as it should be (%v)", i, err)
		}
	}
}

func TestMP3InPlaceStopping(t *testing.T) {
	// An in-place save doesn't start once the app is ending.
	p := mpegFixture(t, "s.mp3", rawTag(3, 0, 2048, rawFrame(3, "TIT2", lat("Old"))), mpegAudio(2000))
	old, _ := os.ReadFile(p)
	defer func(f func() bool) { Stopping = f }(Stopping)
	Stopping = func() bool { return true }
	if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != ErrStopped {
		t.Fatalf("saved: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, old) {
		t.Fatal("changed")
	}
}
