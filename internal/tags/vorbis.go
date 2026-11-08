package tags

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"strings"
)

// Vorbis comments: FLAC's VORBIS_COMMENT block and Ogg Vorbis's and Opus's comment packet, "KEY=value"
// entries after a vendor string. Also FLAC's picture block, which Ogg carries base64'd as
// METADATA_BLOCK_PICTURE.

type comments struct {
	vendor string
	list   []string // "KEY=value", in the file's order
}

// maxComments is the most entries a comment structure may have: far more than any real tag holds, few
// enough that a crafted one can't fill the Q2's memory.
const maxComments = 1 << 16

// parseComments reads the comment structure at the start of b (no packet magic, no framing bit) and
// how many bytes it took.
func parseComments(b []byte) (*comments, int, error) {
	c, n, err := readComments(bytes.NewReader(b), int64(len(b)))
	return c, int(n), err
}

// readComments reads a comment structure of at most size bytes from r, an entry at a time, so only the
// entries are held, and how many bytes it took.
func readComments(r io.Reader, size int64) (*comments, int64, error) {
	var used int64
	var n4 [4]byte
	var scratch []byte
	short := func(err error) error {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ErrCorrupt
		}
		return err
	}
	u32 := func() (int64, error) {
		if size-used < 4 {
			return 0, ErrCorrupt
		}
		if _, err := io.ReadFull(r, n4[:]); err != nil {
			return 0, short(err)
		}
		used += 4
		return int64(binary.LittleEndian.Uint32(n4[:])), nil
	}
	str := func() (string, error) {
		n, err := u32()
		if err != nil {
			return "", err
		}
		if n > size-used {
			return "", ErrCorrupt
		}
		used += n
		if scratch == nil {
			scratch = make([]byte, 8<<10)
		}
		if n <= int64(len(scratch)) {
			if _, err := io.ReadFull(r, scratch[:n]); err != nil {
				return "", short(err)
			}
			return string(scratch[:n]), nil
		}
		var s strings.Builder
		s.Grow(int(n))
		for left := n; left > 0; {
			k := min(left, int64(len(scratch)))
			if _, err := io.ReadFull(r, scratch[:k]); err != nil {
				return "", short(err)
			}
			s.Write(scratch[:k])
			left -= k
		}
		return s.String(), nil
	}
	vendor, err := str()
	if err != nil {
		return nil, 0, err
	}
	count, err := u32()
	if err != nil {
		return nil, 0, err
	}
	if count > (size-used)/4 || count > maxComments {
		return nil, 0, ErrCorrupt
	}
	c := &comments{vendor: vendor, list: make([]string, 0, min(count, 64))}
	for i := int64(0); i < count; i++ {
		e, err := str()
		if err != nil {
			return nil, 0, err
		}
		c.list = append(c.list, e)
	}
	return c, used, nil
}

// size is how many bytes the structure takes.
func (c *comments) size() int64 {
	n := int64(8 + len(c.vendor))
	for _, e := range c.list {
		n += 4 + int64(len(e))
	}
	return n
}

// writeTo writes the structure as parseComments reads it, a piece at a time.
func (c *comments) writeTo(w io.Writer) error {
	buf := make([]byte, 4<<10)
	u32 := func(v int) error {
		binary.LittleEndian.PutUint32(buf, uint32(v))
		_, err := w.Write(buf[:4])
		return err
	}
	put := func(s string) error {
		if err := u32(len(s)); err != nil {
			return err
		}
		for len(s) > 0 {
			k := copy(buf, s)
			if _, err := w.Write(buf[:k]); err != nil {
				return err
			}
			s = s[k:]
		}
		return nil
	}
	if err := put(c.vendor); err != nil {
		return err
	}
	if err := u32(len(c.list)); err != nil {
		return err
	}
	for _, e := range c.list {
		if err := put(e); err != nil {
			return err
		}
	}
	return nil
}

// bytes is the structure as parseComments reads it.
func (c *comments) bytes() []byte {
	var b bytes.Buffer
	b.Grow(int(c.size()))
	c.writeTo(&b)
	return b.Bytes()
}

// entryKey is an entry's key in upper case and its value; ok is false for one without "=".
func entryKey(e string) (key, val string, ok bool) {
	i := strings.IndexByte(e, '=')
	if i <= 0 {
		return "", "", false
	}
	return strings.ToUpper(e[:i]), e[i+1:], true
}

// values are key's values, in order.
func (c *comments) values(key string) []string {
	var out []string
	for _, e := range c.list {
		if k, v, ok := entryKey(e); ok && k == key {
			out = append(out, v)
		}
	}
	return out
}

// vkey is a field's key, and the keys the Q2 reads it from, in the order it prefers them (Q2 Pod's
// patch/xiph.h: TRACKNUM before TRACKNUMBER, DISC before DISCNUMBER, DATE before YEAR; for FLAC,
// "ALBUM ARTIST" when ALBUMARTIST is empty). Setting the field removes all of them, so a stale one
// can't win over the new value.
type vkey struct {
	key  string
	read []string
}

var vorbisKeys = map[Field]vkey{
	Title:       {"TITLE", []string{"TITLE"}},
	Artist:      {"ARTIST", []string{"ARTIST"}},
	Album:       {"ALBUM", []string{"ALBUM"}},
	AlbumArtist: {"ALBUMARTIST", []string{"ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST"}},
	Track:       {"TRACKNUMBER", []string{"TRACKNUM", "TRACKNUMBER"}},
	TrackTotal:  {"TRACKTOTAL", []string{"TRACKTOTAL", "TOTALTRACKS"}},
	Disc:        {"DISCNUMBER", []string{"DISC", "DISCNUMBER"}},
	DiscTotal:   {"DISCTOTAL", []string{"DISCTOTAL", "TOTALDISCS"}},
	Date:        {"DATE", []string{"DATE", "YEAR"}},
	Genre:       {"GENRE", []string{"GENRE"}},
	Composer:    {"COMPOSER", []string{"COMPOSER"}},
	Comment:     {"COMMENT", []string{"COMMENT", "DESCRIPTION"}},
	Lyrics:      {"LYRICS", []string{"LYRICS", "UNSYNCEDLYRICS"}},
}

// readKeys is every key a field is read from.
var readKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range vorbisKeys {
		for _, r := range k.read {
			m[r] = true
		}
	}
	return m
}()

// fields are the editor's fields; "3/12" in a track or disc key gives its total too, unless a total
// key says otherwise. The entries are gone through once, however many there are.
func (c *comments) fields() map[Field]string {
	vals := map[string][]string{}
	for _, e := range c.list {
		if k, v, ok := entryKey(e); ok && readKeys[k] {
			vals[k] = append(vals[k], v)
		}
	}
	// first is the values of the first of keys that has one, joined as the Q2 joins them.
	first := func(keys []string) string {
		for _, k := range keys {
			var vs []string
			for _, v := range vals[k] {
				if v = strings.TrimSpace(v); v != "" {
					vs = append(vs, v)
				}
			}
			if len(vs) > 0 {
				return strings.Join(vs, " / ")
			}
		}
		return ""
	}
	out := map[Field]string{}
	for f, k := range vorbisKeys {
		if v := first(k.read); v != "" {
			out[f] = v
		}
	}
	for _, p := range [][2]Field{{Track, TrackTotal}, {Disc, DiscTotal}} {
		if v, ok := out[p[0]]; ok {
			if n, _ := splitNum(strings.SplitN(v, " / ", 2)[0]); n != "" {
				out[p[0]] = n
			} else {
				delete(out, p[0])
			}
		}
		for _, k := range vorbisKeys[p[0]].read {
			for _, v := range vals[k] {
				if _, of := splitNum(v); out[p[1]] == "" && of != "" {
					out[p[1]] = of
				}
			}
		}
	}
	return out
}

// apply sets each field in set ("" removes it), in the place its first key had, else at the end. A
// number and its total are written together, so a "3/12" doesn't lose its other half.
func (c *comments) apply(set map[Field]string) {
	if len(set) == 0 {
		return
	}
	cur := c.fields()
	s := make(map[Field]string, len(set)+1)
	for f, v := range set {
		s[f] = v
	}
	for _, p := range [][2]Field{{Track, TrackTotal}, {Disc, DiscTotal}} {
		_, a := s[p[0]]
		_, b := s[p[1]]
		if a && !b {
			s[p[1]] = cur[p[1]]
		} else if b && !a {
			s[p[0]] = cur[p[0]]
		}
	}
	for _, f := range Fields {
		v, ok := s[f]
		if !ok {
			continue
		}
		k := vorbisKeys[f]
		drop := map[string]bool{k.key: true}
		for _, r := range k.read {
			drop[r] = true
		}
		kept, at := c.list[:0:0], -1
		for _, e := range c.list {
			if key, _, ok := entryKey(e); ok && drop[key] {
				if at < 0 {
					at = len(kept)
				}
				continue
			}
			kept = append(kept, e)
		}
		if v != "" {
			e := k.key + "=" + v
			if at < 0 || at >= len(kept) {
				kept = append(kept, e)
			} else {
				kept = append(kept, "")
				copy(kept[at+1:], kept[at:])
				kept[at] = e
			}
		}
		c.list = kept
	}
}

// Pictures in comments (Ogg, and FLACs some taggers made): METADATA_BLOCK_PICTURE, base64 of a FLAC
// picture block; old files may have COVERART (base64 of the image) and COVERARTMIME instead.
const blockPicture = "METADATA_BLOCK_PICTURE"

// pictureKey is whether key holds a picture, or a part of one.
func pictureKey(key string) bool {
	return key == blockPicture || key == "COVERART" || key == "COVERARTMIME"
}

// hasPicture is whether the comments hold a picture.
func (c *comments) hasPicture() bool {
	for _, e := range c.list {
		if k, _, ok := entryKey(e); ok && pictureKey(k) {
			return true
		}
	}
	return false
}

// picture is the front cover the comments hold, else their first picture; nil without one. Only the
// start of each picture is decoded to choose; then the one chosen is, whole.
func (c *comments) picture() *picRef {
	vals := c.values(blockPicture)
	front, first := -1, -1
	for i, v := range vals {
		p := pictureHead64(v)
		if p == nil {
			continue
		}
		if first < 0 {
			first = i
		}
		if p.Type == 3 {
			front = i
			break
		}
	}
	for _, i := range []int{front, first} {
		if i < 0 {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(vals[i]))
		if err != nil {
			continue
		}
		p, off, n, err := parsePicture(b)
		if err != nil {
			continue
		}
		p.Data, p.Off, p.Len, p.hasOffset = b[off:off+n], 0, 0, false
		return p
	}
	for _, v := range c.values("COVERART") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v)); err == nil && len(b) > 0 {
			mime := pictureInfo(b).MIME
			if m := c.values("COVERARTMIME"); mime == "" && len(m) > 0 {
				mime = m[0]
			}
			return &picRef{MIME: mime, Type: 3, Data: b}
		}
	}
	return nil
}

// pictureHead64 is the fields of a base64'd picture block, only its start decoded; nil when they
// don't read. Its Len is checked against the most the base64 could hold, not the exact length.
func pictureHead64(v string) *picRef {
	v = strings.TrimSpace(v)
	size := int64(base64.StdEncoding.DecodedLen(len(v)))
	d := base64.NewDecoder(base64.StdEncoding, strings.NewReader(v))
	b := make([]byte, min(size, 1<<10))
	k, _ := io.ReadFull(d, b)
	b = b[:k]
	for {
		p, need, err := pictureFields(b, size)
		if err != nil {
			return nil
		}
		if need == 0 {
			return p
		}
		if need > pictureMaxHead {
			return nil
		}
		more := make([]byte, need-int64(len(b)))
		if k, _ := io.ReadFull(d, more); k < len(more) {
			return nil
		}
		b = append(b, more...)
	}
}

// setPicture replaces every picture the comments hold with p, or removes them all when p is nil.
func (c *comments) setPicture(p *Picture) {
	kept := c.list[:0:0]
	for _, e := range c.list {
		if k, _, ok := entryKey(e); ok && pictureKey(k) {
			continue
		}
		kept = append(kept, e)
	}
	if p != nil {
		kept = append(kept, blockPicture+"="+base64.StdEncoding.EncodeToString(encodePicture(p)))
	}
	c.list = kept
}

// pictureMaxHead is the longest a picture block's fields before its image may be: a MIME type and a
// description longer than that aren't a cover's, and aren't read into memory.
const pictureMaxHead = 1 << 20

// pictureFields reads the fields before the image of a FLAC picture block of size bytes from b, its
// first bytes: the image is Len bytes at Off in the block. When b is too short to hold them, need is
// how many of the block's first bytes would do (more than len(b)), and p is nil.
func pictureFields(b []byte, size int64) (p *picRef, need int64, err error) {
	u32 := func(at int64) (int64, bool) {
		if at+4 > int64(len(b)) {
			return 0, false
		}
		return int64(binary.BigEndian.Uint32(b[at:])), true
	}
	if size < 32 {
		return nil, 0, ErrCorrupt
	}
	ml, ok := u32(4)
	if !ok {
		return nil, 32, nil
	}
	if 12+ml > size {
		return nil, 0, ErrCorrupt
	}
	dl, ok := u32(8 + ml)
	if !ok {
		return nil, 32 + ml, nil
	}
	head := 32 + ml + dl // with the width, height, depth, colours and the image's length
	if head > size {
		return nil, 0, ErrCorrupt
	}
	if head > int64(len(b)) {
		return nil, head, nil
	}
	typ, _ := u32(0)
	n, _ := u32(head - 4)
	if n > size-head {
		return nil, 0, ErrCorrupt
	}
	if typ > 255 {
		typ = 0 // no such type: "other"
	}
	mime, desc := string(b[8:8+ml]), string(b[12+ml:12+ml+dl])
	return &picRef{MIME: mime, Type: byte(typ), Desc: desc, Off: head, Len: n, hasOffset: true}, 0, nil
}

// parsePicture reads a FLAC picture block's body: the image's bytes are b[off:off+n].
func parsePicture(b []byte) (p *picRef, off, n int, err error) {
	p, need, err := pictureFields(b, int64(len(b)))
	if err != nil || need > 0 {
		return nil, 0, 0, ErrCorrupt
	}
	return p, int(p.Off), int(p.Len), nil
}

// pictureHeader is p's FLAC picture block up to its image, which follows it.
func pictureHeader(p *Picture) []byte {
	pi := pictureInfo(p.Data)
	mime := pi.MIME
	if mime == "" {
		mime = p.MIME
	}
	depth := 24
	if mime == "image/png" && len(p.Data) > 25 && (p.Data[25] == 6 || p.Data[25] == 4) {
		depth = 32 // with alpha
	}
	var b bytes.Buffer
	u32 := func(v int) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(v))
		b.Write(n[:])
	}
	typ := int(p.Type)
	if typ == 0 && p.Desc == "" {
		typ = 3
	}
	u32(typ)
	u32(len(mime))
	b.WriteString(mime)
	u32(len(p.Desc))
	b.WriteString(p.Desc)
	u32(pi.Width)
	u32(pi.Height)
	u32(depth)
	u32(0)
	u32(len(p.Data))
	return b.Bytes()
}

// encodePicture is p as a FLAC picture block's body.
func encodePicture(p *Picture) []byte {
	h := pictureHeader(p)
	return append(h[:len(h):len(h)], p.Data...)
}
