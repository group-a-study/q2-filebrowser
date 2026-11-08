package tags

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// Monkey's Audio (.ape): "MAC " and the audio, then an APEv2 tag at the end, before an ID3v1 tag when
// there's one: a 32-byte header, items (a value's length, flags, a key, a NUL, the value: text, or for
// a cover a file name, a NUL and the image) and a 32-byte footer like the header. APEv2 has no
// padding, so a change writes the file again, the new tag where the old one was: the audio doesn't
// move, and every item this doesn't change is copied as it was. Keys are matched in any case. An
// APEv1 tag is read and saved as APEv2; an ID3v1 tag is kept in step, as MP3's is.

const (
	apeMaxText   = 1 << 20 // a longer text value is left in the file, unread
	apeMaxTexts  = 4 << 20 // the most text read from one tag, the shortest first; the rest is left in the file
	apeMaxItems  = 1024    // a tag with more is taken as damaged, and a save never makes one: real ones have tens
	apeHasHeader = 1 << 31
	apeIsHeader  = 1 << 29
	apeBinary    = 1 << 1 // the item type, bits 1 and 2: text, binary, a link
)

func init() { register(apeCodec{}, ".ape") }

type apeCodec struct{}

type apeItem struct {
	key    string
	flags  uint32
	at     int64  // a kept item's place in the file, its header on
	off, n int64  // its value's place and length
	value  []byte // a text value read, or a new item's value
	kept   bool   // the item is in the file as it was
	read   bool   // value holds its text
}

type apeFile struct {
	mac        bool  // "MAC " starts it, after an ID3v2 tag when there's one
	id3v2      bool  // an ID3v2 tag before "MAC "
	start, end int64 // the tag's place, header to footer; the place a new one goes when there's none
	version    int   // 2000 or 1000, 0 without a tag
	items      []*apeItem
	v1         []byte // the ID3v1 tag's 128 bytes, nil without
	odd        string // why it can't be written, "" when it can
}

func le32(b []byte) int64 { return int64(binary.LittleEndian.Uint32(b)) }

func readAPE(r io.ReaderAt, size int64) (*apeFile, error) {
	a := &apeFile{}
	if h, err := readAt(r, 0, 10); err == nil {
		off := id3Len(h)
		a.id3v2 = off > 0
		if m, err := readAt(r, off, 4); err == nil && string(m) == "MAC " {
			a.mac = true
		}
	}
	end := size
	if size >= 128 {
		b, err := readAt(r, size-128, 128)
		if err != nil {
			return nil, err
		}
		if string(b[:3]) == "TAG" && string(b[96:104]) != "APETAGEX" { // not the end of a tag's value
			a.v1, end = b, size-128
		}
	}
	a.start, a.end = end, end
	if !a.mac {
		a.odd = "Inside it isn't Monkey's Audio, so its tags can't be changed here"
	}
	if end < 32 {
		return a, nil
	}
	f, err := readAt(r, end-32, 32)
	if err != nil {
		return nil, err
	}
	if string(f[:8]) != "APETAGEX" {
		return a, nil
	}
	var want []*apeItem // the texts to read once the items are known
	damaged := func() (*apeFile, error) {
		if a.odd == "" {
			a.odd = "Its APE tag is damaged, so its tags can't be changed here"
		}
		return a, apeReadTexts(r, want) // what was found before the damage is still shown
	}
	a.version = int(le32(f[8:]))
	size32, count, flags := le32(f[12:]), le32(f[16:]), le32(f[20:])
	start := end - size32
	if size32 < 32 || start < 4 || flags&apeIsHeader != 0 || a.version != 1000 && a.version != 2000 {
		a.version = 0
		return damaged()
	}
	a.start = start
	if flags&apeHasHeader != 0 && a.version >= 2000 && start >= 36 {
		if h, err := readAt(r, start-32, 32); err == nil && string(h[:8]) == "APETAGEX" {
			if le32(h[12:]) != size32 || le32(h[16:]) != count {
				return damaged() // a header that doesn't agree with its footer
			}
			a.start = start - 32
		}
	}
	if count > apeMaxItems {
		return damaged()
	}
	off, stop := start, end-32
	for int64(len(a.items)) < count {
		if off+9 > stop {
			return damaged()
		}
		h, err := readAt(r, off, min(8+256, stop-off))
		if err != nil {
			return nil, err
		}
		k := bytes.IndexByte(h[8:], 0)
		it := &apeItem{key: string(h[8 : 8+max(k, 0)]), flags: uint32(le32(h[4:])), at: off, n: le32(h), kept: true}
		it.off = off + 8 + int64(k) + 1
		if k < 1 || !apeKey(it.key) || it.off+it.n > stop {
			return damaged()
		}
		// A text a field is read from is read, and in APEv1 every text, which saving turns into UTF-8;
		// the others are copied as they are.
		if (a.version == 1000 || apeField(it.key)) && it.flags&6 == 0 && it.n <= apeMaxText {
			want = append(want, it)
		}
		a.items = append(a.items, it)
		off = it.off + it.n
	}
	if off != stop {
		// The items must fill the tag: a size past them, or a count short of them, would have a save
		// drop what isn't the tag's (the end of the audio) or items it never read.
		return damaged()
	}
	if err := apeReadTexts(r, want); err != nil {
		return nil, err
	}
	return a, nil
}

// apeReadTexts reads the items' values, the shortest first, until apeMaxTexts of text is read: a few
// long values can't leave a track number unread. The rest stay in the file, copied as they are.
func apeReadTexts(r io.ReaderAt, items []*apeItem) error {
	sort.SliceStable(items, func(i, j int) bool { return items[i].n < items[j].n })
	text := int64(0)
	for _, it := range items {
		if text+it.n > apeMaxTexts {
			break
		}
		v, err := readAt(r, it.off, it.n)
		if err != nil {
			return err
		}
		it.value, it.read, text = v, true, text+it.n
	}
	return nil
}

// apeKey is whether k can be an APE key: printable ASCII.
func apeKey(k string) bool {
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7E {
			return false
		}
	}
	return true
}

// apeKeys are the keys each field is read from, the first the one written; setting a field removes
// the others. Track and disc hold "3/12"; their totals are read from the others when that has none.
var apeKeys = map[Field][]string{
	Title: {"Title"}, Artist: {"Artist"}, Album: {"Album"},
	AlbumArtist: {"Album Artist", "AlbumArtist", "Album_Artist"},
	Track:       {"Track", "TrackNumber"}, TrackTotal: {"TrackTotal", "TotalTracks"},
	Disc: {"Disc", "DiscNumber"}, DiscTotal: {"DiscTotal", "TotalDiscs"},
	Date: {"Year", "Date"}, Genre: {"Genre"}, Composer: {"Composer"}, Comment: {"Comment"},
	Lyrics: {"Lyrics", "UnsyncedLyrics"},
}

// apeField is whether key is one a field is read from.
func apeField(key string) bool {
	for _, keys := range apeKeys {
		if apeIs(key, keys) {
			return true
		}
	}
	return false
}

func apeIs(key string, keys []string) bool {
	for _, k := range keys {
		if strings.EqualFold(key, k) {
			return true
		}
	}
	return false
}

// get is the first of keys' text, its values (split by NULs) joined as the Q2 joins them.
func (a *apeFile) get(keys []string) string {
	for _, k := range keys {
		for _, it := range a.items {
			if it.read && strings.EqualFold(it.key, k) {
				v := it.value
				if !utf8.Valid(v) {
					v = []byte(id3Latin1(v)) // APEv1's, from before UTF-8
				}
				return id3Join(strings.Split(string(v), "\x00"))
			}
		}
	}
	return ""
}

func (a *apeFile) fields() map[Field]string {
	out := map[Field]string{}
	put := func(f Field, v string) {
		if v != "" {
			out[f] = v
		}
	}
	for _, f := range Fields {
		switch f {
		case Track, Disc:
			n, of := splitNum(a.get(apeKeys[f]))
			put(f, n)
			total := TrackTotal
			if f == Disc {
				total = DiscTotal
			}
			if of == "" {
				of = a.get(apeKeys[total])
			}
			put(total, of)
		case TrackTotal, DiscTotal:
		default:
			put(f, a.get(apeKeys[f]))
		}
	}
	return out
}

// set puts key's new item where the first item keys name was, removing the others; with none, at
// the end. An empty v removes them all.
func (a *apeFile) set(keys []string, v string) {
	at := -1
	kept := make([]*apeItem, 0, len(a.items)+1)
	for _, it := range a.items {
		if apeIs(it.key, keys) {
			if at < 0 {
				at = len(kept)
			}
			continue
		}
		kept = append(kept, it)
	}
	if v != "" {
		if at < 0 {
			at = len(kept)
		}
		kept = append(kept, nil)
		copy(kept[at+1:], kept[at:])
		kept[at] = &apeItem{key: keys[0], value: []byte(v), read: true}
	}
	a.items = kept
}

// apply makes c's changes. A number and its total are one item ("3/12"): setting either keeps the
// other, and a total without its number isn't written. An item never read (too long, or not text)
// goes only when its own field is set: the half of a number not set keeps its items then, and the
// half set goes in an item of its own.
func (a *apeFile) apply(c Changes) {
	cur := a.fields()
	for _, f := range Fields {
		v, ok := c.Set[f]
		if !ok {
			continue
		}
		switch f {
		case Track, TrackTotal, Disc, DiscTotal:
			n, total := Track, TrackTotal
			if f == Disc || f == DiscTotal {
				n, total = Disc, DiscTotal
			}
			num, setNum := c.Set[n]
			of, setTotal := c.Set[total]
			if !setNum && a.unread(apeKeys[n]) && a.get(apeKeys[n]) == "" {
				a.set(apeKeys[total], of) // the number stays as it is, in an item this can't rewrite
				continue
			}
			if !setNum {
				num = cur[n]
			}
			if !setTotal {
				of = cur[total]
			}
			if num != "" && of != "" {
				num += "/" + of
			}
			if setTotal || !a.unread(apeKeys[total]) {
				a.set(apeKeys[total], "")
			}
			a.set(apeKeys[n], num)
		default:
			a.set(apeKeys[f], v)
		}
	}
	if c.Picture != nil || c.RemovePicture {
		a.items = apeDrop(a.items, func(it *apeItem) bool { return apeCover(it.key) })
	}
	if p := c.Picture; p != nil {
		name := "cover.jpg"
		if p.MIME == "image/png" {
			name = "cover.png"
		}
		v := append(append([]byte(name), 0), p.Data...)
		a.items = append(a.items, &apeItem{key: "Cover Art (Front)", flags: apeBinary, value: v})
	}
}

// unread is whether an item keys name was never read.
func (a *apeFile) unread(keys []string) bool {
	for _, it := range a.items {
		if !it.read && apeIs(it.key, keys) {
			return true
		}
	}
	return false
}

func apeDrop(items []*apeItem, del func(*apeItem) bool) []*apeItem {
	out := items[:0]
	for _, it := range items {
		if !del(it) {
			out = append(out, it)
		}
	}
	return out
}

func apeCover(key string) bool {
	return len(key) > 11 && strings.EqualFold(key[:11], "Cover Art (")
}

// picture is the front cover, else the first cover; nil without.
func (a *apeFile) picture(r io.ReaderAt) *picRef {
	var first *picRef
	for _, it := range a.items {
		if !apeCover(it.key) || it.flags&6 != apeBinary {
			continue
		}
		h, err := readAt(r, it.off, min(it.n, 256))
		if err != nil {
			continue
		}
		k := bytes.IndexByte(h, 0)
		if k < 0 || int64(k)+1 >= it.n {
			continue
		}
		mime := "image/jpeg"
		if strings.HasSuffix(strings.ToLower(string(h[:k])), ".png") {
			mime = "image/png"
		}
		front := strings.EqualFold(it.key, "Cover Art (Front)")
		typ := byte(0)
		if front {
			typ = 3
		}
		p := atFile(mime, typ, string(h[:k]), it.off+int64(k)+1, it.n-int64(k)-1)
		if front {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

func (apeCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	a, err := readAPE(r, size)
	if err != nil {
		return nil, nil, err
	}
	info := &Info{Format: "APE", Writable: a.odd == "", Fields: a.fields()}
	switch {
	case a.version == 2000:
		info.Format = "APE (APEv2)"
	case a.version == 1000:
		info.Format = "APE (APEv1)"
		info.Notes = append(info.Notes, "Saving turns its APEv1 tag into APEv2")
	case a.v1 != nil:
		info.Format, info.Fields = "APE (ID3v1)", id3v1Fields(a.v1)
		info.Notes = append(info.Notes, "It has an old ID3v1 tag only: saving adds an APEv2 tag and keeps the ID3v1 one in step")
	case info.Writable:
		info.Format = "APE (no tag yet)"
	}
	if a.odd != "" {
		info.Notes = append(info.Notes, a.odd)
	}
	if a.id3v2 {
		info.Notes = append(info.Notes, "It has an ID3v2 tag at the start too, which is left as it is")
	}
	return info, a.picture(r), nil
}

// length is the tag's length as it will be written, header and footer included.
func (a *apeFile) length() int64 {
	n := int64(64)
	for _, it := range a.items {
		if it.kept {
			n += it.off + it.n - it.at
		} else {
			n += 8 + int64(len(it.key)) + 1 + int64(len(it.value))
		}
	}
	return n
}

// writeTo writes the tag: kept items are copied from src, those side by side there in one go (a copy
// takes a 32 KB buffer, which a thousand items once took one each of).
func (a *apeFile) writeTo(w io.Writer, src io.ReaderAt) error {
	edge := func(flags int64) []byte {
		b := make([]byte, 32)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], 2000)
		binary.LittleEndian.PutUint32(b[12:], uint32(a.length()-32))
		binary.LittleEndian.PutUint32(b[16:], uint32(len(a.items)))
		binary.LittleEndian.PutUint32(b[20:], uint32(flags))
		return b
	}
	if _, err := w.Write(edge(apeHasHeader | apeIsHeader)); err != nil {
		return err
	}
	from, to := int64(0), int64(0) // the kept items not yet copied
	flush := func() error {
		if to == from {
			return nil
		}
		err := copyRange(w, src, from, to-from)
		from, to = 0, 0
		return err
	}
	for _, it := range a.items {
		if it.kept {
			if to > from && it.at != to {
				if err := flush(); err != nil {
					return err
				}
			}
			if to == from {
				from = it.at
			}
			to = it.off + it.n
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		h := make([]byte, 8, 8+len(it.key)+1)
		binary.LittleEndian.PutUint32(h, uint32(len(it.value)))
		binary.LittleEndian.PutUint32(h[4:], it.flags)
		h = append(append(h, it.key...), 0)
		if _, err := w.Write(h); err != nil {
			return err
		}
		if _, err := w.Write(it.value); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	_, err := w.Write(edge(apeHasHeader))
	return err
}

func (apeCodec) write(path string, c Changes) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	a, err := readAPE(f, size)
	if err != nil {
		return err
	}
	if a.odd != "" {
		return ErrReadOnly
	}
	if a.version == 0 && a.v1 != nil {
		a.apply(Changes{Set: id3v1Fields(a.v1)}) // a new tag starts with what ID3v1 says
	}
	if a.version == 1000 {
		// APEv1's text came before UTF-8: what isn't that is Latin-1, and becomes UTF-8 for APEv2.
		for _, it := range a.items {
			if it.read && !utf8.Valid(it.value) {
				it.value, it.kept = []byte(id3Latin1(it.value)), false
			}
		}
	}
	a.apply(c)
	if a.length()-32 > 0xFFFFFFFF || len(a.items) > apeMaxItems {
		return ErrTooBig // past apeMaxItems, the next read would call the tag damaged
	}
	f.Close()
	var v1 []byte
	if a.v1 != nil {
		v1 = id3v1Update(a.v1, c.Set)
	}
	return rewrite(path, a.start+a.length()+size-a.end, func(src *os.File, w io.Writer) error {
		if err := copyRange(w, src, 0, a.start); err != nil {
			return err
		}
		if err := a.writeTo(w, src); err != nil {
			return err
		}
		if v1 != nil {
			_, err := w.Write(v1)
			return err
		}
		return copyRange(w, src, a.end, size-a.end)
	})
}
