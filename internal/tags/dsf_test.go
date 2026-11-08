package tags

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// dsfMake is a DSF: its header, a stereo fmt chunk, a data chunk of audio, the tag (none when nil)
// and what follows it.
func dsfMake(audio, tag, after []byte) []byte {
	b := make([]byte, 92, 92+len(audio)+len(tag)+len(after))
	copy(b, "DSD ")
	binary.LittleEndian.PutUint64(b[4:], 28)
	copy(b[28:], "fmt ")
	binary.LittleEndian.PutUint64(b[32:], 52)
	for i, v := range []uint32{1, 0, 2, 2, 2822400, 1} {
		binary.LittleEndian.PutUint32(b[40+4*i:], v)
	}
	binary.LittleEndian.PutUint64(b[64:], uint64(len(audio)*4))
	binary.LittleEndian.PutUint32(b[72:], 4096)
	copy(b[80:], "data")
	binary.LittleEndian.PutUint64(b[84:], uint64(12+len(audio)))
	b = append(b, audio...)
	if tag != nil {
		binary.LittleEndian.PutUint64(b[20:], uint64(len(b)))
	}
	b = append(append(b, tag...), after...)
	binary.LittleEndian.PutUint64(b[12:], uint64(len(b)))
	return b
}

// dsfOpen is p's bytes and layout, which must be one that can be written, its length as its header says.
func dsfOpen(t *testing.T, p string) (*dsfFile, []byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	d, err := readDSF(bytes.NewReader(b), int64(len(b)))
	if err != nil || d.odd != "" || le64(b[12:]) != int64(len(b)) {
		t.Fatalf("layout %v %+v, length %d of %d", err, d, le64(b[12:]), len(b))
	}
	return d, b
}

func TestDSFInPlace(t *testing.T) {
	audio := iffAudio(8192)
	tag := rawTag(3, 0, 500, rawFrame(3, "TIT2", lat("Old")), rawFrame(3, "TALB", lat("Album")))
	p := iffTemp(t, "a.dsf", dsfMake(audio, tag, nil))
	old, _ := os.ReadFile(p)
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "DSF (ID3v2.3)" || !info.Writable || info.Fields[Title] != "Old" || info.Fields[Album] != "Album" {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Ново", Date: "2020"}}); err != nil {
		t.Fatal(err)
	}
	d, b := dsfOpen(t, p)
	if len(b) != len(old) || !bytes.Equal(b[:d.ptr], old[:d.ptr]) || d.tagLen != int64(len(tag)) {
		t.Fatal("not in place, or the header or audio changed")
	}
	info, _ = Read(p)
	if info.Fields[Title] != "Ново" || info.Fields[Date] != "2020" || info.Fields[Album] != "Album" {
		t.Fatalf("fields %#v", info.Fields)
	}
}

func TestDSFNoTag(t *testing.T) {
	audio := iffAudio(4096)
	p := iffTemp(t, "b.dsf", dsfMake(audio, nil, nil))
	old, _ := os.ReadFile(p)
	if info, _ := Read(p); info.Format != "DSF (no tag yet)" || !info.Writable {
		t.Fatalf("info %#v", info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "One", Artist: "Someone"}}); err != nil {
		t.Fatal(err)
	}
	d, b := dsfOpen(t, p)
	if d.ptr != int64(len(old)) || !bytes.Equal(b[28:d.ptr], old[28:]) || int64(len(b)) != d.ptr+d.tagLen ||
		d.tagLen != d.tag.needed()+dsfSpare {
		t.Fatalf("tag at %d, %d bytes", d.ptr, d.tagLen)
	}
	info, _ := Read(p)
	if info.Format != "DSF (ID3v2.4)" || info.Fields[Title] != "One" || info.Fields[Artist] != "Someone" {
		t.Fatalf("info %#v", info)
	}
	// A big picture: written again, the tag where it was.
	big := append(append([]byte(nil), testJPEG...), bytes.Repeat([]byte{1}, 100<<10)...)
	if err := Write(p, Changes{Picture: &Picture{Data: big}}); err != nil {
		t.Fatal(err)
	}
	d2, b := dsfOpen(t, p)
	if d2.ptr != d.ptr || !bytes.Equal(b[28:d.ptr], old[28:]) || int64(len(b)) != d2.ptr+d2.tagLen {
		t.Fatalf("tag at %d, %d bytes", d2.ptr, d2.tagLen)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, big) {
		t.Fatalf("picture %v", err)
	}
	n := len(b)
	if err := Write(p, Changes{RemovePicture: true, Set: map[Field]string{Artist: ""}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := Read(p); size(t, p) != int64(n) || info.Picture != nil || info.Fields[Artist] != "" || info.Fields[Title] != "One" {
		t.Fatalf("not in place, or %#v", info)
	}
}

func TestDSFAfterTag(t *testing.T) {
	audio := iffAudio(4096)
	tag := rawTag(4, 0, 0, rawFrame(4, "TIT2", utf8z("Old")))
	after := []byte("something after the tag")
	p := iffTemp(t, "c.dsf", dsfMake(audio, tag, after))
	if err := Write(p, Changes{Set: map[Field]string{Title: "A longer title"}}); err != nil {
		t.Fatal(err)
	}
	d, b := dsfOpen(t, p)
	if !bytes.Equal(b[92:d.ptr], audio) || !bytes.Equal(b[d.ptr+d.tagLen:], after) || d.tag.text("TIT2") != "A longer title" {
		t.Fatal("audio or what follows the tag changed")
	}
}

// A file cut short anywhere reads without a panic, and is never one to write.
func TestDSFCutShort(t *testing.T) {
	b := dsfMake(iffAudio(301), rawTag(4, 0, 20, rawFrame(4, "TIT2", utf8z("x"))), nil)
	for n := 0; n < len(b)-1; n++ {
		info, _, err := dsfCodec{}.read(bytes.NewReader(b[:n]), int64(n))
		if err == nil && info.Writable {
			t.Fatalf("cut to %d of %d: writable", n, len(b))
		}
	}
}

func TestDSFRefuses(t *testing.T) {
	audio := iffAudio(4096)
	tag := rawTag(4, 0, 10, rawFrame(4, "TIT2", utf8z("x")))
	inside := dsfMake(audio, tag, nil)
	binary.LittleEndian.PutUint64(inside[20:], 200)
	copy(inside[200:], tag)
	short := dsfMake(audio, nil, nil)
	short = short[:len(short)-100]
	junk := dsfMake(audio, []byte("not a tag at all"), nil)
	past := dsfMake(audio, nil, nil)
	binary.LittleEndian.PutUint64(past[20:], uint64(len(past)+10))
	for name, b := range map[string][]byte{"inside": inside, "short": short, "junk": junk, "past": past} {
		p := iffTemp(t, "x.dsf", b)
		info, err := Read(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Writable || len(info.Notes) != 1 || (name == "inside") != (info.Fields[Title] == "x") {
			t.Fatalf("%s: %#v", name, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "y"}}); err != ErrReadOnly {
			t.Fatalf("%s: write %v", name, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, b) {
			t.Fatalf("%s: changed", name)
		}
	}
	bad := dsfMake(audio, nil, nil)
	copy(bad[80:], "date")
	for _, b := range [][]byte{bad, []byte("DSD \x1c\x00\x00\x00"), []byte("DSD \x1d\x00\x00\x00\x00\x00\x00\x00" + string(make([]byte, 80)))} {
		if _, err := Read(iffTemp(t, "y.dsf", b)); err != ErrCorrupt {
			t.Fatalf("%q: %v", b[:8], err)
		}
	}
}
