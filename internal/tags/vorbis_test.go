package tags

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCommentsRoundTrip(t *testing.T) {
	c := &comments{vendor: "reference libFLAC 1.4.3", list: []string{"TITLE=One", "artist=A", "ARTIST=B",
		"TRACKNUMBER=3/12", "MQAENCODER=x", "ALBUM ARTIST=Old"}}
	got, n, err := parseComments(c.bytes())
	if err != nil || n != len(c.bytes()) || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %v %d %#v", err, n, got)
	}
	f := c.fields()
	want := map[Field]string{Title: "One", Artist: "A / B", Track: "3", TrackTotal: "12", AlbumArtist: "Old"}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("fields %#v", f)
	}
}

func TestCommentsApply(t *testing.T) {
	// TRACKNUM wins on the Q2, so it's the track; the total comes from TRACKNUMBER's "3/12".
	c := &comments{list: []string{"TITLE=One", "TRACKNUM=9", "TRACKNUMBER=3/12", "ALBUM ARTIST=Old",
		"YEAR=1999", "MQAENCODER=x"}}
	if v := c.fields(); v[Track] != "9" || v[TrackTotal] != "12" {
		t.Fatalf("fields %#v", v)
	}
	c.apply(map[Field]string{Track: "4", AlbumArtist: "New", Date: "", Title: "Two"})
	want := []string{"TITLE=Two", "TRACKNUMBER=4", "ALBUMARTIST=New", "MQAENCODER=x", "TRACKTOTAL=12"}
	if !reflect.DeepEqual(c.list, want) {
		t.Fatalf("list %#v", c.list)
	}
	c.apply(map[Field]string{TrackTotal: ""})
	if v := c.fields(); v[Track] != "4" || v[TrackTotal] != "" {
		t.Fatalf("total cleared: %#v", c.list)
	}
}

func TestPictureBlock(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 4, 0, 0, 0xFF, 0xC2, 0, 11, 8, 0, 20, 0, 30, 3, 0, 0, 0, 0, 0}
	b := encodePicture(&Picture{Type: 3, Data: jpeg})
	p, off, n, err := parsePicture(b)
	if err != nil || p.MIME != "image/jpeg" || p.Type != 3 || !bytes.Equal(b[off:off+n], jpeg) {
		t.Fatalf("%v %#v", err, p)
	}
	pi := pictureInfo(jpeg)
	if pi.Width != 30 || pi.Height != 20 || !pi.Progressive {
		t.Fatalf("info %#v", pi)
	}
	c := &comments{}
	c.setPicture(&Picture{Data: jpeg})
	if got := c.picture(); got == nil || !bytes.Equal(got.Data, jpeg) || got.hasOffset {
		t.Fatalf("comment picture %#v", got)
	}
	c.setPicture(nil)
	if len(c.list) != 0 {
		t.Fatal("not removed")
	}
	// Cut anywhere, a block doesn't read; its fields from its start say how much more they need.
	for i := 0; i < len(b); i++ {
		if _, _, _, err := parsePicture(b[:i]); err == nil {
			t.Fatalf("cut at %d read", i)
		}
	}
	if _, need, err := pictureFields(b[:10], int64(len(b))); err != nil || need != int64(off) {
		t.Fatalf("need %d %v, want %d", need, err, off)
	}
}

// The entries are read one at a time, from a reader as from bytes; a count past the limit, or past what
// the bytes could hold, is refused before anything is made for it.
func TestCommentsLimits(t *testing.T) {
	long := "LYRICS=" + strings.Repeat("la ", 40<<10)
	c := &comments{vendor: "v", list: []string{"TITLE=One", long, "ARTIST=A"}}
	b := c.bytes()
	if c.size() != int64(len(b)) {
		t.Fatalf("size %d, bytes %d", c.size(), len(b))
	}
	got, n, err := readComments(iotest.OneByteReader(bytes.NewReader(append(b, 1, 2, 3))), int64(len(b)+3))
	if err != nil || n != int64(len(b)) || !reflect.DeepEqual(got, c) {
		t.Fatalf("from a reader: %v %d", err, n)
	}
	if _, _, err := readComments(bytes.NewReader(b[:len(b)-1]), int64(len(b))); err != ErrCorrupt {
		t.Fatalf("short: %v", err)
	}
	counted := func(count, entries int) []byte {
		b := make([]byte, 8+4*entries)
		binary.LittleEndian.PutUint32(b[4:], uint32(count))
		return b
	}
	if c, _, err := parseComments(counted(maxComments, maxComments)); err != nil || len(c.list) != maxComments {
		t.Fatalf("%d entries: %v", maxComments, err)
	}
	for _, b := range [][]byte{counted(maxComments+1, maxComments+1), counted(1<<30, 10)} {
		var err error
		if n := allocated(func() { _, _, err = parseComments(b) }); err != ErrCorrupt || n > 64<<10 {
			t.Fatalf("refused: %v, %d bytes", err, n)
		}
	}
}

// The comments' front cover is chosen by decoding the start of each picture, then that one alone.
func TestCommentsPictureFront(t *testing.T) {
	back := noisePNG(2<<20, 6)
	front := smallJPEG(30, 20, 500)
	c := &comments{}
	for _, p := range []*Picture{{Type: 4, Data: back}, {Type: 3, Data: front}} {
		c.list = append(c.list, blockPicture+"="+base64.StdEncoding.EncodeToString(encodePicture(p)))
	}
	var got *picRef
	if n := allocated(func() { got = c.picture() }); n > 256<<10 {
		t.Fatalf("picture() allocated %d bytes", n)
	}
	if got == nil || got.Type != 3 || !bytes.Equal(got.Data, front) {
		t.Fatalf("picture %#v", got)
	}
	// Without a front cover, the first.
	c.list = c.list[:1]
	if got := c.picture(); got == nil || got.Type != 4 || !bytes.Equal(got.Data, back) {
		t.Fatal("no first picture")
	}
	if !c.hasPicture() || (&comments{list: []string{"COVERARTMIME=image/png"}}).hasPicture() == false {
		t.Fatal("hasPicture")
	}
}
