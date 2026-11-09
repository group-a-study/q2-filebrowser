package tags

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// paginate lays packets out on pages of at most segs segments, as a muxer might: a page ends when it is
// full or after a packet flush says so. A page's granule position is that of the last packet ending on
// it, -1 when none does.
func paginate(serial, seq uint32, packets [][]byte, gran []int64, segs int, flush func(i int) bool,
	bos, eos bool) []byte {
	type page struct {
		lacing, data []byte
		gran         int64
		cont         bool
	}
	var pages []*page
	cur := &page{gran: -1}
	for i, p := range packets {
		for at := 0; ; {
			l := min(len(p)-at, 255)
			cur.lacing = append(cur.lacing, byte(l))
			cur.data = append(cur.data, p[at:at+l]...)
			at += l
			done := l < 255
			if done {
				cur.gran = gran[i]
			}
			if len(cur.lacing) == segs || done && flush(i) {
				pages = append(pages, cur)
				cur = &page{gran: -1, cont: !done}
			}
			if done {
				break
			}
		}
	}
	if len(cur.lacing) > 0 {
		pages = append(pages, cur)
	}
	var out bytes.Buffer
	for i, pg := range pages {
		p := &oggPage{granule: uint64(pg.gran), serial: serial, seq: seq + uint32(i), lacing: pg.lacing}
		if pg.cont {
			p.flags |= oggContinued
		}
		if bos && i == 0 {
			p.flags |= oggBOS
		}
		if eos && i == len(pages)-1 {
			p.flags |= oggEOS
		}
		h := p.header()
		binary.LittleEndian.PutUint32(h[22:], pageCRC(h, pg.data))
		out.Write(h)
		out.Write(pg.data)
	}
	return out.Bytes()
}

func vorbisID() []byte {
	b := []byte("\x01vorbis\x00\x00\x00\x00\x02\x44\xac\x00\x00\x00\x00\x00\x00\x00\xf4\x01\x00\x00\x00\x00\x00\xb8\x01")
	return b
}

func vorbisCommentPacket(list []string, pad int) []byte {
	b := append([]byte("\x03vorbis"), (&comments{vendor: "Xiph.Org libVorbis I 20200704", list: list}).bytes()...)
	return append(append(b, 1), make([]byte, pad)...)
}

func opusHead() []byte { return []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00") }

func opusTags(list []string, after []byte) []byte {
	b := append([]byte("OpusTags"), (&comments{vendor: "Lavf61", list: list}).bytes()...)
	return append(b, after...)
}

// audioPackets are n made-up audio packets, some of them 255 and 510 bytes long, and their granules.
func audioPackets(n int) ([][]byte, []int64) {
	var ps [][]byte
	var gs []int64
	for i := 0; i < n; i++ {
		size := 40 + i*37%700
		switch {
		case i%17 == 0:
			size = 255
		case i%23 == 0:
			size = 510
		}
		p := make([]byte, size)
		for j := range p {
			p[j] = byte(i*7 + j)
		}
		ps = append(ps, p)
		gs = append(gs, int64(1024*(i+1)))
	}
	return ps, gs
}

// oggStream is a whole stream: header packets on pages of segs segments, the first alone, the last
// ending its page; audio packets on pages of 8 packets.
func oggStream(serial uint32, head [][]byte, segs int) []byte {
	audio, ag := audioPackets(120)
	packets := append(append([][]byte(nil), head...), audio...)
	gran := append(make([]int64, len(head)), ag...)
	return paginate(serial, 0, packets, gran, segs, func(i int) bool {
		return i == 0 || i == len(head)-1 || i >= len(head) && (i-len(head))%8 == 7
	}, true, true)
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func vorbisFile(t *testing.T, list []string, segs int) string {
	t.Helper()
	setup := append([]byte("\x05vorbis"), bytes.Repeat([]byte{0x5A, 0xA5, 0x3C}, 1200)...)
	return writeTemp(t, "a.ogg", oggStream(0x1234, [][]byte{vorbisID(), vorbisCommentPacket(list, 0), setup}, segs))
}

type tpage struct {
	flags       byte
	granule     uint64
	serial, seq uint32
	lacing      []byte
	body        []byte
	crcOK       bool
}

// pagesOf are b's pages, and the bytes after the last one that aren't a page.
func pagesOf(b []byte) (pages []tpage, rest []byte) {
	for off := 0; off < len(b); {
		p, ok := parseOggHeader(b[off:])
		if !ok || off+int(p.size()) > len(b) {
			return pages, b[off:]
		}
		hl, end := off+int(p.hlen()), off+int(p.size())
		pages = append(pages, tpage{p.flags, p.granule, p.serial, p.seq, p.lacing, b[hl:end],
			pageCRC(b[off:hl], b[hl:end]) == p.crc})
		off = end
	}
	return pages, nil
}

// headerPages is how many pages the first n packets take.
func headerPages(pages []tpage, n int) int {
	for i, p := range pages {
		for _, l := range p.lacing {
			if l < 255 {
				n--
			}
		}
		if n <= 0 {
			return i + 1
		}
	}
	return len(pages)
}

// sameAudio fails unless after's pages past its headers are before's, but for sequence numbers moved
// by the change in header pages, and every page's CRC is right.
func sameAudio(t *testing.T, before, after []byte, headers int) {
	t.Helper()
	bp, brest := pagesOf(before)
	ap, arest := pagesOf(after)
	hb, ha := headerPages(bp, headers), headerPages(ap, headers)
	if len(bp)-hb != len(ap)-ha || !bytes.Equal(brest, arest) {
		t.Fatalf("pages %d-%d → %d-%d, rest %d → %d bytes", len(bp), hb, len(ap), ha, len(brest), len(arest))
	}
	for i, p := range ap {
		if !p.crcOK {
			t.Fatalf("page %d: bad CRC", i)
		}
		if i > 0 && p.serial == ap[i-1].serial && p.seq != ap[i-1].seq+1 {
			t.Fatalf("page %d: sequence %d after %d", i, p.seq, ap[i-1].seq)
		}
	}
	for i := range bp[hb:] {
		b, a := bp[hb+i], ap[ha+i]
		if a.flags != b.flags || a.granule != b.granule || a.serial != b.serial || !bytes.Equal(a.lacing, b.lacing) ||
			!bytes.Equal(a.body, b.body) || a.seq != b.seq+uint32(ha-hb) {
			t.Fatalf("audio page %d changed", i)
		}
	}
	if !bytes.Equal(before[:len(bp[0].body)+28], after[:len(bp[0].body)+28]) {
		t.Fatal("first page changed")
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// oggComment is the comments of the Ogg file at p, those left in the file read from there, and the
// bytes after them in their packet.
func oggComment(t *testing.T, p string) (*comments, []byte) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	h, err := readOggHead(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	tg, err := h.comment(f)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range tg.cm.list {
		if s, ok := tg.refs[e]; ok {
			b, err := io.ReadAll(h.commentReader(f, s.off, s.n))
			if err != nil {
				t.Fatal(err)
			}
			tg.cm.list[i] = string(b)
		}
	}
	rest, err := io.ReadAll(h.commentReader(f, tg.rest.off, tg.rest.n))
	if err != nil {
		t.Fatal(err)
	}
	return tg.cm, rest
}

// oggCover is a METADATA_BLOCK_PICTURE entry holding a made-up JPEG of n bytes, and the JPEG.
func oggCover(n int) (string, []byte) {
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, bytes.Repeat([]byte{7}, n-12)...)
	for i := 12; i < n; i += 4093 {
		jpeg[i] = byte(i >> 12)
	}
	return blockPicture + "=" + base64.StdEncoding.EncodeToString(encodePicture(&Picture{Type: 3, Data: jpeg})), jpeg
}

// oggAllocated is how many bytes f allocates.
func oggAllocated(f func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	f()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

func TestOggCRC(t *testing.T) {
	if c := oggCRC(0, []byte("123456789")); c != 0x89A1897F {
		t.Fatalf("crc %08x", c)
	}
	b := oggStream(7, [][]byte{vorbisID(), vorbisCommentPacket(nil, 0), []byte("\x05vorbis")}, 255)
	pages, _ := pagesOf(b)
	p, _ := parseOggHeader(b)
	h := p.header()
	h[18] = 0x55 // a new sequence number
	want := pageCRC(h, pages[0].body)
	if got := renumbered(p.crc, p.seq, 0x55, p.size()); got != want {
		t.Fatalf("renumbered %08x, want %08x", got, want)
	}
	if got := renumbered(p.crc^1, p.seq, 0x55, p.size()); got == want {
		t.Fatal("a wrong CRC came out right")
	}
}

func TestOggLayout(t *testing.T) {
	pages := oggLayout([]int64{255 * 255, 100})
	if len(pages) != 2 || len(pages[0].lacing) != 255 || pages[0].granule != ^uint64(0) || pages[0].flags != 0 ||
		!bytes.Equal(pages[1].lacing, []byte{0, 100}) || pages[1].flags != oggContinued || pages[1].granule != 0 {
		t.Fatalf("layout %+v %+v", pages[0], pages[1])
	}
	if pages := oggLayout([]int64{255}); len(pages) != 1 || !bytes.Equal(pages[0].lacing, []byte{255, 0}) {
		t.Fatalf("255 bytes: %+v", pages[0])
	}
}

func TestOggVorbis(t *testing.T) {
	p := vorbisFile(t, []string{"TITLE=Old", "ARTIST=A", "TRACKNUMBER=3/12", "REPLAYGAIN_TRACK_GAIN=-6.0 dB"}, 255)
	before := readFile(t, p)
	info, err := Read(p)
	if err != nil || info.Format != "Ogg Vorbis" || !info.Writable || info.Fields[Title] != "Old" ||
		info.Fields[Track] != "3" || info.Fields[TrackTotal] != "12" || len(info.Notes) != 0 {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "A longer title", Album: "Álbum"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partName(p)); err == nil {
		t.Fatal("part file left")
	}
	after := readFile(t, p)
	sameAudio(t, before, after, 3)
	info, _ = Read(p)
	if info.Fields[Title] != "A longer title" || info.Fields[Album] != "Álbum" || info.Fields[Artist] != "A" {
		t.Fatalf("fields %#v", info.Fields)
	}
	cm, rest := oggComment(t, p)
	if len(rest) != 1+oggPadding || rest[0] != 1 || !strings.HasPrefix(cm.vendor, "Xiph") ||
		cm.list[len(cm.list)-2] != "REPLAYGAIN_TRACK_GAIN=-6.0 dB" {
		t.Fatalf("comment packet %#v, %d after", cm, len(rest))
	}
	// The padding takes the next change in place.
	st, _ := os.Stat(p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "Longer still, and in place"}}); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	if !os.SameFile(st, st2) || st2.Size() != st.Size() {
		t.Fatal("not written in place")
	}
	sameAudio(t, before, readFile(t, p), 3)
	if info, _ := Read(p); info.Fields[Title] != "Longer still, and in place" {
		t.Fatalf("fields %#v", info.Fields)
	}
}

func TestOggSpanningComment(t *testing.T) {
	big := "COMMENT=" + strings.Repeat("x", 200<<10)
	p := vorbisFile(t, []string{"TITLE=Big", big}, 255)
	before := readFile(t, p)
	bp, _ := pagesOf(before)
	if n := headerPages(bp, 3); n < 5 {
		t.Fatalf("headers on %d pages", n)
	}
	// Fewer header pages: the audio's sequence numbers go down.
	if err := Write(p, Changes{Set: map[Field]string{Comment: "short"}}); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, p)
	sameAudio(t, before, after, 3)
	ap, _ := pagesOf(after)
	if headerPages(ap, 3) != 2 {
		t.Fatalf("headers on %d pages", headerPages(ap, 3))
	}
	if info, _ := Read(p); info.Fields[Comment] != "short" || info.Fields[Title] != "Big" {
		t.Fatalf("fields %#v", info.Fields)
	}
	// And more again, with a picture.
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 50, 0, 60, 3}, bytes.Repeat([]byte{7}, 300<<10)...)
	if err := Write(p, Changes{Picture: &Picture{Data: jpeg}}); err != nil {
		t.Fatal(err)
	}
	sameAudio(t, before, readFile(t, p), 3)
	pic, err := ReadPicture(p)
	if err != nil || pic == nil || !bytes.Equal(pic.Data, jpeg) || pic.Type != 3 {
		t.Fatalf("picture %v", err)
	}
	info, _ := Read(p)
	if info.Picture == nil || info.Picture.Width != 60 || info.Picture.Size != len(jpeg) {
		t.Fatalf("picture %#v", info.Picture)
	}
	if err := Write(p, Changes{RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	if info, _ := Read(p); info.Picture != nil {
		t.Fatal("picture kept")
	}
	sameAudio(t, before, readFile(t, p), 3)
	if err := Write(p, Changes{Picture: &Picture{Data: append(jpeg, make([]byte, 1<<20)...)}}); !errors.Is(err, errOggPicture) {
		t.Fatalf("a big picture: %v", err)
	}
}

// A packet as long as a multiple of 255 ends with a 0 segment, which may be all its last page holds of it.
func TestOggLacingEdge(t *testing.T) {
	list := []string{"TITLE=Edge", "COMMENT="}
	n := 255*255 - len(vorbisCommentPacket(list, 0))
	list[1] += strings.Repeat("e", n)
	p := vorbisFile(t, list, 255)
	before := readFile(t, p)
	bp, _ := pagesOf(before)
	if len(bp[1].lacing) != 255 || bp[2].lacing[0] != 0 || bp[2].flags != oggContinued {
		t.Fatalf("fixture pages %v %v", len(bp[1].lacing), bp[2].lacing[:2])
	}
	if info, err := Read(p); err != nil || info.Fields[Title] != "Edge" || len(info.Fields[Comment]) != n {
		t.Fatalf("%v %#v", err, info)
	}
	// Same length: its pages keep their layout, in place.
	st, _ := os.Stat(p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "Egde"}}); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	after := readFile(t, p)
	if !os.SameFile(st, st2) || len(after) != len(before) {
		t.Fatal("not in place")
	}
	sameAudio(t, before, after, 3)
	if info, _ := Read(p); info.Fields[Title] != "Egde" {
		t.Fatalf("fields %#v", info.Fields)
	}
	// Too much room left over: a rewrite, the packet small again.
	if err := Write(p, Changes{Set: map[Field]string{Comment: ""}}); err != nil {
		t.Fatal(err)
	}
	if after = readFile(t, p); len(after) > len(before)-60000 {
		t.Fatalf("%d bytes, from %d", len(after), len(before))
	}
	sameAudio(t, before, after, 3)
	// A rewrite that makes the comment packet 255*255 bytes long itself.
	list = []string{"TITLE=Egde", "LYRICS="}
	m := 255*255 - oggPadding - len(vorbisCommentPacket(list, 0))
	if err := Write(p, Changes{Set: map[Field]string{Lyrics: strings.Repeat("l", m)}}); err != nil {
		t.Fatal(err)
	}
	after = readFile(t, p)
	sameAudio(t, before, after, 3)
	ap, _ := pagesOf(after)
	if len(ap[1].lacing) != 255 || ap[1].granule != ^uint64(0) || ap[2].lacing[0] != 0 || ap[2].flags != oggContinued ||
		ap[2].granule != 0 {
		t.Fatalf("pages %d %x, %v %d %x", len(ap[1].lacing), ap[1].granule, ap[2].lacing[:2], ap[2].flags, ap[2].granule)
	}
	if info, _ := Read(p); len(info.Fields[Lyrics]) != m || info.Fields[Comment] != "" {
		t.Fatalf("lyrics %d", len(info.Fields[Lyrics]))
	}
}

// Header pages laid out as another muxer does (8 segments a page here) stay so for a packet as long
// as before; a longer one is laid out as libvorbis does.
func TestOggOtherLayout(t *testing.T) {
	p := vorbisFile(t, []string{"TITLE=Old title", "ARTIST=A"}, 8)
	before := readFile(t, p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "New"}}); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, p)
	bp, _ := pagesOf(before)
	ap, _ := pagesOf(after)
	if len(after) != len(before) || len(ap) != len(bp) || headerPages(bp, 3) != 3 || len(ap[1].lacing) != 8 {
		t.Fatal("layout changed")
	}
	sameAudio(t, before, after, 3)
	if err := Write(p, Changes{Set: map[Field]string{Album: strings.Repeat("a", 1000)}}); err != nil {
		t.Fatal(err)
	}
	after = readFile(t, p)
	sameAudio(t, before, after, 3)
	ap, _ = pagesOf(after)
	if n := headerPages(ap, 3); n != 2 || len(ap[1].lacing) <= 8 {
		t.Fatalf("headers on %d pages, %d segments", n, len(ap[1].lacing))
	}
}

func TestOpus(t *testing.T) {
	// Data after the comments (its first byte odd) is kept; zeros there are room to write in place.
	keep := []byte{0x01, 'x', 'y'}
	p := writeTemp(t, "a.opus", oggStream(9, [][]byte{opusHead(), opusTags([]string{"TITLE=One", "ARTIST=Ä"}, keep)}, 255))
	before := readFile(t, p)
	info, err := Read(p)
	if err != nil || info.Format != "Opus" || !info.Writable || info.Fields[Artist] != "Ä" {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Two", Date: "2004"}}); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, p)
	sameAudio(t, before, after, 2)
	if cm, rest := oggComment(t, p); !bytes.Equal(rest, keep) || cm.vendor != "Lavf61" {
		t.Fatalf("after the comments: %q", rest)
	}
	if info, _ := Read(p); info.Fields[Title] != "Two" || info.Fields[Date] != "2004" {
		t.Fatalf("fields %#v", info.Fields)
	}

	p = writeTemp(t, "b.opus", oggStream(9, [][]byte{opusHead(), opusTags([]string{"TITLE=One"}, make([]byte, 512))}, 255))
	before = readFile(t, p)
	st, _ := os.Stat(p)
	if err := Write(p, Changes{Set: map[Field]string{Title: "A title longer than One", Genre: "Jazz"}}); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	after = readFile(t, p)
	if !os.SameFile(st, st2) || len(after) != len(before) {
		t.Fatal("not in place")
	}
	sameAudio(t, before, after, 2)
	if info, _ := Read(p); info.Fields[Title] != "A title longer than One" || info.Fields[Genre] != "Jazz" {
		t.Fatalf("fields %#v", info.Fields)
	}
}

func TestOggReadOnly(t *testing.T) {
	cm := (&comments{vendor: "v", list: []string{"TITLE=Flac in Ogg"}}).bytes()
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 5, 0, 6, 3}
	pic := encodePicture(&Picture{Type: 3, Data: jpeg})
	first := append([]byte("\x7fFLAC\x01\x00\x00\x02fLaC"), flacHeader(flacStreamInfo, false, 34)...)
	first = append(first, make([]byte, 34)...)
	flac := oggStream(3, [][]byte{first, append(flacHeader(flacComment, false, len(cm)), cm...),
		append(flacHeader(flacPicture, true, len(pic)), pic...)}, 255)
	speex := append([]byte("Speex   "), make([]byte, 72)...)
	two := append(paginate(1, 0, [][]byte{[]byte("fishead\x00rest")}, []int64{0}, 255, func(int) bool { return true },
		true, false), oggStream(2, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=Muxed"}, 0),
		[]byte("\x05vorbis")}, 255)...)
	// Its setup packet's page has audio on it too.
	audio, ag := audioPackets(20)
	untidy := paginate(4, 0, append([][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=Untidy"}, 0),
		[]byte("\x05vorbis")}, audio...), append([]int64{0, 0, 0}, ag...), 255,
		func(i int) bool { return i == 0 || i > 3 && i%8 == 0 }, true, true)
	for _, c := range []struct {
		name, data, format, title string
	}{
		{"flac.oga", string(flac), "Ogg FLAC", "Flac in Ogg"},
		{"speex.ogg", string(oggStream(5, [][]byte{speex, cm}, 255)), "Speex", "Flac in Ogg"},
		{"muxed.ogg", string(two), "Ogg Vorbis", "Muxed"},
		{"untidy.ogg", string(untidy), "Ogg Vorbis", "Untidy"},
	} {
		p := writeTemp(t, c.name, []byte(c.data))
		info, err := Read(p)
		if err != nil || info.Format != c.format || info.Writable || info.Fields[Title] != c.title || len(info.Notes) != 1 {
			t.Fatalf("%s: %v %#v", c.name, err, info)
		}
		if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrReadOnly {
			t.Fatalf("%s: written: %v", c.name, err)
		}
		if c.format == "Ogg FLAC" && (info.Picture == nil || info.Picture.Width != 6) {
			t.Fatalf("FLAC picture %#v", info.Picture)
		}
	}
}

func TestOggChained(t *testing.T) {
	setup := []byte("\x05vorbis\x01\x02")
	one := oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One"}, 0), setup}, 255)
	two := oggStream(2, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=Two"}, 0), setup}, 255)
	p := writeTemp(t, "a.ogg", append(append([]byte(nil), one...), two...))
	info, err := Read(p)
	if err != nil || !info.Writable || info.Fields[Title] != "One" || len(info.Notes) != 1 {
		t.Fatalf("%v %#v", err, info)
	}
	// Lyrics long enough for another header page, so the first stream's pages are numbered again.
	if err := Write(p, Changes{Set: map[Field]string{Title: "The first of two", Lyrics: strings.Repeat("l", 65000)}}); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, p)
	if !bytes.HasSuffix(after, two) {
		t.Fatal("the second stream changed")
	}
	sameAudio(t, one, after[:len(after)-len(two)], 3)
	if ap, _ := pagesOf(after); headerPages(ap, 3) != 3 {
		t.Fatalf("headers on %d pages", headerPages(ap, 3))
	}
	if info, _ := Read(p); info.Fields[Title] != "The first of two" {
		t.Fatalf("fields %#v", info.Fields)
	}
}

// A page with a wrong CRC stays wrong; bytes after the last page stay as they were.
func TestOggDamage(t *testing.T) {
	b := oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One"}, 0), []byte("\x05vorbis")}, 255)
	pages, _ := pagesOf(b)
	off := 0
	for _, p := range pages[:5] {
		off += 27 + len(p.lacing) + len(p.body)
	}
	b[off+27+len(pages[5].lacing)+5] ^= 0xFF // the sixth page's data
	b = append(b, append([]byte("TAG"), make([]byte, 125)...)...)
	p := writeTemp(t, "a.ogg", b)
	// Lyrics long enough for another header page, so the pages after are numbered again.
	if err := Write(p, Changes{Set: map[Field]string{Lyrics: strings.Repeat("l", 65000)}}); err != nil {
		t.Fatal(err)
	}
	ap, rest := pagesOf(readFile(t, p))
	for i, pg := range ap {
		if pg.crcOK == (i == 6) {
			t.Fatalf("page %d: CRC right is %v", i, pg.crcOK)
		}
		if i > 0 && pg.seq != ap[i-1].seq+1 {
			t.Fatalf("page %d: sequence %d", i, pg.seq)
		}
	}
	if len(ap) != len(pages)+1 || !bytes.Equal(ap[6].body, pages[5].body) || len(rest) != 128 ||
		string(rest[:3]) != "TAG" {
		t.Fatalf("%d pages, %d after", len(ap), len(rest))
	}
	// A header page with a wrong CRC isn't written.
	b = oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One"}, 0), []byte("\x05vorbis")}, 255)
	off = 28 + len(pages[0].body)
	b[off+27+len(pages[1].lacing)+15] ^= 1 // in the vendor string
	p = writeTemp(t, "b.ogg", b)
	// Its tags read, but it says up front that they can't be changed.
	if info, err := Read(p); err != nil || info.Fields[Title] != "One" || info.Writable || len(info.Notes) != 1 ||
		!strings.Contains(info.Notes[0], "damaged") {
		t.Fatalf("bad header page read: %v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "x"}}); err != ErrCorrupt {
		t.Fatalf("bad header page: %v", err)
	}
	if err := Write(writeTemp(t, "c.ogg", []byte("OggS\x00\x02 short")), Changes{Set: map[Field]string{Title: "x"}}); err != ErrCorrupt {
		t.Fatalf("short file: %v", err)
	}
}

func TestOggTooBig(t *testing.T) {
	p := vorbisFile(t, []string{"TITLE=One", "COMMENT=" + strings.Repeat("c", oggMaxComment)}, 255)
	if info, err := Read(p); err != nil || !info.Writable || len(info.Notes) != 1 ||
		!strings.Contains(info.Notes[0], "tags are too big") {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Two"}}); err != ErrTooBig {
		t.Fatalf("a 4 MB comment packet: %v", err)
	}
}

// An ID3 tag some taggers put before the Ogg data is kept as it was.
func TestOggID3Before(t *testing.T) {
	id3 := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x14"), make([]byte, 20)...)
	one := oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One"}, 0), []byte("\x05vorbis")}, 255)
	p := writeTemp(t, "a.ogg", append(append([]byte(nil), id3...), one...))
	info, err := Read(p)
	if err != nil || !info.Writable || info.Fields[Title] != "One" || len(info.Notes) != 1 {
		t.Fatalf("%v %#v", err, info)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Two", Lyrics: strings.Repeat("l", 65000)}}); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, p)
	if !bytes.HasPrefix(after, id3) {
		t.Fatal("the ID3 tag changed")
	}
	sameAudio(t, one, after[len(id3):], 3)
}

// A file of nothing but empty packets or empty pages is given up on before it takes the memory: the
// headers are read from a few pages, not from every page there is.
func TestOggHeadBounded(t *testing.T) {
	// Ogg FLAC whose first packet counts no headers waits for the last: zero-length packets never are.
	first := append([]byte("\x7fFLAC\x01\x00\x00\x00fLaC\x00\x00\x00\x22"), make([]byte, 34)...)
	var a bytes.Buffer
	a.Write(paginate(7, 0, [][]byte{first}, []int64{0}, 255, func(int) bool { return true }, true, false))
	lacing := bytes.Repeat([]byte{0}, 255)
	for i := 1; i <= 4000; i++ { // 1.1 MB
		p := &oggPage{serial: 7, seq: uint32(i), lacing: lacing}
		h := p.header()
		binary.LittleEndian.PutUint32(h[22:], pageCRC(h))
		a.Write(h)
	}
	// Vorbis followed by pages with no segments at all.
	var b bytes.Buffer
	b.Write(paginate(7, 0, [][]byte{vorbisID()}, []int64{0}, 255, func(int) bool { return true }, true, false))
	for i := 1; i <= 100000; i++ { // 2.7 MB
		p := &oggPage{serial: 7, seq: uint32(i), granule: ^uint64(0)}
		h := p.header()
		binary.LittleEndian.PutUint32(h[22:], pageCRC(h))
		b.Write(h)
	}
	for _, c := range []struct {
		name string
		data []byte
	}{{"empty packets", a.Bytes()}, {"empty pages", b.Bytes()}} {
		var err error
		n := oggAllocated(func() { _, err = readOggHead(bytes.NewReader(c.data), int64(len(c.data))) })
		if err != ErrCorrupt || n > 4<<20 {
			t.Fatalf("%s: %v, %d KB allocated for a file of %d KB", c.name, err, n>>10, len(c.data)>>10)
		}
		if _, err := Read(writeTemp(t, "a.ogg", c.data)); err != ErrCorrupt {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// Junk between audio pages: the stream's pages after it are numbered on, the junk kept where it was;
// after junk, a page of another stream ends the numbering.
func TestOggJunkBetweenPages(t *testing.T) {
	setup := append([]byte("\x05vorbis"), bytes.Repeat([]byte{0x5A}, 300)...)
	head := [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One"}, 0), setup}
	clean := oggStream(1, head, 4)
	pages, _ := pagesOf(clean)
	off, at := 0, 0
	for i, p := range pages {
		if i > 10 && p.flags&oggContinued != 0 { // a packet that goes on past the junk
			at = off
			break
		}
		off += 27 + len(p.lacing) + len(p.body)
	}
	if at == 0 {
		t.Fatal("no continued page")
	}
	junk := []byte("JUNKJUNK")
	b := append(append(append([]byte(nil), clean[:at]...), junk...), clean[at:]...)
	p, q := writeTemp(t, "a.ogg", b), writeTemp(t, "b.ogg", clean)
	// Lyrics long enough for another header page, so the pages after are numbered again.
	set := map[Field]string{Lyrics: strings.Repeat("l", 65000)}
	if err := Write(p, Changes{Set: set}); err != nil {
		t.Fatal(err)
	}
	if err := Write(q, Changes{Set: set}); err != nil {
		t.Fatal(err)
	}
	after, want := readFile(t, p), readFile(t, q)
	sameAudio(t, clean, want, 3)
	i := bytes.Index(after, junk)
	if i < 0 || !bytes.Equal(append(after[:i:i], after[i+len(junk):]...), want) {
		t.Fatal("not the same edit as the file's without the junk, with the junk where it was")
	}

	// A stream with no last page, junk, then another stream: that one is left as it was.
	audio, ag := audioPackets(30)
	one := paginate(1, 0, append(append([][]byte(nil), head...), audio...), append(make([]int64, 3), ag...), 255,
		func(i int) bool { return i == 0 || i == 2 || i > 2 && i%8 == 0 }, true, false)
	two := oggStream(2, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=Two"}, 0), setup}, 255)
	tail := append(append([]byte("JUNK"), two...), "JUNK"...)
	p = writeTemp(t, "c.ogg", append(append([]byte(nil), one...), tail...))
	if err := Write(p, Changes{Set: set}); err != nil {
		t.Fatal(err)
	}
	after = readFile(t, p)
	if !bytes.HasSuffix(after, tail) {
		t.Fatal("the second stream changed")
	}
	sameAudio(t, one, after[:len(after)-len(tail)], 3)
}

// An Ogg song whose cover is too big to keep says so when read; a save that keeps it says why it
// can't be, and leaves the file as it was; one that removes or replaces it saves.
func TestOggBigCover(t *testing.T) {
	pic, jpeg := oggCover(3200 << 10)
	for _, c := range []struct {
		name    string
		data    []byte
		headers int
	}{
		{"a.ogg", oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket([]string{"TITLE=One", pic}, 0),
			append([]byte("\x05vorbis"), make([]byte, 400)...)}, 255), 3},
		{"a.opus", oggStream(9, [][]byte{opusHead(), opusTags([]string{"TITLE=One", pic}, nil)}, 255), 2},
	} {
		p := writeTemp(t, c.name, c.data)
		info, err := Read(p)
		if err != nil || !info.Writable || info.Picture == nil || info.Picture.Size != len(jpeg) || len(info.Notes) != 1 ||
			!strings.Contains(info.Notes[0], "cover is too big to keep") {
			t.Fatalf("%s: %v %#v", c.name, err, info)
		}
		err = Write(p, Changes{Set: map[Field]string{Title: "Two"}})
		if err != errOggCover || errors.Is(err, ErrTooBig) || !strings.HasPrefix(err.Error(), "tags: ") {
			t.Fatalf("%s: keeping the cover: %v", c.name, err)
		}
		if !bytes.Equal(readFile(t, p), c.data) {
			t.Fatalf("%s: changed", c.name)
		}
		small := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 11, 8, 0, 5, 0, 6, 3}
		if err := Write(p, Changes{Set: map[Field]string{Title: "Two"}, Picture: &Picture{Data: small}}); err != nil {
			t.Fatalf("%s: a smaller cover: %v", c.name, err)
		}
		sameAudio(t, c.data, readFile(t, p), c.headers)
		if info, _ := Read(p); info.Fields[Title] != "Two" || info.Picture == nil || info.Picture.Width != 6 || len(info.Notes) != 0 {
			t.Fatalf("%s: %#v", c.name, info)
		}
		p = writeTemp(t, "b"+c.name, c.data)
		if err := Write(p, Changes{Set: map[Field]string{Title: "Two"}, RemovePicture: true}); err != nil {
			t.Fatalf("%s: no cover: %v", c.name, err)
		}
		sameAudio(t, c.data, readFile(t, p), c.headers)
		if info, _ := Read(p); info.Fields[Title] != "Two" || info.Picture != nil || len(info.Notes) != 0 {
			t.Fatalf("%s: %#v", c.name, info)
		}
	}
}

// A cover that takes the headers past oggMaxHeaders, in pages of 4 KB as mutagen makes them, leaves
// its tags readable: the cover is read from the file, not held with the headers. One over oggMaxCover
// isn't read at all, and a note says so.
func TestOggHugeCover(t *testing.T) {
	pic, jpeg := oggCover(6500 << 10)
	p := vorbisFile(t, []string{"TITLE=One", "ARTIST=A", pic}, 16)
	before := readFile(t, p)
	var info *Info
	var err error
	n := oggAllocated(func() { info, err = Read(p) })
	if err != nil || !info.Writable || info.Fields[Title] != "One" || info.Fields[Artist] != "A" || info.Picture == nil ||
		info.Picture.Size != len(jpeg) || info.Picture.Width != 60 || len(info.Notes) != 1 {
		t.Fatalf("%v %#v", err, info)
	}
	if n > uint64(len(jpeg))+2<<20 {
		t.Fatalf("%d KB allocated to read it", n>>10)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, jpeg) {
		t.Fatalf("picture: %v", err)
	}
	n = oggAllocated(func() { err = Write(p, Changes{Set: map[Field]string{Title: "Two"}, RemovePicture: true}) })
	if err != nil || n > uint64(len(jpeg))+3<<20 {
		t.Fatalf("%v, %d KB allocated to save it", err, n>>10)
	}
	sameAudio(t, before, readFile(t, p), 3)
	if info, _ := Read(p); info.Fields[Title] != "Two" || info.Picture != nil || len(info.Notes) != 0 {
		t.Fatalf("%#v", info)
	}

	pic, _ = oggCover(oggMaxCover + 1<<10)
	p = vorbisFile(t, []string{"TITLE=One", pic}, 16)
	before = readFile(t, p)
	n = oggAllocated(func() { info, err = Read(p) })
	if err != nil || info.Fields[Title] != "One" || info.Picture != nil || len(info.Notes) != 2 ||
		!strings.Contains(info.Notes[0], "over 8 MB") || n > 2<<20 {
		t.Fatalf("%v %#v, %d KB allocated", err, info, n>>10)
	}
	if pic, err := ReadPicture(p); err != nil || pic != nil {
		t.Fatalf("picture: %v", err)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Two"}}); err != errOggCover {
		t.Fatalf("keeping it: %v", err)
	}
	if err := Write(p, Changes{RemovePicture: true}); err != nil {
		t.Fatal(err)
	}
	sameAudio(t, before, readFile(t, p), 3)
}

// A cover too long to hold stays in the file through a save that keeps it: in place, read from where
// it was, moving towards the packet's start, never its end (it goes first when it would); in a file
// written again, copied from the old one. Neither save holds it.
func TestOggCoverLeftInFile(t *testing.T) {
	pic, jpeg := oggCover(2 << 20)
	odd := "X=\x00\x00"                              // NULs, as a placeholder has
	data := "X-DATA=" + strings.Repeat("d", 100<<10) // too long to hold, not a picture
	setup := append([]byte("\x05vorbis"), bytes.Repeat([]byte{0x5A, 0xA5, 0x3C}, 1200)...)
	list := []string{"TITLE=One", odd, pic, data, "ARTIST=A"}
	p := writeTemp(t, "a.ogg", oggStream(1, [][]byte{vorbisID(), vorbisCommentPacket(list, 4096), setup}, 255))
	before := readFile(t, p)
	check := func(want []string, inPlace bool) {
		t.Helper()
		after := readFile(t, p)
		if inPlace != (len(after) == len(before)) {
			t.Fatalf("%d bytes, from %d", len(after), len(before))
		}
		sameAudio(t, before, after, 3)
		cm, _ := oggComment(t, p)
		if strings.Join(cm.list, "\n") != strings.Join(want, "\n") {
			t.Fatalf("comments %.40q", cm.list)
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, jpeg) {
			t.Fatalf("picture: %v", err)
		}
	}
	save := func(set map[Field]string) {
		t.Helper()
		var err error
		if n := oggAllocated(func() { err = oggCodec{}.write(p, Changes{Set: set}) }); err != nil || n > 1<<20 {
			t.Fatalf("%v, %d KB allocated", err, n>>10)
		}
	}
	st, _ := os.Stat(p)
	// Shorter before the cover: in place, the order kept.
	save(map[Field]string{Title: "On"})
	check([]string{"TITLE=On", odd, pic, data, "ARTIST=A"}, true)
	// Longer: in place, the entries left in the file first.
	save(map[Field]string{Title: "Three"})
	check([]string{pic, data, "TITLE=Three", odd, "ARTIST=A"}, true)
	if st2, _ := os.Stat(p); !os.SameFile(st, st2) {
		t.Fatal("not in place")
	}
	// Longer than the padding takes: written again.
	save(map[Field]string{Lyrics: strings.Repeat("l", 30000)})
	check([]string{pic, data, "TITLE=Three", odd, "ARTIST=A", "LYRICS=" + strings.Repeat("l", 30000)}, false)

	// Opus's data after the comments is kept with them, in place and written again.
	keep := []byte{0x01, 'x', 'y'}
	b := oggStream(9, [][]byte{opusHead(), opusTags([]string{"TITLE=One", pic}, keep)}, 255)
	p = writeTemp(t, "a.opus", b)
	for _, title := range []string{"Two", "Three"} {
		if err := Write(p, Changes{Set: map[Field]string{Title: title}}); err != nil {
			t.Fatal(err)
		}
		after := readFile(t, p)
		if inPlace := len(after) == len(b); inPlace != (title == "Two") {
			t.Fatalf("%s: in place %v", title, inPlace)
		}
		sameAudio(t, b, after, 2)
		cm, rest := oggComment(t, p)
		if !bytes.Equal(rest, keep) || len(cm.list) != 2 || cm.list[0] != "TITLE="+title {
			t.Fatalf("%s: %.40q, %q after", title, cm.list, rest)
		}
		if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, jpeg) {
			t.Fatalf("picture: %v", err)
		}
	}
}

// An entry left in the file stands in the list as a placeholder of a few bytes, counted as held,
// however long a run of NULs a held entry has: what a read or a save takes doesn't grow with that run
// times the entries left in the file.
func TestOggPlaceholdersSmall(t *testing.T) {
	const run = 1 << 20
	blob := "X-BLOB=" + strings.Repeat("a", 66<<10)
	file := func(refs int) (string, []string) {
		list := []string{"TITLE=One", "LYRICS=" + strings.Repeat("\x00", run)}
		for i := 0; i < refs; i++ {
			list = append(list, blob)
		}
		return vorbisFile(t, list, 255), list
	}
	p, list := file(30)
	q, _ := file(1)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	h, err := readOggHead(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	tg, err := h.comment(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.refs) != 30 {
		t.Fatalf("%d left in the file", len(tg.refs))
	}
	var marks int64
	for e := range tg.refs {
		if len(e) > len("X-BLOB=")+8 {
			t.Fatalf("a placeholder of %d bytes", len(e))
		}
		marks += int64(len(e))
	}
	if h.held < run+marks || h.held > run+1<<20 {
		t.Fatalf("%d bytes counted as held", h.held)
	}

	read := func(p string) uint64 {
		return oggAllocated(func() {
			if info, err := Read(p); err != nil || info.Fields[Title] != "One" {
				t.Fatalf("%v %#v", err, info)
			}
		})
	}
	if n, m := read(p), read(q); n > m+1<<20 || n > 4*run {
		t.Fatalf("read: %d KB allocated with 30 entries left in the file, %d KB with one", n>>10, m>>10)
	}
	write := func(p string) uint64 {
		return oggAllocated(func() {
			if err := Write(p, Changes{Set: map[Field]string{Title: "Two"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if n, m := write(p), write(q); n > m+2<<20 || n > 8*run {
		t.Fatalf("save: %d KB allocated with 30 entries left in the file, %d KB with one", n>>10, m>>10)
	}
	list[0] = "TITLE=Two"
	if cm, _ := oggComment(t, p); strings.Join(cm.list, "\n") != strings.Join(list, "\n") {
		t.Fatalf("comments %.40q", cm.list)
	}
}

// An entry read that is what a placeholder would be makes the placeholder longer, not the same.
func TestOggPlaceholderTaken(t *testing.T) {
	pic, jpeg := oggCover(100 << 10)
	blob := "X-BLOB=" + strings.Repeat("b", 70<<10)
	list := []string{"TITLE=One", "X-BLOB=\x000", "X-BLOB=\x00\x000", blob, pic, blockPicture + "=\x001"}
	p := vorbisFile(t, list, 255)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	h, err := readOggHead(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	tg, err := h.comment(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TITLE=One", "X-BLOB=\x000", "X-BLOB=\x00\x000", "X-BLOB=\x00\x00\x000",
		blockPicture + "=\x00\x001", blockPicture + "=\x001"}
	if strings.Join(tg.cm.list, "\n") != strings.Join(want, "\n") || len(tg.refs) != 2 {
		t.Fatalf("%q", tg.cm.list)
	}
	if err := Write(p, Changes{Set: map[Field]string{Title: "Three"}}); err != nil {
		t.Fatal(err)
	}
	list[0] = "TITLE=Three"
	if cm, _ := oggComment(t, p); strings.Join(cm.list, "\n") != strings.Join(list, "\n") {
		t.Fatalf("comments %.40q", cm.list)
	}
	if pic, err := ReadPicture(p); err != nil || !bytes.Equal(pic.Data, jpeg) {
		t.Fatalf("picture: %v", err)
	}
}

// oggPic is a METADATA_BLOCK_PICTURE entry of type typ holding a JPEG w px wide and n bytes long;
// when bad > 0 its base64 is broken from that character on, so its head reads but its bytes don't.
func oggPic(typ byte, w, n, bad int) string {
	b := encodePicture(&Picture{MIME: "image/jpeg", Data: smallJPEG(w, 10, n)})
	binary.BigEndian.PutUint32(b, uint32(typ))
	s := []byte(base64.StdEncoding.EncodeToString(b))
	for i := bad; bad > 0 && i < len(s); i++ {
		s[i] = '*'
	}
	return blockPicture + "=" + string(s)
}

// countedReader counts the bytes read through it.
type countedReader struct {
	r io.ReaderAt
	n int64
}

func (c *countedReader) ReadAt(b []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(b, off)
	c.n += int64(n)
	return n, err
}

// The front cover is chosen over the pictures before it; one whose bytes don't read gives way to the
// next, front covers first, for oggMaxTries of them in all, then to an old COVERART.
func TestOggPictureChoice(t *testing.T) {
	front := func(w int) string { return oggPic(3, w, 2000, 0) }
	other := func(w int) string { return oggPic(4, w, 2000, 0) }
	broken := func(typ byte) string { return oggPic(typ, 99, 2000, 400) }
	art := "COVERART=" + base64.StdEncoding.EncodeToString(smallJPEG(77, 10, 500))
	for i, c := range []struct {
		list  []string
		width int
	}{
		{[]string{other(11), broken(3), front(22), front(33)}, 22},
		{[]string{other(11), broken(3), broken(4), other(44)}, 11},
		{[]string{broken(3), broken(4), other(44)}, 44},
		{[]string{broken(3), art}, 77},
		// Past oggMaxTries pictures that don't read, none is shown.
		{[]string{broken(3), broken(3), broken(3), broken(3), front(22)}, 0},
		{[]string{broken(4), broken(3), broken(4), broken(4), art}, 0},
	} {
		info, err := Read(vorbisFile(t, append([]string{"TITLE=One"}, c.list...), 255))
		if err != nil || info.Fields[Title] != "One" {
			t.Fatalf("%d: %v %#v", i, err, info)
		}
		if info.Picture == nil && c.width != 0 || info.Picture != nil && info.Picture.Width != c.width {
			t.Fatalf("%d: picture %#v, not %d px wide", i, info.Picture, c.width)
		}
	}
}

// Many pictures whose heads read but whose bytes don't cost a read of each head and a few whole
// reads, not a read of every head again after each whole read that fails.
func TestOggPictureScanLinear(t *testing.T) {
	// Held: pictures of 1 KB, broken past their first KB of base64.
	const n = 3000
	list := []string{"TITLE=One"}
	for i := 0; i < n; i++ {
		list = append(list, oggPic(4, 50, 1100, 1420))
	}
	p := vorbisFile(t, list, 255)
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	info, err := Read(p)
	runtime.ReadMemStats(&m1)
	if err != nil || info.Fields[Title] != "One" || info.Picture != nil {
		t.Fatalf("%v %#v", err, info)
	}
	if k := m1.Mallocs - m0.Mallocs; k > 40*n+100000 {
		t.Fatalf("%d allocations to read %d pictures", k, n)
	}

	// Left in the file: pictures of 70 KB, broken near their end.
	const m = 48
	big := oggPic(4, 50, 52<<10, 60000)
	list = []string{"TITLE=One"}
	for i := 0; i < m; i++ {
		list = append(list, big)
	}
	p = vorbisFile(t, list, 255)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	h, err := readOggHead(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	tg, err := h.comment(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.refs) != m {
		t.Fatalf("%d left in the file", len(tg.refs))
	}
	r := &countedReader{r: f}
	if pic, over := tg.picture(h, r); pic != nil || over {
		t.Fatalf("%v %v", pic, over)
	}
	if most := int64(oggMaxTries*len(big) + m*4<<10); r.n > most {
		t.Fatalf("%d KB read for %d pictures of %d KB", r.n>>10, m, len(big)>>10)
	}
}

// A reader of the comment packet from anywhere in it reads what a reader from its start does there.
func TestOggCommentReader(t *testing.T) {
	pic, _ := oggCover(40 << 10)
	p := vorbisFile(t, []string{"TITLE=One", pic, "ARTIST=A"}, 16) // pages of 4 KB
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	h, err := readOggHead(f, st.Size())
	if err != nil || len(h.cspans) < 10 {
		t.Fatalf("%v, %d spans", err, len(h.cspans))
	}
	all, err := io.ReadAll(h.commentReader(f, 0, h.clen))
	if err != nil || int64(len(all)) != h.clen {
		t.Fatalf("%v, %d bytes of %d", err, len(all), h.clen)
	}
	offs := []int64{0, 1, h.clen - 1}
	for _, s := range h.cstarts {
		offs = append(offs, s-1, s, s+1)
	}
	for _, off := range offs {
		if off < 0 || off >= h.clen {
			continue
		}
		n := min(5000, h.clen-off)
		b, err := io.ReadAll(h.commentReader(f, off, n))
		if err != nil || !bytes.Equal(b, all[off:off+n]) {
			t.Fatalf("from %d: %v", off, err)
		}
	}
}
