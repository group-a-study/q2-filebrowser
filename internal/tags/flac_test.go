package tags

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// flacFixture is a FLAC with STREAMINFO, a SEEKTABLE, the comments list, pad bytes of padding (none
// when pad < 0) and audio bytes after the metadata.
func flacFixture(t *testing.T, list []string, pad int, audio []byte) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("fLaC")
	b.Write(flacHeader(flacStreamInfo, false, 34))
	b.Write(bytes.Repeat([]byte{0x11}, 34))
	b.Write(flacHeader(3, false, 18))
	b.Write(bytes.Repeat([]byte{0x22}, 18))
	cm := (&comments{vendor: "test", list: list}).bytes()
	b.Write(flacHeader(flacComment, pad < 0, len(cm)))
	b.Write(cm)
	if pad >= 0 {
		b.Write(flacHeader(flacPadding, true, pad))
		b.Write(make([]byte, pad))
	}
	b.Write(audio)
	p := filepath.Join(t.TempDir(), "a.flac")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func audioAfter(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	ff, err := readFLAC(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := readAt(f, ff.audio, st.Size()-ff.audio)
	return b
}

func TestFLACInPlace(t *testing.T) {
	audio := bytes.Repeat([]byte{0xFF, 0xF8, 1, 2}, 1000)
	p := flacFixture(t, []string{"TITLE=Old", "TRACKNUMBER=3/12", "MQAENCODER=x"}, 4096, audio)
	before, _ := os.Stat(p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "New title", Album: "Álbum"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if after.Size() != before.Size() {
		t.Fatalf("size %d → %d: not in place", before.Size(), after.Size())
	}
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Fields[Title] != "New title" || info.Fields[Album] != "Álbum" || info.Fields[Track] != "3" ||
		info.Fields[TrackTotal] != "12" {
		t.Fatalf("fields %#v", info.Fields)
	}
	if !bytes.Equal(audioAfter(t, p), audio) {
		t.Fatal("audio changed")
	}
	f, _ := os.Open(p)
	ff, _ := readFLAC(f, after.Size())
	f.Close()
	if len(ff.blocks) != 4 || ff.blocks[1].typ != 3 || ff.blocks[3].typ != flacPadding {
		t.Fatalf("blocks %#v", ff.blocks)
	}
}

func TestFLACRewriteAndPicture(t *testing.T) {
	audio := bytes.Repeat([]byte{0xFF, 0xF8, 9, 9}, 5000)
	p := flacFixture(t, []string{"TITLE=Old"}, -1, audio)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, bytes.Repeat([]byte{7}, 5000)...)
	if err := Write(p, Changes{Set: map[Field]string{Artist: "Someone"}, Picture: &Picture{Data: jpeg}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Fields[Artist] != "Someone" || info.Fields[Title] != "Old" || info.Picture == nil ||
		info.Picture.Width != 60 || info.Picture.Height != 50 || info.Picture.Size != len(jpeg) {
		t.Fatalf("info %#v %#v", info.Fields, info.Picture)
	}
	pic, err := ReadPicture(p)
	if err != nil || !bytes.Equal(pic.Data, jpeg) || pic.MIME != "image/jpeg" {
		t.Fatalf("picture %v", err)
	}
	if !bytes.Equal(audioAfter(t, p), audio) {
		t.Fatal("audio changed")
	}
	// Now there is padding: removing the picture stays in place.
	before, _ := os.Stat(p)
	if err := Write(p, Changes{RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if info, _ := Read(p); info.Picture != nil || after.Size() != before.Size() {
		t.Fatalf("picture kept, or size %d → %d", before.Size(), after.Size())
	}
}

func TestFLACChecks(t *testing.T) {
	p := flacFixture(t, nil, 100, []byte{1, 2, 3})
	for _, c := range []Changes{
		{Set: map[Field]string{"mood": "x"}},
		{Set: map[Field]string{Track: "three"}},
		{Picture: &Picture{Data: []byte("GIF89a")}},
	} {
		if err := Write(p, c); err == nil {
			t.Fatalf("%#v taken", c)
		}
	}
	os.WriteFile(p, []byte("fLaC\x01\x00\x00\x22"), 0o644) // STREAMINFO cut short
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("short file: %v", err)
	}
}

// fb is a metadata block for flacOf: its type and body.
type fb struct {
	typ  byte
	body []byte
}

var flacAudio = bytes.Repeat([]byte{0xFF, 0xF8, 1, 2}, 1000)

// flacOf is a FLAC of STREAMINFO and blocks, the last flagged, then flacAudio.
func flacOf(t *testing.T, blocks ...fb) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("fLaC")
	all := append([]fb{{flacStreamInfo, bytes.Repeat([]byte{0x11}, 34)}}, blocks...)
	for i, k := range all {
		b.Write(flacHeader(k.typ, i == len(all)-1, len(k.body)))
		b.Write(k.body)
	}
	b.Write(flacAudio)
	p := filepath.Join(t.TempDir(), "a.flac")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func vcBlock(list ...string) fb {
	return fb{flacComment, (&comments{vendor: "test", list: list}).bytes()}
}

func padBlock(n int) fb { return fb{flacPadding, make([]byte, n)} }

func picBlock(typ byte, data []byte) fb {
	return fb{flacPicture, encodePicture(&Picture{Type: typ, Data: data})}
}

// noisePNG is n bytes of a 640×480 PNG: its header, then noise.
func noisePNG(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	copy(b, "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	binary.BigEndian.PutUint32(b[16:], 640)
	binary.BigEndian.PutUint32(b[20:], 480)
	return b
}

// smallJPEG is a baseline JPEG's start, w×h px, n bytes long.
func smallJPEG(w, h, n int) []byte {
	b := append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 3},
		make([]byte, n)...)
	return b[:n]
}

// allocated is how many bytes f allocates.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func flacBlocks(t *testing.T, p string) *flacFile {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	ff, err := readFLAC(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func flacBody(t *testing.T, p string, b flacBlock) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, err := readAt(f, b.off+4, b.size)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// planned is how many bytes writing c to p in place writes; -1 when it writes the file again.
func planned(t *testing.T, p string, c Changes) int64 {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	pl, err := planFLAC(f, st.Size(), c)
	if err != nil {
		t.Fatal(err)
	}
	if pl.again != nil {
		return -1
	}
	var n int64
	for _, run := range pl.runs {
		n += run[len(run)-1].at + run[len(run)-1].n - run[0].at
	}
	return n
}

type countingReader struct {
	r io.ReaderAt
	n int64
}

func (c *countingReader) ReadAt(b []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(b, off)
	c.n += int64(n)
	return n, err
}

// Reading a FLAC reads a cover's fields, not the cover: a 4 MB one costs a few KB to read, and
// ReadPicture holds it once.
func TestFLACReadLeavesTheCover(t *testing.T) {
	cover := noisePNG(4<<20, 1)
	p := flacOf(t, picBlock(3, cover), vcBlock("TITLE=Old"), padBlock(8192))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	cr := &countingReader{r: f}
	_, pic, err := flacCodec{}.read(cr, st.Size())
	f.Close()
	if err != nil || pic == nil || !pic.hasOffset || pic.Len != int64(len(cover)) || pic.Type != 3 {
		t.Fatalf("read %v %#v", err, pic)
	}
	if cr.n > 16<<10 {
		t.Fatalf("read %d bytes of the file for its tags", cr.n)
	}
	var info *Info
	if n := allocated(func() { info, err = Read(p) }); n > 256<<10 {
		t.Fatalf("Read allocated %d bytes", n)
	}
	if err != nil || info.Picture == nil || info.Picture.Width != 640 || info.Picture.Size != len(cover) ||
		info.Fields[Title] != "Old" {
		t.Fatalf("info %v %#v", err, info)
	}
	var got *Picture
	if n := allocated(func() { got, err = ReadPicture(p) }); n > uint64(len(cover))+256<<10 {
		t.Fatalf("ReadPicture allocated %d bytes for a %d-byte cover", n, len(cover))
	}
	if err != nil || !bytes.Equal(got.Data, cover) {
		t.Fatalf("picture %v", err)
	}
	// Fields before the image that don't fit the first read are read too; an image past its block isn't.
	long := encodePicture(&Picture{Type: 3, Desc: string(bytes.Repeat([]byte{'d'}, 5000)), Data: cover[:2000]})
	p = flacOf(t, fb{flacPicture, long}, padBlock(10))
	if info, err := Read(p); err != nil || info.Picture == nil || info.Picture.Size != 2000 {
		t.Fatalf("long description: %v %#v", err, info)
	}
	binary.BigEndian.PutUint32(long[len(long)-2000-4:], 2001)
	p = flacOf(t, fb{flacPicture, long}, padBlock(10))
	if info, err := Read(p); err != nil || info.Picture != nil {
		t.Fatalf("image past its block: %v %#v", err, info)
	}
}

// A title edit in place writes the comments and the padding's header, whichever order the blocks are
// in: a cover is neither held in memory nor moved nor written back.
func TestFLACInPlaceLeavesTheCovers(t *testing.T) {
	cover := noisePNG(2<<20, 2)
	for name, blocks := range map[string][]fb{
		"cover first":    {picBlock(3, cover), vcBlock("TITLE=Old"), padBlock(8192)},
		"comments first": {vcBlock("TITLE=Old"), picBlock(3, cover), padBlock(8192)},
		"three covers": {vcBlock("TITLE=Old"), picBlock(3, cover[:1<<20]), picBlock(4, noisePNG(1<<20, 3)),
			picBlock(0, noisePNG(1<<20, 4)), padBlock(4096)},
	} {
		t.Run(name, func(t *testing.T) {
			p := flacOf(t, blocks...)
			before := flacBlocks(t, p)
			c := Changes{Set: map[Field]string{Title: "A longer title than the old one"}}
			if n := planned(t, p, c); n < 0 || n > 16<<10 {
				t.Fatalf("writes %d bytes", n)
			}
			var err error
			if n := allocated(func() { err = Write(p, c) }); n > 512<<10 {
				t.Fatalf("Write allocated %d bytes", n)
			}
			if err != nil {
				t.Fatal(err)
			}
			after := flacBlocks(t, p)
			if after.audio != before.audio {
				t.Fatalf("metadata %d → %d bytes: not in place", before.audio, after.audio)
			}
			var pics []flacBlock
			for _, b := range after.blocks {
				if b.typ == flacPicture {
					pics = append(pics, b)
				}
			}
			for i, b := range before.blocks {
				if b.typ != flacPicture {
					continue
				}
				if len(pics) == 0 || pics[0].off != b.off || !bytes.Equal(flacBody(t, p, pics[0]), blocks[i-1].body) {
					t.Fatalf("picture at %d moved or changed: %#v", b.off, after.blocks)
				}
				pics = pics[1:]
			}
			info, err := Read(p)
			if err != nil || info.Fields[Title] != "A longer title than the old one" || info.Picture == nil {
				t.Fatalf("read %v %#v", err, info)
			}
			if !bytes.Equal(audioAfter(t, p), flacAudio) {
				t.Fatal("audio changed")
			}
			if name == "comments first" {
				var types []byte
				for _, b := range after.blocks {
					types = append(types, b.typ)
				}
				if want := []byte{flacStreamInfo, flacPadding, flacPicture, flacComment, flacPadding}; !bytes.Equal(types, want) {
					t.Fatalf("blocks %v, want %v", types, want)
				}
			}
			// And back: the comments shrink where they are now.
			if n := planned(t, p, Changes{Set: map[Field]string{Title: "B"}}); n < 0 || n > 16<<10 {
				t.Fatalf("writes %d bytes back", n)
			}
		})
	}
}

// tightFLAC is STREAMINFO, the comments, a picture of cover and 100 bytes of padding: no room takes
// the comments grown by tightEdit, all the room together does.
func tightFLAC(t *testing.T, cover []byte) string {
	t.Helper()
	return flacOf(t, vcBlock("TITLE=Old", "COMMENT="+strings.Repeat("x", 150)), picBlock(3, cover), padBlock(100))
}

var tightEdit = Changes{Set: map[Field]string{Title: "Old" + strings.Repeat("y", 50)}}

// planMoves is how many bytes of kept blocks writing c to p in place moves; -1 when it writes the file
// again.
func planMoves(t *testing.T, p string, c Changes) int64 {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	pl, err := planFLAC(f, st.Size(), c)
	if err != nil {
		t.Fatal(err)
	}
	if pl.again != nil {
		return -1
	}
	var n int64
	for _, run := range pl.runs {
		for _, s := range run {
			if s.moved {
				n += s.n
			}
		}
	}
	return n
}

// A little block after the comments moves up towards the start to make room, in place: up to flacNear
// of it, header and all.
func TestFLACInPlaceMovesUp(t *testing.T) {
	for _, n := range []int{1000, flacNear - 4 - 41} { // a PNG's picture block is 41 bytes and the PNG
		cover := noisePNG(n, 5)
		p := tightFLAC(t, cover)
		before := flacBlocks(t, p)
		st, _ := os.Stat(p)
		if k := planMoves(t, p, tightEdit); k < 0 || k > flacNear {
			t.Fatalf("%d-byte picture: moves %d bytes", n, k)
		}
		if err := Write(p, tightEdit); err != nil {
			t.Fatal(err)
		}
		after := flacBlocks(t, p)
		if st2, _ := os.Stat(p); !os.SameFile(st, st2) || after.audio != before.audio {
			t.Fatalf("%d-byte picture: not in place", n)
		}
		if len(after.blocks) != 4 || after.blocks[1].typ != flacPicture || after.blocks[1].off >= before.blocks[2].off ||
			after.blocks[2].typ != flacComment {
			t.Fatalf("blocks %#v → %#v", before.blocks, after.blocks)
		}
		if !bytes.Equal(flacBody(t, p, after.blocks[1]), picBlock(3, cover).body) {
			t.Fatal("picture changed")
		}
		if info, err := Read(p); err != nil || info.Fields[Title] != tightEdit.Set[Title] || info.Fields[Comment] == "" {
			t.Fatalf("read %v %#v", err, info)
		}
		if !bytes.Equal(audioAfter(t, p), flacAudio) {
			t.Fatal("audio changed")
		}
	}
}

// A cover isn't moved in place to make room: an app that ends part way through would leave no song.
// The file is written again, the cover streamed, in its place among the blocks, with padding after.
func TestFLACMoveWritesAgain(t *testing.T) {
	for _, n := range []int{flacNear - 4 - 41 + 1, 2 << 20} {
		cover := noisePNG(n, 5)
		p := tightFLAC(t, cover)
		st, _ := os.Stat(p)
		if k := planMoves(t, p, tightEdit); k != -1 {
			t.Fatalf("%d-byte cover: in place, moving %d bytes", n, k)
		}
		var err error
		if a := allocated(func() { err = Write(p, tightEdit) }); n > 1<<20 && a > uint64(n)/2 {
			t.Fatalf("Write allocated %d bytes for a %d-byte cover", a, n)
		}
		if err != nil {
			t.Fatal(err)
		}
		if st2, _ := os.Stat(p); os.SameFile(st, st2) {
			t.Fatalf("%d-byte cover: in place", n)
		}
		after := flacBlocks(t, p)
		var types []byte
		for _, b := range after.blocks {
			types = append(types, b.typ)
		}
		if want := []byte{flacStreamInfo, flacComment, flacPicture, flacPadding}; !bytes.Equal(types, want) ||
			after.blocks[3].size != flacNewPadding {
			t.Fatalf("blocks %#v", after.blocks)
		}
		if !bytes.Equal(flacBody(t, p, after.blocks[2]), picBlock(3, cover).body) {
			t.Fatal("cover changed")
		}
		if info, err := Read(p); err != nil || info.Fields[Title] != tightEdit.Set[Title] || info.Fields[Comment] == "" {
			t.Fatalf("read %v %#v", err, info)
		}
		if !bytes.Equal(audioAfter(t, p), flacAudio) {
			t.Fatal("audio changed")
		}
		// Now the padding has room, and the next edit leaves the cover where it is.
		if k := planMoves(t, p, Changes{Set: map[Field]string{Title: strings.Repeat("z", 500)}}); k != 0 {
			t.Fatalf("next edit moves %d bytes", k)
		}
	}
}

// When the app ends during a save that would have moved a cover, the save gives up and the song is
// as it was, wherever it stops; or it is all written.
func TestFLACMoveStops(t *testing.T) {
	cover := noisePNG(1<<20, 6)
	orig, err := os.ReadFile(tightFLAC(t, cover))
	if err != nil {
		t.Fatal(err)
	}
	defer func(f func() bool) { Stopping = f }(Stopping)
	stopped := 0
	for _, k := range []int{0, 1, 2, 5, 10, 20, 40, 1 << 30} {
		p := filepath.Join(t.TempDir(), "s.flac")
		if err := os.WriteFile(p, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		calls := 0
		Stopping = func() bool { calls++; return calls > k } // the app ends after k checks
		err := Write(p, tightEdit)
		got, _ := os.ReadFile(p)
		switch err {
		case ErrStopped:
			stopped++
			if !bytes.Equal(got, orig) {
				t.Fatalf("stopped after %d checks: the file changed", k)
			}
		case nil:
			info, rerr := Read(p)
			if rerr != nil || info.Fields[Title] != tightEdit.Set[Title] || !bytes.Equal(audioAfter(t, p), flacAudio) {
				t.Fatalf("after %d checks: %v %#v", k, rerr, info)
			}
		default:
			t.Fatalf("after %d checks: %v", k, err)
		}
		if _, err := os.Stat(partName(p)); err == nil {
			t.Fatal("part file left")
		}
		if k == 0 && err != ErrStopped || k == 1<<30 && err != nil {
			t.Fatalf("after %d checks: %v", k, err)
		}
	}
	if stopped < 3 {
		t.Fatalf("stopped %d times", stopped)
	}
}

// An ID3 tag before "fLaC" stays as it is, in place and written again.
func TestFLACAfterID3(t *testing.T) {
	id3 := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x14"), bytes.Repeat([]byte{0x33}, 20)...)
	p := flacOf(t, vcBlock("TITLE=Old"), padBlock(100))
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(id3, b...), 0o644)
	for _, c := range []Changes{
		{Set: map[Field]string{Title: "In place"}},
		{Set: map[Field]string{Lyrics: string(bytes.Repeat([]byte{'l'}, 500))}},
	} {
		if err := Write(p, c); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if !bytes.HasPrefix(b, id3) || !bytes.Equal(audioAfter(t, p), flacAudio) {
			t.Fatal("ID3 tag or audio changed")
		}
	}
	info, err := Read(p)
	if err != nil || info.Fields[Title] != "In place" || len(info.Fields[Lyrics]) != 500 || len(info.Notes) != 1 {
		t.Fatalf("read %v %#v", err, info)
	}
}

// Two comment blocks read as one list, and a write keeps it: an edit of one field changes no other.
func TestFLACTwoCommentBlocks(t *testing.T) {
	blocks := []fb{vcBlock("TITLE=First"), vcBlock("TITLE=Second", "ARTIST=OnlyInSecond"), padBlock(500)}
	p := flacOf(t, blocks...)
	info, err := Read(p)
	if err != nil || info.Fields[Title] != "First / Second" || info.Fields[Artist] != "OnlyInSecond" ||
		len(info.Notes) != 1 {
		t.Fatalf("read %v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Album: "Z"}}); err != nil {
		t.Fatal(err)
	}
	info, err = Read(p)
	if err != nil || info.Fields[Title] != "First / Second" || info.Fields[Artist] != "OnlyInSecond" ||
		info.Fields[Album] != "Z" || len(info.Notes) != 0 {
		t.Fatalf("after %v %#v", err, info)
	}
	n := 0
	for _, b := range flacBlocks(t, p).blocks {
		if b.typ == flacComment {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d comment blocks", n)
	}
	// A picture alone makes them one too, as the note says.
	p = flacOf(t, blocks...)
	if err := Write(p, Changes{Picture: &Picture{Type: 3, MIME: "image/jpeg", Data: smallJPEG(20, 10, 300)}}); err != nil {
		t.Fatal(err)
	}
	if info, err := Read(p); err != nil || info.Fields[Artist] != "OnlyInSecond" || len(info.Notes) != 0 ||
		info.Picture == nil {
		t.Fatalf("after a picture %v %#v", err, info)
	}
}

// A crafted file of many blocks or many entries is refused before it costs memory or time.
func TestFLACCraftedCounts(t *testing.T) {
	blocks := func(n int) []fb {
		var out []fb
		for i := 0; i < n; i++ {
			out = append(out, fb{2, nil})
		}
		return out
	}
	if _, err := Read(flacOf(t, blocks(flacMaxBlocks-1)...)); err != nil {
		t.Fatalf("%d blocks: %v", flacMaxBlocks, err)
	}
	p := flacOf(t, blocks(flacMaxBlocks)...)
	var err error
	if n := allocated(func() { _, err = Read(p) }); err != ErrCorrupt || n > 256<<10 {
		t.Fatalf("%d blocks: %v, %d bytes", flacMaxBlocks+1, err, n)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrCorrupt {
		t.Fatalf("write: %v", err)
	}
	// An entry count past the limit, each entry empty.
	empty := func(n int) fb {
		b := make([]byte, 8+4*n) // no vendor, the count, n empty entries
		binary.LittleEndian.PutUint32(b[4:], uint32(n))
		return fb{flacComment, b}
	}
	p = flacOf(t, empty(maxComments+10))
	if n := allocated(func() { _, err = Read(p) }); err != ErrCorrupt || n > 256<<10 {
		t.Fatalf("%d entries: %v, %d bytes", maxComments+10, err, n)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrCorrupt {
		t.Fatalf("write: %v", err)
	}
	// Two blocks under the limit, past it together.
	p = flacOf(t, empty(maxComments/2+10), empty(maxComments/2+10))
	if _, err := Read(p); err != ErrCorrupt {
		t.Fatalf("two blocks: %v", err)
	}
}

// A cover kept in the comments, as some taggers do, is shown, and a new picture or Remove replaces it.
func TestFLACCommentPicture(t *testing.T) {
	jpeg := smallJPEG(300, 200, 4000)
	mbp := blockPicture + "=" + base64.StdEncoding.EncodeToString(encodePicture(&Picture{Type: 3, Data: jpeg}))
	art := []string{"COVERART=" + base64.StdEncoding.EncodeToString(jpeg), "COVERARTMIME=image/jpeg"}
	for name, list := range map[string][]string{"block picture": {"TITLE=Orig", mbp}, "coverart": append([]string{"TITLE=Orig"}, art...)} {
		t.Run(name, func(t *testing.T) {
			p := flacOf(t, vcBlock(list...), padBlock(4096))
			info, err := Read(p)
			if err != nil || info.Picture == nil || info.Picture.Width != 300 || info.Picture.Size != len(jpeg) {
				t.Fatalf("read %v %#v", err, info)
			}
			if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, jpeg) {
				t.Fatalf("picture %v", err)
			}
			// A title edit keeps it.
			if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != nil {
				t.Fatal(err)
			}
			if info, _ := Read(p); info.Picture == nil || info.Fields[Title] != "New" {
				t.Fatalf("after a title %#v", info)
			}
			hasPicture := func() bool {
				f, _ := os.Open(p)
				defer f.Close()
				cm, _, err := flacBlocks(t, p).comments(f)
				if err != nil {
					t.Fatal(err)
				}
				return cm.hasPicture()
			}
			st, _ := os.Stat(p)
			if err := Write(p, Changes{RemovePicture: true}); err != nil {
				t.Fatal(err)
			}
			if info, _ := Read(p); info.Picture != nil || info.Fields[Title] != "New" || hasPicture() {
				t.Fatalf("after Remove %#v", info)
			}
			if after, _ := os.Stat(p); after.Size() != st.Size() {
				t.Fatal("not in place")
			}
			// A new picture replaces one in the comments: it goes in a picture block, alone.
			p = flacOf(t, vcBlock(list...), padBlock(4096))
			other := smallJPEG(40, 30, 1000)
			if err := Write(p, Changes{Picture: &Picture{Type: 3, MIME: "image/jpeg", Data: other}}); err != nil {
				t.Fatal(err)
			}
			if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, other) || hasPicture() {
				t.Fatalf("after a new picture %v", err)
			}
		})
	}
}

// randText is n random letters, digits and a few accents.
func randText(r *rand.Rand, n int) string {
	const a = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJ0123456789éü"
	var s strings.Builder
	for s.Len() < n {
		s.WriteByte(a[r.Intn(len(a))])
	}
	return strings.ToValidUTF8(s.String(), "")
}

// randFLAC is a FLAC of random blocks after STREAMINFO: seek tables, APPLICATIONs and other kinds,
// one or two comment blocks (a cover in one now and then), pictures of up to 1.2 MB and padding;
// sometimes an ID3 tag first.
func randFLAC(r *rand.Rand) []byte {
	var b bytes.Buffer
	if r.Intn(5) == 0 {
		n := r.Intn(200)
		b.Write([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, byte(n >> 7), byte(n & 0x7f)})
		b.Write(bytes.Repeat([]byte{0x33}, n))
	}
	b.WriteString("fLaC")
	random := func(typ byte, n int) fb {
		k := fb{typ, make([]byte, n)}
		r.Read(k.body)
		return k
	}
	all, vcs := []fb{random(flacStreamInfo, 34)}, 0
	for i, n := 0, r.Intn(7); i < n; i++ {
		switch k := r.Intn(9); {
		case k == 0:
			all = append(all, random(flacSeekTable, 18*r.Intn(20)))
		case k == 1:
			all = append(all, random(2, 4+r.Intn(3000)))
		case k == 2:
			all = append(all, random(byte(5+2*r.Intn(5)), r.Intn(500)))
		case k <= 4 && vcs < 2:
			var list []string
			keys := []string{"TITLE", "ARTIST", "ALBUM", "DATE", "YEAR", "GENRE", "MQAENCODER", "COMMENT", "LYRICS", "title"}
			for j, m := 0, r.Intn(8); j < m; j++ {
				v := randText(r, 1+r.Intn(40))
				if r.Intn(10) == 0 {
					v = randText(r, 1000+r.Intn(5000))
				}
				list = append(list, keys[r.Intn(len(keys))]+"="+v)
			}
			if r.Intn(8) == 0 {
				jpeg := smallJPEG(10+r.Intn(500), 10+r.Intn(500), 200+r.Intn(3000))
				list = append(list, blockPicture+"="+base64.StdEncoding.EncodeToString(encodePicture(&Picture{Type: 3, Data: jpeg})))
			}
			all, vcs = append(all, vcBlock(list...)), vcs+1
		case k == 5 || k == 6:
			n := []int{100 + r.Intn(5000), 20000 + r.Intn(200000), 300000 + r.Intn(900000)}[r.Intn(3)]
			all = append(all, picBlock(byte(r.Intn(6)), noisePNG(n, r.Int63())))
		default:
			n := r.Intn(20000)
			if r.Intn(5) == 0 {
				n = r.Intn(10)
			}
			all = append(all, padBlock(n))
		}
	}
	for i, k := range all {
		b.Write(flacHeader(k.typ, i == len(all)-1, len(k.body)))
		b.Write(k.body)
	}
	b.Write(flacAudio)
	return b.Bytes()
}

// randChanges are random fields to set, sometimes with a new picture or Remove.
func randChanges(r *rand.Rand) Changes {
	for {
		c := Changes{Set: map[Field]string{}}
		if r.Intn(2) == 0 {
			c.Set[Title] = []string{"", randText(r, 1+r.Intn(30)), randText(r, 100+r.Intn(300)),
				randText(r, 2000+r.Intn(4000))}[r.Intn(4)]
		}
		if r.Intn(4) == 0 {
			c.Set[Artist] = randText(r, r.Intn(30))
		}
		if r.Intn(5) == 0 {
			c.Set[Lyrics] = randText(r, r.Intn(3)*r.Intn(15000))
		}
		switch r.Intn(8) {
		case 0, 1:
			c.Picture = &Picture{Data: smallJPEG(20+r.Intn(900), 20+r.Intn(900), 300+r.Intn(3)*r.Intn(150000))}
		case 2:
			c.RemovePicture = true
		}
		if !c.Empty() && Check(c) == nil {
			return c
		}
	}
}

// flacMeta is a FLAC's metadata as a test sees it: what is before "fLaC", each block with its offset,
// and the audio.
type flacMeta struct {
	prefix, audio []byte
	blocks        []fbAt
}

type fbAt struct {
	fb
	off int64
}

func flacMetaOf(t *testing.T, desc string, b []byte) flacMeta {
	t.Helper()
	ff, err := readFLAC(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", desc, err)
	}
	m := flacMeta{prefix: b[:ff.start], audio: b[ff.audio:]}
	for i, k := range ff.blocks {
		if k.last != (i == len(ff.blocks)-1) {
			t.Fatalf("%s: block %d of %d flagged last", desc, i, len(ff.blocks))
		}
		body := b[k.off+4 : k.off+4+k.size]
		if k.typ == flacPadding && bytes.Count(body, []byte{0}) != len(body) {
			t.Fatalf("%s: padding that isn't zeros", desc)
		}
		m.blocks = append(m.blocks, fbAt{fb{k.typ, body}, k.off})
	}
	return m
}

// of is m's blocks for which keep is true.
func (m flacMeta) of(keep func(byte) bool) []fbAt {
	var out []fbAt
	for _, k := range m.blocks {
		if keep(k.typ) {
			out = append(out, k)
		}
	}
	return out
}

func sameBlocks(a, b []fbAt) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].typ != b[i].typ || !bytes.Equal(a[i].body, b[i].body) {
			return false
		}
	}
	return true
}

// Random layouts and edits, five in a row on each file: every block but the ones that change is kept,
// byte for byte and in order, and so are the audio and an ID3 tag first; the comments and pictures are
// as the edit says. In place the metadata keeps its length, and kept blocks move up only, flacNear of
// them at most, so a cover never moves.
func TestFLACRandomEdits(t *testing.T) {
	n := 150
	if testing.Short() {
		n = 30
	}
	stats := map[string]int{}
	isVC := func(typ byte) bool { return typ == flacComment }
	isPic := func(typ byte) bool { return typ == flacPicture }
	for seed := 0; seed < n; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		p := filepath.Join(t.TempDir(), "r.flac")
		if err := os.WriteFile(p, randFLAC(r), 0o644); err != nil {
			t.Fatal(err)
		}
		for step := 0; step < 5; step++ {
			c := randChanges(r)
			desc := fmt.Sprintf("seed %d step %d", seed, step)
			cur, _ := os.ReadFile(p)
			before := flacMetaOf(t, desc, cur)
			stBefore, _ := os.Stat(p)
			var merged *comments
			vcs := before.of(isVC)
			for _, k := range vcs {
				cm, _, err := parseComments(k.body)
				if err != nil {
					t.Fatal(err)
				}
				if merged == nil {
					merged = cm
				} else {
					merged.list = append(merged.list, cm.list...)
				}
			}
			old := map[Field]string{}
			if merged != nil {
				old = merged.fields()
			}
			set := map[Field]string{}
			for k, v := range c.Set {
				if v = clean(k, v); clean(k, old[k]) != v {
					set[k] = v
				}
			}
			picChange := c.Picture != nil || c.RemovePicture
			newVC := len(set) > 0 || len(vcs) > 1 || picChange && merged != nil && merged.hasPicture()
			moves := planMoves(t, p, c)
			if err := Write(p, c); err != nil {
				t.Fatalf("%s: %v", desc, err)
			}
			got, _ := os.ReadFile(p)
			if len(set) == 0 && !picChange {
				if !bytes.Equal(got, cur) {
					t.Fatalf("%s: nothing to change, but the file changed", desc)
				}
				continue
			}
			after := flacMetaOf(t, desc, got)
			if !bytes.Equal(after.prefix, before.prefix) || !bytes.Equal(after.audio, before.audio) {
				t.Fatalf("%s: ID3 tag or audio changed", desc)
			}
			kept := func(typ byte) bool {
				return typ != flacPadding && !(typ == flacComment && newVC) && !(typ == flacPicture && picChange)
			}
			keptBefore, keptAfter := before.of(kept), after.of(kept)
			if !sameBlocks(keptAfter, keptBefore) {
				t.Fatalf("%s: kept blocks changed", desc)
			}
			wantVC := vcs
			if newVC {
				m := merged
				if m == nil {
					m = &comments{vendor: "q2-filebrowser"}
				}
				m.apply(set)
				if picChange {
					m.setPicture(nil)
				}
				wantVC = nil
				if len(vcs) > 0 || len(m.list) > 0 {
					wantVC = []fbAt{{fb: fb{flacComment, m.bytes()}}}
				}
			}
			if !sameBlocks(after.of(isVC), wantVC) {
				t.Fatalf("%s: comments not as set", desc)
			}
			wantPics := before.of(isPic)
			if picChange {
				wantPics = nil
			}
			if c.Picture != nil {
				pp := *c.Picture
				pp.MIME, pp.Type = pictureInfo(pp.Data).MIME, 3
				wantPics = []fbAt{{fb: fb{flacPicture, encodePicture(&pp)}}}
			}
			if !sameBlocks(after.of(isPic), wantPics) {
				t.Fatalf("%s: pictures not as set", desc)
			}
			if stAfter, _ := os.Stat(p); !os.SameFile(stBefore, stAfter) {
				stats["written again"]++
				if moves != -1 {
					t.Fatalf("%s: planned in place, written again", desc)
				}
				continue
			}
			stats["in place"]++
			if len(got)-len(after.audio) != len(cur)-len(before.audio) || moves < 0 || moves > flacNear {
				t.Fatalf("%s: in place, %d bytes moved, metadata %d → %d", desc, moves,
					len(cur)-len(before.audio), len(got)-len(after.audio))
			}
			var moved int64
			for i, k := range keptAfter {
				was := keptBefore[i]
				if k.off > was.off || k.off < was.off && len(k.body) > flacNear {
					t.Fatalf("%s: a %d-byte block moved from %d to %d", desc, len(k.body), was.off, k.off)
				}
				if k.off != was.off {
					moved += 4 + int64(len(k.body))
					stats["moved up"]++
				}
			}
			if moved > flacNear {
				t.Fatalf("%s: %d bytes of kept blocks moved", desc, moved)
			}
		}
	}
	t.Logf("%v", stats)
}

// Removing a cover that sits after long padding: the new padding's header goes before the old cover's
// bytes are zeroed, so a save cut short between any two writes leaves a FLAC that reads.
func TestFLACZeroFillLast(t *testing.T) {
	p := flacOf(t, vcBlock("TITLE=Old"), padBlock(100<<10), picBlock(3, noisePNG(200<<10, 9)), padBlock(10))
	orig, _ := os.ReadFile(p)
	f, _ := os.Open(p)
	st, _ := f.Stat()
	pl, err := planFLAC(f, st.Size(), Changes{RemovePicture: true})
	f.Close()
	if err != nil || pl.again != nil || len(pl.runs) == 0 {
		t.Fatalf("plan %v %#v", err, pl)
	}
	zeroOnly := false
	for _, run := range pl.runs {
		zero := true
		for _, s := range run {
			zero = zero && s.zero
		}
		if !zero && zeroOnly {
			t.Fatal("a run with headers after one of zeros alone")
		}
		zeroOnly = zeroOnly || zero
	}
	for k := 0; k <= len(pl.runs); k++ {
		q := filepath.Join(t.TempDir(), "q.flac")
		os.WriteFile(q, orig, 0o644)
		if err := patchRuns(q, pl.runs[:k]); err != nil {
			t.Fatal(err)
		}
		if info, err := Read(q); err != nil || info.Fields[Title] != "Old" {
			t.Fatalf("cut after %d of %d runs: %v %#v", k, len(pl.runs), err, info)
		}
	}
	if err := Write(p, Changes{RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	if info, err := Read(p); err != nil || info.Picture != nil || !bytes.Equal(audioAfter(t, p), flacAudio) {
		t.Fatalf("removed: %v %#v", err, info)
	}
}
