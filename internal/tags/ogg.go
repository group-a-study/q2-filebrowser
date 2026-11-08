package tags

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Ogg (.ogg, .oga, .opus): pages, each "OggS", flags, a granule position, its stream's serial number,
// its own sequence number, a CRC and a segment table, carrying the packets of one stream or more. A
// Vorbis stream starts with three header packets (identification, comments, setup), an Opus stream
// with two (OpusHead, OpusTags). The tags are the comment packet's Vorbis comments, as FLAC has them,
// pictures among them as METADATA_BLOCK_PICTURE.
//
// The comment packet isn't held whole: its entries are read from the file one by one, and one too
// long to hold that isn't a field's (a cover, mostly) stays there, read from there when it is wanted
// and copied from there when it is kept.
//
// A write makes the comment packet again and lays the header pages out as libvorbis does: the
// identification header alone on the first page (kept as it was), the other headers from the second
// page on, 255 segments a page, the last page ending with them so the audio starts on a page of its
// own. Every audio page is copied as it was but for its sequence number, moved by however many pages
// the headers gained or lost, and its CRC: granule positions and packets don't change. When the new
// packet is as long as the old one (zeros after the comments make up the difference, as Opus allows
// and taggers do for Vorbis) the header pages keep their layout and are written in place, an entry
// left in the file never moving towards the packet's end (those entries go first when one would).
//
// Ogg FLAC and Speex are read, not written; so is a file with several streams at once (a video's, a
// Skeleton). In a chain of streams, one after another, only the first is changed.

func init() { register(oggCodec{}, ".ogg", ".oga", ".opus") }

type oggCodec struct{}

const (
	oggContinued = 0x01
	oggBOS       = 0x02
	oggEOS       = 0x04

	oggMaxHeaders = 8 << 20  // the most header bytes held
	oggMaxPages   = 16 << 10 // the most pages read for the headers: 64 MB of them in pages of 4 KB, as mutagen makes
	oggMaxPackets = 1 << 10  // the most header packets
	oggMaxEntries = 16 << 10 // the most comments
	oggHoldEntry  = 64 << 10 // the longest comment held that isn't a field's; a longer one stays in the file
	oggMaxCover   = 8 << 20  // the biggest picture read from the comments
	oggMaxTries   = 4        // the most pictures read whole to find one whose bytes read
	oggMaxComment = 4 << 20  // the biggest comment packet written: the Q2's TagLib holds it four times over
	oggMaxPicture = 1 << 20  // the biggest picture written
	oggPadding    = 2 << 10  // zeros after the comments of a packet laid out again, for next time
	oggMaxPadding = 16 << 10 // the most zeros kept to stay in place
)

var (
	errOggPicture = errors.New("tags: the picture is over 1 MB, more than an Ogg or Opus song should hold")
	errOggCover   = errors.New("tags: its cover is too big to keep in an Ogg song; remove it or choose a smaller one")
)

// oggPage is a page's header.
type oggPage struct {
	off     int64 // where it is in the file
	flags   byte
	granule uint64
	serial  uint32
	seq     uint32
	crc     uint32
	lacing  []byte // the segment table
	body    int    // the data's length
	ok      bool   // its CRC is right (header pages only)
}

func (p *oggPage) hlen() int64 { return int64(27 + len(p.lacing)) }

func (p *oggPage) size() int64 { return p.hlen() + int64(p.body) }

// parseOggHeader reads the page header at the start of b.
func parseOggHeader(b []byte) (*oggPage, bool) {
	if len(b) < 27 || string(b[:4]) != "OggS" || b[4] != 0 || len(b) < 27+int(b[26]) {
		return nil, false
	}
	p := &oggPage{flags: b[5], granule: binary.LittleEndian.Uint64(b[6:]), serial: binary.LittleEndian.Uint32(b[14:]),
		seq: binary.LittleEndian.Uint32(b[18:]), crc: binary.LittleEndian.Uint32(b[22:]),
		lacing: append([]byte(nil), b[27:27+int(b[26])]...)}
	for _, l := range p.lacing {
		p.body += int(l)
	}
	return p, true
}

// readFull fills b from off; ErrCorrupt when the file is shorter.
func readFull(r io.ReaderAt, b []byte, off int64) error {
	k, err := r.ReadAt(b, off)
	if k == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = ErrCorrupt
	}
	return err
}

// readOggPage reads the page header at off into buf (27+255 bytes or more); ErrCorrupt when there
// isn't one, or its page runs past the end.
func readOggPage(r io.ReaderAt, off, size int64, buf []byte) (*oggPage, error) {
	n := min(27+255, size-off)
	if n < 27 {
		return nil, ErrCorrupt
	}
	if err := readFull(r, buf[:n], off); err != nil {
		return nil, err
	}
	p, ok := parseOggHeader(buf[:n])
	if !ok || off+p.size() > size {
		return nil, ErrCorrupt
	}
	p.off = off
	return p, nil
}

// header is p's header as written, with p.crc in it.
func (p *oggPage) header() []byte {
	h := make([]byte, p.hlen())
	copy(h, "OggS")
	h[5] = p.flags
	binary.LittleEndian.PutUint64(h[6:], p.granule)
	binary.LittleEndian.PutUint32(h[14:], p.serial)
	binary.LittleEndian.PutUint32(h[18:], p.seq)
	binary.LittleEndian.PutUint32(h[22:], p.crc)
	h[26] = byte(len(p.lacing))
	copy(h[27:], p.lacing)
	return h
}

// Ogg's CRC-32: polynomial 0x04C11DB7, not reflected, no initial value, no final xor.
var oggTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func oggCRC(c uint32, b []byte) uint32 {
	for _, x := range b {
		c = c<<8 ^ oggTable[byte(c>>24)^x]
	}
	return c
}

// pageCRC is the CRC of a page with header h, its CRC field taken as zero, and data body.
func pageCRC(h []byte, body ...[]byte) uint32 {
	c := oggCRC(0, h[:22])
	c = oggCRC(c, []byte{0, 0, 0, 0})
	c = oggCRC(c, h[26:])
	for _, b := range body {
		c = oggCRC(c, b)
	}
	return c
}

// renumbered is the CRC of a page of n bytes, stored with crc, once its sequence number goes from old
// to seq. The CRC is linear (no initial value, no final xor), so the change is the CRC of the change
// alone: a page whose CRC was wrong stays wrong, and the page's data needn't be looked at.
func renumbered(crc, old, seq uint32, n int64) uint32 {
	var d [4]byte
	binary.LittleEndian.PutUint32(d[:], old^seq)
	c := oggCRC(0, d[:])
	for i := int64(22); i < n; i++ { // the zeros after it
		c = c<<8 ^ oggTable[c>>24]
	}
	return crc ^ c
}

// oggSpan is n bytes from off: of the file, or of the comment packet.
type oggSpan struct{ off, n int64 }

// oggHead is the start of an Ogg file: its stream's header pages and packets.
type oggHead struct {
	start   int64  // where the Ogg data starts: after an ID3 tag some taggers put first
	kind    string // "vorbis", "opus", "flac", "speex", "" for another codec
	serial  uint32
	pages   []*oggPage // the stream's pages, from its first to the one its last header packet ends on
	packets [][]byte   // its header packets; nil for the comment packet of Vorbis, Opus or Speex, left in the file
	cspans  []oggSpan  // where in the file that comment packet's bytes are, a span a page
	cstarts []int64    // where in the packet each span starts
	clen    int64      // its length
	held    int64      // header bytes held
	end     int64      // where that last header page ends
	muxed   bool       // other streams' pages are among its own
	tidy    bool       // the first packet alone on the first page, nothing after the headers on the last
	damaged bool       // a header page's CRC is wrong
}

// oggKind is the codec whose first packet starts with b.
func oggKind(b []byte) string {
	switch {
	case len(b) >= 7 && b[0] == 1 && string(b[1:7]) == "vorbis":
		return "vorbis"
	case bytes.HasPrefix(b, []byte("OpusHead")):
		return "opus"
	case len(b) >= 5 && b[0] == 0x7F && string(b[1:5]) == "FLAC":
		return "flac"
	case bytes.HasPrefix(b, []byte("Speex   ")):
		return "speex"
	}
	return ""
}

// spills is whether h's comment packet (its second) is left in the file, its entries read from there.
func (h *oggHead) spills() bool { return h.kind == "vorbis" || h.kind == "opus" || h.kind == "speex" }

// hold counts n more header bytes held; ErrTooBig past oggMaxHeaders.
func (h *oggHead) hold(n int64) error {
	if h.held += n; h.held > oggMaxHeaders {
		return ErrTooBig
	}
	return nil
}

// span adds the n bytes at off in the file to the comment packet.
func (h *oggHead) span(off, n int64) {
	if k := len(h.cspans) - 1; k >= 0 && h.cspans[k].off+h.cspans[k].n == off {
		h.cspans[k].n += n
	} else if n > 0 {
		h.cspans = append(h.cspans, oggSpan{off, n})
		h.cstarts = append(h.cstarts, h.clen)
	}
	h.clen += n
}

// complete is whether h holds all its stream's header packets.
func (h *oggHead) complete() bool {
	n := len(h.packets)
	if n == 0 {
		return false
	}
	b := h.packets[0]
	switch h.kind {
	case "vorbis":
		return n >= 3
	case "opus":
		return n >= 2
	case "speex": // its header, the comments, then the extra headers it counts
		extra := 0
		if len(b) >= 80 {
			extra = int(min(binary.LittleEndian.Uint32(b[76:]), 8))
		}
		return n >= 2+extra
	case "flac": // the metadata blocks after the first packet: as many as it says, else to the last
		if len(b) >= 9 && binary.BigEndian.Uint16(b[7:]) > 0 {
			return n >= 1+int(binary.BigEndian.Uint16(b[7:]))
		}
		last := h.packets[n-1]
		return n >= 2 && len(last) > 0 && last[0]&0x80 != 0
	}
	return true
}

// readOggHead reads the header packets of the file's first stream of a codec it knows (else of its
// first stream). It reads at most oggMaxPages pages and holds at most oggMaxPackets packets of
// oggMaxHeaders bytes, so a file of nothing but pages or empty packets can't take the memory.
func readOggHead(r io.ReaderAt, size int64) (*oggHead, error) {
	h := &oggHead{tidy: true}
	for h.start < size {
		n := id3Skip(r, h.start)
		if n == 0 {
			break
		}
		h.start += n
	}
	buf := make([]byte, 27+255+255*255) // a page
	read := 0
	// Every stream's first page comes before any other page.
	var first *oggPage
	for off := h.start; off < size; {
		if read++; read > oggMaxPages {
			return nil, ErrCorrupt
		}
		p, err := readOggPage(r, off, size, buf)
		if err != nil && first == nil {
			return nil, err
		}
		if err != nil || p.flags&oggBOS == 0 {
			break
		}
		kind := ""
		if len(p.lacing) > 0 {
			b, err := readAt(r, off+p.hlen(), int64(min(p.body, 16)))
			if err != nil {
				return nil, err
			}
			kind = oggKind(b)
		}
		if first != nil {
			h.muxed = true
		}
		if first == nil || h.kind == "" && kind != "" {
			first, h.kind, h.serial = p, kind, p.serial
		}
		off += p.size()
	}
	if first == nil {
		return nil, ErrCorrupt
	}
	var cur []byte             // the packet being put together
	mid, spill := false, false // a packet is under way (its last segment was 255); it's the comment packet, left in the file
	for off := first.off; !h.complete(); {
		if read++; read > oggMaxPages {
			return nil, ErrCorrupt
		}
		p, err := readOggPage(r, off, size, buf)
		if err != nil {
			return nil, err
		}
		off += p.size()
		if p.serial != h.serial {
			if !h.muxed {
				return nil, ErrCorrupt
			}
			continue
		}
		if len(h.pages) > 0 && p.flags&oggBOS != 0 ||
			len(p.lacing) > 0 && (p.flags&oggContinued != 0) != mid {
			return nil, ErrCorrupt
		}
		page := buf[:p.size()]
		if err := readFull(r, page, p.off); err != nil {
			return nil, err
		}
		hl := int(p.hlen())
		p.ok = pageCRC(page[:hl], page[hl:]) == p.crc
		h.damaged = h.damaged || !p.ok
		body := page[hl:]
		h.pages = append(h.pages, p)
		h.end = off
		at := 0
		for i, l := range p.lacing {
			if !mid && len(h.packets) == 1 && h.spills() {
				spill = true
			}
			if spill {
				h.span(p.off+int64(hl+at), int64(l))
			} else {
				if err := h.hold(int64(l)); err != nil {
					return nil, err
				}
				cur = append(cur, body[at:at+int(l)]...)
			}
			at += int(l)
			if mid = l == 255; mid {
				continue
			}
			if !spill && cur == nil {
				cur = []byte{}
			}
			h.packets = append(h.packets, cur)
			cur, spill = nil, false
			if len(h.packets) > oggMaxPackets {
				return nil, ErrCorrupt
			}
			last := i == len(p.lacing)-1
			if len(h.packets) == 1 && (len(h.pages) > 1 || !last) {
				h.tidy = false // the first packet must be alone on the first page
			}
			if h.complete() {
				if !last {
					h.tidy = false // so must the headers' last page end with them
				}
				break
			}
		}
	}
	return h, nil
}

// writable is whether h's tags can be written: Vorbis or Opus alone, laid out as the spec says.
func (h *oggHead) writable() bool {
	if h.muxed || !h.tidy || len(h.pages) < 2 {
		return false
	}
	switch h.kind {
	case "vorbis":
		return bytes.HasPrefix(h.packets[2], []byte("\x05vorbis"))
	case "opus":
		return true
	}
	return false
}

// spanReader reads n bytes of the comment packet, skip bytes into its spans.
type spanReader struct {
	r     io.ReaderAt
	spans []oggSpan
	skip  int64
	n     int64
}

func (s *spanReader) Read(b []byte) (int, error) {
	if s.n <= 0 {
		return 0, io.EOF
	}
	for len(s.spans) > 0 && s.skip >= s.spans[0].n {
		s.skip -= s.spans[0].n
		s.spans = s.spans[1:]
	}
	if len(s.spans) == 0 {
		return 0, ErrCorrupt
	}
	k := min(int64(len(b)), s.spans[0].n-s.skip, s.n)
	m, err := s.r.ReadAt(b[:k], s.spans[0].off+s.skip)
	s.skip += int64(m)
	s.n -= int64(m)
	if int64(m) == k {
		return m, nil
	}
	if err == nil || err == io.EOF {
		err = ErrCorrupt
	}
	return m, err
}

// commentReader reads n bytes of h's comment packet from off in it, starting at the span that holds
// off: a reader made for each entry doesn't go through every page before it.
func (h *oggHead) commentReader(r io.ReaderAt, off, n int64) io.Reader {
	k := sort.Search(len(h.cstarts), func(i int) bool { return h.cstarts[i] > off }) - 1
	if k < 0 {
		return &spanReader{r: r, spans: h.cspans, skip: off, n: n}
	}
	return &spanReader{r: r, spans: h.cspans[k:], skip: off - h.cstarts[k], n: n}
}

// oggCursor reads h's comment packet from its start, a piece at a time.
type oggCursor struct {
	h   *oggHead
	r   io.ReaderAt
	br  *bufio.Reader
	pos int64
	b   [4]byte
}

func (c *oggCursor) left() int64 { return c.h.clen - c.pos }

// corrupt is err, but ErrCorrupt for a packet that ends too soon.
func corrupt(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return ErrCorrupt
	}
	return err
}

func (c *oggCursor) read(b []byte) error {
	if int64(len(b)) > c.left() {
		return ErrCorrupt
	}
	if _, err := io.ReadFull(c.br, b); err != nil {
		return corrupt(err)
	}
	c.pos += int64(len(b))
	return nil
}

func (c *oggCursor) u32() (int64, error) {
	if err := c.read(c.b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(c.b[:])), nil
}

func (c *oggCursor) str(n int64) (string, error) {
	if n > c.left() {
		return "", ErrCorrupt
	}
	var s strings.Builder
	s.Grow(int(n))
	if _, err := io.CopyN(&s, c.br, n); err != nil {
		return "", corrupt(err)
	}
	c.pos += n
	return s.String(), nil
}

// peek is the next n bytes, fewer at the packet's end, without reading past them.
func (c *oggCursor) peek(n int) []byte {
	b, _ := c.br.Peek(int(min(int64(n), c.left())))
	return b
}

// skip passes n bytes, reading them only when they are buffered already.
func (c *oggCursor) skip(n int64) {
	c.pos += n
	if n <= int64(c.br.Buffered()) {
		c.br.Discard(int(n))
		return
	}
	c.br.Reset(c.h.commentReader(c.r, c.pos, c.left()))
}

// oggTags is a comment packet as read: its comments, each entry too long to hold that isn't a field's
// (a cover) standing in cm.list as a placeholder, refs saying where its bytes are in the packet.
type oggTags struct {
	kind  string
	magic string
	cm    *comments
	refs  map[string]oggSpan
	rest  oggSpan // the bytes after the comments: Vorbis's framing bit and padding, Opus's data or padding
	first byte    // the first of them
}

// placeholder is the placeholder of the j-th entry left in the file, whose key is key: the key, "=",
// a NUL and j, with another NUL before j for as long as that is an entry in taken. j, after the last
// NUL, tells placeholders apart; a NUL tells them from any entry written, which has none.
func placeholder(key string, j int, taken map[string]bool) string {
	var b []byte
	if key != "" {
		b = append([]byte(key), '=')
	}
	b = append(b, 0)
	k := len(b)
	b = strconv.AppendInt(b, int64(j), 10)
	for taken[string(b)] {
		b = strconv.AppendInt(append(b[:k], 0), int64(j), 10)
		k++
	}
	return string(b)
}

// comment is the comments of h's comment packet (Vorbis, Opus, Speex), their entries read one by one
// from r; nil without one. An entry a field is read from is always held. Every other entry too long
// to hold stands in the list as its placeholder, a few bytes long, counted as held.
func (h *oggHead) comment(r io.ReaderAt) (*oggTags, error) {
	if len(h.packets) < 2 || !h.spills() {
		return nil, nil
	}
	t := &oggTags{kind: h.kind, magic: map[string]string{"vorbis": "\x03vorbis", "opus": "OpusTags"}[h.kind],
		refs: map[string]oggSpan{}}
	c := &oggCursor{h: h, r: r, br: bufio.NewReaderSize(h.commentReader(r, 0, h.clen), 16<<10)}
	magic := make([]byte, len(t.magic))
	if err := c.read(magic); err != nil || string(magic) != t.magic {
		return nil, ErrCorrupt
	}
	n, err := c.u32()
	if err != nil {
		return nil, err
	}
	if n > c.left() {
		return nil, ErrCorrupt
	}
	if err := h.hold(n); err != nil {
		return nil, err
	}
	vendor, err := c.str(n)
	if err != nil {
		return nil, err
	}
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	if count > c.left()/4 {
		return nil, ErrCorrupt
	}
	if count > oggMaxEntries {
		return nil, ErrTooBig
	}
	t.cm = &comments{vendor: vendor, list: make([]string, 0, count)}
	var (
		spans []oggSpan
		keys  []string
		at    []int // where in the list each stands
	)
	for i := int64(0); i < count; i++ {
		n, err := c.u32()
		if err != nil {
			return nil, err
		}
		if n > c.left() {
			return nil, ErrCorrupt
		}
		if n > oggHoldEntry {
			key := ""
			if b := c.peek(256); bytes.IndexByte(b, '=') > 0 {
				key = string(b[:bytes.IndexByte(b, '=')])
			}
			if !readKeys[strings.ToUpper(key)] {
				if err := h.hold(64 + int64(len(key))); err != nil {
					return nil, err
				}
				spans, keys, at = append(spans, oggSpan{c.pos, n}), append(keys, key), append(at, len(t.cm.list))
				t.cm.list = append(t.cm.list, "")
				c.skip(n)
				continue
			}
		}
		if err := h.hold(32 + n); err != nil {
			return nil, err
		}
		s, err := c.str(n)
		if err != nil {
			return nil, err
		}
		t.cm.list = append(t.cm.list, s)
	}
	if len(spans) > 0 {
		// Only an entry read with a NUL in it can be a placeholder's like.
		taken := map[string]bool{}
		for _, s := range t.cm.list {
			if strings.IndexByte(s, 0) >= 0 {
				taken[s] = true
			}
		}
		for j, i := range at {
			p := placeholder(keys[j], j, taken)
			if err := h.hold(int64(len(p))); err != nil {
				return nil, err
			}
			t.cm.list[i] = p
			t.refs[p] = spans[j]
		}
	}
	t.rest = oggSpan{c.pos, c.left()}
	if t.rest.n > 0 {
		if err := c.read(c.b[:1]); err != nil {
			return nil, err
		}
		t.first = c.b[0]
	}
	return t, nil
}

// data is whether what follows the comments is data to keep: Opus's, its first byte odd.
func (t *oggTags) data() bool { return t.kind == "opus" && t.rest.n > 0 && t.first&1 != 0 }

// size is how long t's packet is with list as its comments, before any zeros after them, and how much
// of that its pictures take.
func (t *oggTags) size(list []string) (n, pics int64) {
	n = int64(len(t.magic)) + 4 + int64(len(t.cm.vendor)) + 4
	for _, e := range list {
		m := int64(len(e))
		if s, ok := t.refs[e]; ok {
			m = s.n
		}
		n += 4 + m
		if k, _, ok := entryKey(e); ok && pictureKey(k) {
			pics += 4 + m
		}
	}
	switch {
	case t.kind == "vorbis":
		n++ // the framing bit
	case t.data():
		n += t.rest.n
	}
	return n, pics
}

// spaceless drops the spaces and tabs a tagger may leave around base64.
type spaceless struct{ r io.Reader }

func (s spaceless) Read(b []byte) (int, error) {
	for {
		n, err := s.r.Read(b)
		k := 0
		for _, c := range b[:n] {
			if c != ' ' && c != '\t' {
				b[k] = c
				k++
			}
		}
		if k > 0 || err != nil || n == 0 {
			return k, err
		}
	}
}

// decoder decodes entry e's value, base64, read from where it is; max is the most bytes it can give.
func (t *oggTags) decoder(h *oggHead, r io.ReaderAt, e string) (io.Reader, int64) {
	i := strings.IndexByte(e, '=') + 1
	var v io.Reader
	n := int64(len(e) - i)
	if s, ok := t.refs[e]; ok {
		n = s.n - int64(i)
		v = h.commentReader(r, s.off+int64(i), n)
	} else {
		v = strings.NewReader(e[i:])
	}
	return base64.NewDecoder(base64.StdEncoding, spaceless{v}), int64(base64.StdEncoding.DecodedLen(int(n)))
}

// picHead reads a FLAC picture block from d, of at most max bytes, up to its image, as pictureHead64
// does: the picture but for its bytes, which are p.Len, and those of them read already. Its first
// read is short, enough for a cover's MIME type and description, as every picture's head is read.
func picHead(d io.Reader, max int64) (p *picRef, got []byte, err error) {
	b := make([]byte, min(max, 256))
	k, _ := io.ReadFull(d, b)
	b = b[:k]
	for {
		p, need, err := pictureFields(b, max)
		if err != nil {
			return nil, nil, err
		}
		if need == 0 {
			return p, b[p.Off:min(int64(len(b)), p.Off+p.Len)], nil
		}
		if need > pictureMaxHead {
			return nil, nil, ErrCorrupt
		}
		more := make([]byte, need-int64(len(b)))
		if k, _ := io.ReadFull(d, more); k < len(more) {
			return nil, nil, ErrCorrupt
		}
		b = append(b, more...)
	}
}

// picture is t's front cover, else its first picture, else an old COVERART, its bytes read from r;
// nil without one, and then big when it is over oggMaxCover. The pictures' heads are read once, in
// one pass, to choose; then those chosen are read whole in turn, front covers first, until one reads,
// at most oggMaxTries of them in all: a file of many pictures whose bytes don't read costs a pass.
func (t *oggTags) picture(h *oggHead, r io.ReaderAt) (pic *picRef, big bool) {
	var front, other []int // the pictures whose heads read
	for i, e := range t.cm.list {
		if k, _, ok := entryKey(e); !ok || k != blockPicture {
			continue
		}
		p, _, err := picHead(t.decoder(h, r, e))
		switch {
		case err != nil:
		case p.Type == 3:
			front = append(front, i)
		case len(other) < oggMaxTries:
			other = append(other, i)
		}
		if len(front) == oggMaxTries {
			break
		}
	}
	tries := oggMaxTries
	var buf []byte // the bytes of the last that didn't read, used again
	for _, i := range append(front, other...) {
		if tries == 0 {
			return nil, false
		}
		tries--
		d, max := t.decoder(h, r, t.cm.list[i])
		p, got, err := picHead(d, max)
		if err != nil {
			continue
		}
		if p.Len > oggMaxCover {
			return nil, true
		}
		if int64(cap(buf)) < p.Len {
			buf = make([]byte, p.Len)
		}
		p.Data = buf[:p.Len]
		n := copy(p.Data, got)
		if _, err := io.ReadFull(d, p.Data[n:]); err == nil {
			p.Off, p.Len, p.hasOffset = 0, 0, false
			return p, false
		}
	}
	for _, e := range t.cm.list {
		if k, _, ok := entryKey(e); !ok || k != "COVERART" {
			continue
		}
		if tries == 0 {
			return nil, false
		}
		tries--
		d, max := t.decoder(h, r, e)
		if max > oggMaxCover {
			return nil, true
		}
		if int64(cap(buf)) < max {
			buf = make([]byte, max)
		}
		b := buf[:max]
		n, err := io.ReadFull(d, b)
		if err != nil && err != io.ErrUnexpectedEOF || n == 0 {
			continue
		}
		b = b[:n]
		mime := pictureInfo(b).MIME
		if m := t.cm.values("COVERARTMIME"); mime == "" && len(m) > 0 {
			mime = m[0]
		}
		return &picRef{MIME: mime, Type: 3, Data: b}, false
	}
	return nil, false
}

// flacTags is an Ogg FLAC stream's comments, its VORBIS_COMMENT metadata block's; nil without one.
func (h *oggHead) flacTags() (*comments, error) {
	for _, p := range h.packets[1:] {
		if len(p) >= 4 && p[0]&0x7f == flacComment {
			cm, _, err := parseComments(p[4:])
			return cm, err
		}
	}
	return nil, nil
}

// flacPicture is an Ogg FLAC stream's front cover, else its first picture: PICTURE metadata blocks.
func (h *oggHead) flacPicture() *picRef {
	var first *picRef
	for _, b := range h.packets[1:] {
		if len(b) < 4 || b[0]&0x7f != flacPicture {
			continue
		}
		p, off, n, err := parsePicture(b[4:])
		if err != nil {
			continue
		}
		p.Data, p.Off, p.Len, p.hasOffset = b[4+off:4+off+n], 0, 0, false
		if p.Type == 3 {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// chained is whether the file's last page belongs to another stream: streams chained one after another.
func (h *oggHead) chained(r io.ReaderAt, size int64) bool {
	for _, n := range []int64{8 << 10, 66 << 10} {
		n = min(n, size-h.end)
		if n < 27 {
			return false
		}
		b, err := readAt(r, size-n, n)
		if err != nil {
			return false
		}
		for i := len(b) - 27; i >= 0; i-- {
			p, ok := parseOggHeader(b[i:])
			if !ok || int64(i)+p.size() > n {
				continue
			}
			hl := i + int(p.hlen())
			if pageCRC(b[i:hl], b[hl:int64(i)+p.size()]) == p.crc {
				return p.serial != h.serial
			}
		}
		if n == size-h.end {
			break
		}
	}
	return false
}

func (oggCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	h, err := readOggHead(r, size)
	if err != nil {
		return nil, nil, err
	}
	info := &Info{Fields: map[Field]string{}, Writable: h.writable() && !h.damaged}
	info.Format = map[string]string{"vorbis": "Ogg Vorbis", "opus": "Opus", "flac": "Ogg FLAC", "speex": "Speex",
		"": "Ogg"}[h.kind]
	var (
		pic *picRef
		t   *oggTags
		big bool
	)
	if h.kind == "flac" {
		cm, err := h.flacTags()
		if err != nil {
			return nil, nil, err
		}
		if cm != nil {
			info.Fields = cm.fields()
		}
		pic = h.flacPicture()
	} else {
		if t, err = h.comment(r); err != nil {
			return nil, nil, err
		}
		if t != nil {
			info.Fields = t.cm.fields()
			pic, big = t.picture(h, r)
		}
	}
	if h.start > 0 {
		info.Notes = append(info.Notes, "It has an ID3 tag before the Ogg data too, which is left as it is")
	}
	switch {
	case h.muxed:
		info.Notes = append(info.Notes, "It holds more than one stream, so its tags can be read here but not changed")
	case h.kind == "flac":
		info.Notes = append(info.Notes, "Tags of FLAC in Ogg can be read here but not changed")
	case h.kind == "speex":
		info.Notes = append(info.Notes, "Speex tags can be read here but not changed")
	case h.kind == "":
		info.Notes = append(info.Notes, "It isn't Vorbis, Opus or FLAC, so it has no tags to show")
	case !h.writable():
		info.Notes = append(info.Notes, "Its header pages aren't laid out as Ogg says, so its tags can be read here but not changed")
	case h.damaged:
		info.Notes = append(info.Notes, "A header page is damaged, so its tags can be read here but not changed")
	case h.chained(r, size):
		info.Notes = append(info.Notes, "It's a chain of streams: only the first one's tags are shown and changed")
	}
	if big {
		info.Notes = append(info.Notes, "Its cover is over 8 MB, too big to show here")
	}
	// A save keeps what it doesn't change, so one that keeps all this can't be.
	if info.Writable && t != nil {
		if n, pics := t.size(t.cm.list); n > oggMaxComment && n-pics <= oggMaxComment {
			info.Notes = append(info.Notes, "Its cover is too big to keep in an Ogg song: remove it or choose a smaller one to change its tags")
		} else if n > oggMaxComment {
			info.Notes = append(info.Notes, "Its tags are too big to keep in an Ogg song: clear the longest to change them")
		}
	}
	return info, pic, nil
}

// oggPart is bytes of the header packets as written: b, else n bytes of the old comment packet from off.
type oggPart struct {
	b      []byte
	off, n int64
}

func (p oggPart) size() int64 {
	if p.b != nil {
		return int64(len(p.b))
	}
	return p.n
}

// parts is t's packet with list as its comments and pad zeros after, as written: Vorbis's magic, the
// comments and the framing bit; Opus's magic, the comments and what came after them when that is
// data. An entry left in the file is a part of its own, read from there; late is whether one lands
// further on in the packet than it was.
func (t *oggTags) parts(list []string, pad int64) (parts []oggPart, late bool) {
	b := []byte(t.magic)
	at := int64(0)
	put := func(s string) {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	ref := func(s oggSpan) {
		if len(b) > 0 {
			parts, at, b = append(parts, oggPart{b: b}), at+int64(len(b)), nil
		}
		late = late || at > s.off
		parts, at = append(parts, oggPart{off: s.off, n: s.n}), at+s.n
	}
	put(t.cm.vendor)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(list)))
	for _, e := range list {
		if s, ok := t.refs[e]; ok {
			b = binary.LittleEndian.AppendUint32(b, uint32(s.n))
			ref(s)
		} else {
			put(e)
		}
	}
	if t.data() {
		ref(t.rest)
	} else if t.kind == "vorbis" {
		b = append(b, 1)
	}
	b = append(b, make([]byte, pad)...)
	if len(b) > 0 {
		parts = append(parts, oggPart{b: b})
	}
	return parts, late
}

// refsFirst is list with the entries left in the file first, so none of them moves towards the end.
func refsFirst(list []string, refs map[string]oggSpan) []string {
	out := make([]string, 0, len(list))
	for _, first := range []bool{true, false} {
		for _, e := range list {
			if _, ok := refs[e]; ok == first {
				out = append(out, e)
			}
		}
	}
	return out
}

// oggLayout lays packets of these lengths out on pages as libogg does when it flushes them: 255
// segments a page, the last page ending with the last packet. A page on which a packet ends has
// granule position 0, one that a packet only passes through -1, Ogg's "no packet ends here".
func oggLayout(lens []int64) []*oggPage {
	var lacing []byte
	for _, n := range lens {
		for ; n >= 255; n -= 255 {
			lacing = append(lacing, 255)
		}
		lacing = append(lacing, byte(n))
	}
	var pages []*oggPage
	for cont := false; len(lacing) > 0; {
		p := &oggPage{lacing: lacing[:min(255, len(lacing))], granule: ^uint64(0)}
		lacing = lacing[len(p.lacing):]
		if cont {
			p.flags = oggContinued
		}
		for _, l := range p.lacing {
			p.body += int(l)
			if l < 255 {
				p.granule = 0
			}
		}
		cont = p.lacing[len(p.lacing)-1] == 255
		pages = append(pages, p)
	}
	return pages
}

// writePages writes pages, laid out as they say, their data taken in turn from parts, each with its
// CRC; a part left in the file is read from src, the file as it was. A page at a time is held.
func (h *oggHead) writePages(w io.Writer, src io.ReaderAt, pages []*oggPage, parts []oggPart) error {
	buf := make([]byte, 255*255)
	var rd io.Reader // the part being read from src
	pi, at := 0, int64(0)
	for _, p := range pages {
		b := buf[:p.body]
		for k := 0; k < len(b); {
			for pi < len(parts) && at == parts[pi].size() {
				pi, at, rd = pi+1, 0, nil
			}
			if pi == len(parts) {
				return ErrCorrupt
			}
			pt := parts[pi]
			m := int(min(int64(len(b)-k), pt.size()-at))
			if pt.b != nil {
				copy(b[k:k+m], pt.b[at:])
			} else {
				if rd == nil {
					rd = h.commentReader(src, pt.off, pt.n)
				}
				if _, err := io.ReadFull(rd, b[k:k+m]); err != nil {
					return corrupt(err)
				}
			}
			k += m
			at += int64(m)
		}
		hd := p.header()
		p.crc = pageCRC(hd, b)
		binary.LittleEndian.PutUint32(hd[22:], p.crc)
		if _, err := w.Write(hd); err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	for pi < len(parts) && at == parts[pi].size() {
		pi, at = pi+1, 0
	}
	if pi != len(parts) {
		return ErrCorrupt
	}
	return nil
}

// copyOggPages copies src's pages from off on to w, each as it was but for its sequence number, moved
// by shift, and so its CRC. Bytes that aren't a page (damage) are copied as they are, the stream's
// pages after them numbered on; from the stream's last page on (a chained stream's next, or bytes no
// page of it follows) the rest is copied as it is.
func copyOggPages(w io.Writer, src io.ReaderAt, off, size int64, serial, shift uint32) error {
	if shift == 0 {
		return copyRange(w, src, off, size-off)
	}
	br := bufio.NewReaderSize(io.NewSectionReader(src, off, size-off), 64<<10)
	var hb [27 + 255]byte
	synced := true // just after a page: the next is taken as it is; after junk, only one whose CRC is right
	for off < size {
		p := peekOggPage(br, size-off, !synced)
		if p != nil && p.serial == serial && p.flags&oggBOS == 0 {
			b, _ := br.Peek(int(p.hlen()))
			h := hb[:len(b)]
			copy(h, b)
			seq := p.seq + shift
			binary.LittleEndian.PutUint32(h[18:], seq)
			binary.LittleEndian.PutUint32(h[22:], renumbered(p.crc, p.seq, seq, p.size()))
			if _, err := w.Write(h); err != nil {
				return err
			}
			if _, err := br.Discard(len(h)); err != nil {
				return err
			}
			if _, err := io.CopyN(w, br, int64(p.body)); err != nil {
				return err
			}
			off += p.size()
			synced = true
			if p.flags&oggEOS != 0 {
				break
			}
			continue
		}
		if p != nil {
			break // another stream's page, or the stream starting again: the next in a chain
		}
		n := oggJunk(br)
		if n == 0 {
			break
		}
		if _, err := io.CopyN(w, br, int64(n)); err != nil {
			return err
		}
		off += int64(n)
		synced = false
	}
	return copyRange(w, src, off, size-off)
}

// peekOggPage is the page at the start of br, of at most left bytes, its CRC checked when check is
// set; nil when there isn't one.
func peekOggPage(br *bufio.Reader, left int64, check bool) *oggPage {
	b, err := br.Peek(27)
	if err != nil {
		return nil
	}
	if b, err = br.Peek(27 + int(b[26])); err != nil {
		return nil
	}
	p, ok := parseOggHeader(b)
	if !ok || p.size() > left {
		return nil
	}
	if check {
		b, err := br.Peek(int(p.size()))
		if err != nil || pageCRC(b[:p.hlen()], b[p.hlen():]) != p.crc {
			return nil
		}
	}
	return p
}

// oggJunk is how many bytes at the start of br come before the next "OggS" after its first byte: all
// it holds when there is none, but for the last 3, which may start one, until the file ends.
func oggJunk(br *bufio.Reader) int {
	b, err := br.Peek(br.Size())
	if len(b) == 0 {
		return 0
	}
	if i := bytes.Index(b[1:], []byte("OggS")); i >= 0 {
		return i + 1
	}
	if err != nil {
		return len(b)
	}
	return len(b) - 3
}

func (oggCodec) write(path string, c Changes) error {
	if c.Picture != nil && len(c.Picture.Data) > oggMaxPicture {
		return errOggPicture
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	size := st.Size()
	h, err := readOggHead(f, size)
	var t *oggTags
	switch {
	case err != nil:
	case !h.writable():
		err = ErrReadOnly
	case h.damaged:
		err = ErrCorrupt
	default:
		t, err = h.comment(f)
	}
	f.Close()
	if err != nil {
		return err
	}
	t.cm.apply(c.Set)
	if c.Picture != nil {
		t.cm.setPicture(c.Picture)
	} else if c.RemovePicture {
		t.cm.setPicture(nil)
	}
	n, pics := t.size(t.cm.list)
	if n > oggMaxComment {
		if c.Picture == nil && !c.RemovePicture && n-pics <= oggMaxComment {
			return errOggCover
		}
		return ErrTooBig
	}
	// Zeros after the comments: as many as keep the packet as long as it was when that is near enough,
	// else room for next time; none after Opus's data.
	pad := int64(0)
	if !t.data() {
		pad = oggPadding
		if old := h.clen; old >= n && old-n <= oggMaxPadding {
			pad = old - n
		}
		pad = min(pad, oggMaxComment-n)
	}
	parts, late := t.parts(t.cm.list, pad)
	inPlace := n+pad == h.clen
	if inPlace && late {
		parts, late = t.parts(refsFirst(t.cm.list, t.refs), pad)
		inPlace = !late
	}
	for _, p := range h.packets[2:] {
		parts = append(parts, oggPart{b: p})
	}
	old := h.pages[1:]
	var oldLen int64
	for _, p := range old {
		oldLen += p.size()
	}
	if inPlace {
		// As long as it was: the old pages' layout holds it, and the file stays as it is around them.
		pages := make([]*oggPage, len(old))
		for i, p := range old {
			q := *p
			pages[i] = &q
		}
		return patchWith(path, old[0].off, oldLen, func(src *os.File, w io.Writer) error {
			return h.writePages(w, src, pages, parts)
		})
	}
	lens := []int64{n + pad}
	for _, p := range h.packets[2:] {
		lens = append(lens, int64(len(p)))
	}
	pages := oggLayout(lens)
	var newLen int64
	for i, p := range pages {
		p.serial, p.seq = h.serial, old[0].seq+uint32(i)
		newLen += p.size()
	}
	pages[len(pages)-1].flags |= old[len(old)-1].flags & oggEOS
	start, end, serial := old[0].off, h.end, h.serial
	shift := uint32(len(pages) - len(old))
	return rewrite(path, size-oldLen+newLen, func(src *os.File, w io.Writer) error {
		if err := copyRange(w, src, 0, start); err != nil {
			return err
		}
		if err := h.writePages(w, src, pages, parts); err != nil {
			return err
		}
		return copyOggPages(w, src, end, size, serial, shift)
	})
}
