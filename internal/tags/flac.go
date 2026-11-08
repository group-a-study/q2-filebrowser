package tags

import (
	"bufio"
	"io"
	"os"
)

// FLAC: "fLaC", then metadata blocks (STREAMINFO first, one flagged last), then the audio. The tags are
// the VORBIS_COMMENT block, the pictures PICTURE blocks. A write keeps every other block as it was
// (SEEKTABLE, APPLICATION, CUESHEET, and MQA's), and fits the new blocks into the room PADDING held,
// so the audio stays where it is; only when they don't fit is the file written again, with 8 KB of
// padding for next time. An ID3 tag some taggers put before "fLaC" is kept and not changed.
//
// In place, a kept block stays where it is, its bytes neither read nor written, and the new blocks
// go into the room around it (the old comment block's, the old pictures', the padding's); what is
// left of each room becomes padding. When moving the kept blocks up towards the start moves little
// (flacNear), they move up instead, a piece at a time, and the new blocks go after them. Nothing moves
// towards the end, and a cover is never held in memory to be written back. When no room is big enough
// and making one would move more, a cover say, the file is written again: an app that ends cuts a
// write short, and a move cut short loses the song where a rewrite leaves it as it was.

const (
	flacStreamInfo = 0
	flacPadding    = 1
	flacSeekTable  = 3
	flacComment    = 4
	flacPicture    = 6
	flacMaxBlock   = 1<<24 - 1
	flacMaxBlocks  = 1024 // far more than any real file has
	flacNewPadding = 8 << 10
	flacNear       = 64 << 10 // little enough to move, or to write again rather than split a write in two
)

type flacBlock struct {
	typ  byte
	last bool
	off  int64 // its header's offset in the file
	size int64 // its body's length
}

type flacFile struct {
	start  int64 // where "fLaC" is: after a leading ID3 tag
	blocks []flacBlock
	audio  int64 // where the metadata ends
}

func init() { register(flacCodec{}, ".flac") }

type flacCodec struct{}

// id3Skip is the length of an ID3v2 tag at off, 0 without one.
func id3Skip(r io.ReaderAt, off int64) int64 {
	h := make([]byte, 10)
	if n, _ := r.ReadAt(h, off); n < 10 || string(h[:3]) != "ID3" {
		return 0
	}
	sz := int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
	if h[5]&0x10 != 0 {
		sz += 10 // a footer
	}
	return 10 + sz
}

func readFLAC(r io.ReaderAt, size int64) (*flacFile, error) {
	ff := &flacFile{}
	for ff.start < size {
		n := id3Skip(r, ff.start)
		if n == 0 {
			break
		}
		ff.start += n
	}
	magic, err := readAt(r, ff.start, 4)
	if err != nil || string(magic) != "fLaC" {
		return nil, ErrCorrupt
	}
	off := ff.start + 4
	for {
		h, err := readAt(r, off, 4)
		if err != nil {
			return nil, ErrCorrupt
		}
		b := flacBlock{typ: h[0] & 0x7f, last: h[0]&0x80 != 0, off: off,
			size: int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])}
		if b.typ == 127 || off+4+b.size > size || len(ff.blocks) == 0 && b.typ != flacStreamInfo ||
			len(ff.blocks) == flacMaxBlocks {
			return nil, ErrCorrupt
		}
		ff.blocks = append(ff.blocks, b)
		off += 4 + b.size
		if b.last {
			break
		}
	}
	ff.audio = off
	return ff, nil
}

// comments are the file's Vorbis comments, nil without a comment block, and how many blocks held them.
// A file should have one; when it has more, the first's come first, then the others' (ffmpeg reads
// them all, and a save makes them one).
func (ff *flacFile) comments(r io.ReaderAt) (*comments, int, error) {
	var cm *comments
	n := 0
	for _, b := range ff.blocks {
		if b.typ != flacComment {
			continue
		}
		c, _, err := readComments(bufio.NewReaderSize(io.NewSectionReader(r, b.off+4, b.size), 4<<10), b.size)
		if err != nil {
			return nil, 0, err
		}
		n++
		if cm == nil {
			cm = c
			continue
		}
		if len(cm.list)+len(c.list) > maxComments {
			return nil, 0, ErrCorrupt
		}
		cm.list = append(cm.list, c.list...)
	}
	return cm, n, nil
}

// picture is picture block b, only its fields read: the image is left where it is in the file. Nil
// when it doesn't read.
func (ff *flacFile) picture(r io.ReaderAt, b flacBlock) *picRef {
	n := min(b.size, 1<<10)
	for {
		head, err := readAt(r, b.off+4, n)
		if err != nil {
			return nil
		}
		p, need, err := pictureFields(head, b.size)
		if err != nil {
			return nil
		}
		if need == 0 {
			p.Off += b.off + 4
			return p
		}
		if need <= n || need > pictureMaxHead {
			return nil
		}
		n = need
	}
}

func (flacCodec) read(r io.ReaderAt, size int64) (*Info, *picRef, error) {
	ff, err := readFLAC(r, size)
	if err != nil {
		return nil, nil, err
	}
	info := &Info{Format: "FLAC", Writable: true, Fields: map[Field]string{}}
	if ff.start > 0 {
		info.Notes = append(info.Notes, "It has an ID3 tag before the FLAC data too, which is left as it is")
	}
	cm, n, err := ff.comments(r)
	if err != nil {
		return nil, nil, err
	}
	if cm != nil {
		info.Fields = cm.fields()
	}
	if n > 1 {
		info.Notes = append(info.Notes, "Its tags are in more than one comment block; a save makes them one")
	}
	var pic *picRef
	for _, b := range ff.blocks {
		if b.typ != flacPicture || pic != nil && pic.Type == 3 {
			continue
		}
		if p := ff.picture(r, b); p != nil && (pic == nil || p.Type == 3) {
			pic = p
		}
	}
	if pic == nil && cm != nil {
		pic = cm.picture() // a cover some taggers keep in the comments, as in Ogg
	}
	return info, pic, nil
}

// flacHeader is a block's 4-byte header.
func flacHeader(typ byte, last bool, n int) []byte {
	h := []byte{typ, byte(n >> 16), byte(n >> 8), byte(n)}
	if last {
		h[0] |= 0x80
	}
	return h
}

// flacOut is a block as a write lays it out: kept, its body copied from the file; new, its body
// written by body; or padding, zeros.
type flacOut struct {
	typ  byte
	at   int64 // where its header goes
	size int64 // its body's length
	keep *flacBlock
	body func(w io.Writer) error
}

// flacLay puts blocks one after another from at, then padding up to end; false when they don't fit.
func flacLay(blocks []*flacOut, at, end int64) ([]flacOut, bool) {
	var out []flacOut
	for _, b := range blocks {
		o := *b
		o.at = at
		at += 4 + o.size
		out = append(out, o)
	}
	switch room := end - at; {
	case room == 0:
	case room >= 4 && room-4 <= flacMaxBlock:
		out = append(out, flacOut{typ: flacPadding, at: at, size: room - 4})
	default:
		return nil, false
	}
	return out, true
}

func (flacCodec) write(path string, c Changes) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	pl, err := planFLAC(f, st.Size(), c)
	f.Close()
	if err != nil {
		return err
	}
	if pl.again == nil {
		return patchRuns(path, pl.runs)
	}
	return rewrite(path, pl.size, pl.writeAgain)
}

// flacPlan is how a write changes a file: in place, the runs to write (none when nothing changes);
// else the blocks to write the file again with, size bytes in all.
type flacPlan struct {
	ff    *flacFile
	runs  [][]flacSeg
	again []flacOut
	size  int64 // the whole file's, written again
	old   int64 // the file's size now
}

func planFLAC(f io.ReaderAt, size int64, c Changes) (*flacPlan, error) {
	ff, err := readFLAC(f, size)
	if err != nil {
		return nil, err
	}
	cm, vcs, err := ff.comments(f)
	if err != nil {
		return nil, err
	}
	picChange := c.Picture != nil || c.RemovePicture
	// The comments are written again when a field changes, when they hold a cover a new one replaces
	// (some taggers put it there), and to make two comment blocks one.
	var vc, pic *flacOut
	if len(c.Set) > 0 || vcs > 1 || picChange && cm != nil && cm.hasPicture() {
		if cm == nil {
			cm = &comments{vendor: "q2-filebrowser"}
		}
		cm.apply(c.Set)
		if picChange {
			cm.setPicture(nil)
		}
		if vcs > 0 || len(cm.list) > 0 {
			vc = &flacOut{typ: flacComment, size: cm.size(), body: cm.writeTo}
		}
	}
	if c.Picture != nil {
		head, data := pictureHeader(c.Picture), c.Picture.Data
		pic = &flacOut{typ: flacPicture, size: int64(len(head) + len(data)), body: func(w io.Writer) error {
			if _, err := w.Write(head); err != nil {
				return err
			}
			_, err := w.Write(data)
			return err
		}}
	}
	var news []*flacOut
	for _, b := range []*flacOut{vc, pic} {
		if b != nil {
			if b.size > flacMaxBlock {
				return nil, ErrTooBig
			}
			news = append(news, b)
		}
	}
	newVC := vc != nil
	gone := func(b flacBlock) bool {
		return b.typ == flacPadding || b.typ == flacComment && newVC || b.typ == flacPicture && picChange
	}
	pl := &flacPlan{ff: ff, old: size}
	if outs := ff.inPlace(news, gone); outs != nil {
		pl.runs = ff.changes(outs)
		return pl, nil
	}
	// Written again, in the file's order: the comments where the first comment block was (else after
	// STREAMINFO and a SEEKTABLE), the picture where the first old one was (else last), then padding.
	var order []*flacOut
	for i, b := range ff.blocks {
		switch {
		case b.typ == flacComment && vc != nil:
			order, vc = append(order, vc), nil
		case b.typ == flacPicture && pic != nil:
			order, pic = append(order, pic), nil
		case !gone(b):
			order = append(order, &flacOut{typ: b.typ, size: b.size, keep: &ff.blocks[i]})
		}
	}
	if vc != nil {
		at := 1
		if len(order) > 1 && order[1].typ == flacSeekTable {
			at = 2
		}
		order = append(order[:at], append([]*flacOut{vc}, order[at:]...)...)
	}
	if pic != nil {
		order = append(order, pic)
	}
	var need int64
	for _, b := range order {
		need += 4 + b.size
	}
	end := ff.start + 4 + need + 4 + flacNewPadding
	pl.again, _ = flacLay(order, ff.start+4, end)
	pl.size = end + size - ff.audio
	return pl, nil
}

// writeAgain writes the whole file as the plan has it, from src as it is.
func (pl *flacPlan) writeAgain(src *os.File, w io.Writer) error {
	ff, buf := pl.ff, make([]byte, 64<<10)
	if err := copyBuf(w, src, 0, ff.start, buf); err != nil {
		return err
	}
	if _, err := w.Write([]byte("fLaC")); err != nil {
		return err
	}
	for i, o := range pl.again {
		if _, err := w.Write(flacHeader(o.typ, i == len(pl.again)-1, int(o.size))); err != nil {
			return err
		}
		if err := o.writeBody(src, w, buf); err != nil {
			return err
		}
	}
	return copyBuf(w, src, ff.audio, pl.old-ff.audio, buf)
}

// writeBody writes o's body: a kept block's bytes from src (through buf), a new one's, or zeros.
func (o *flacOut) writeBody(src io.ReaderAt, w io.Writer, buf []byte) error {
	switch {
	case o.keep != nil:
		return copyBuf(w, src, o.keep.off+4, o.size, buf)
	case o.body != nil:
		return o.body(w)
	}
	return writeZeros(w, o.size)
}

// inPlace lays news out in the room the metadata has now, with the blocks that aren't gone; nil when
// they don't fit with the kept blocks where they are, or moved up by flacNear in all at most. They
// move up when that moves little (it saves padding in the middle); else they stay where they are.
func (ff *flacFile) inPlace(news []*flacOut, gone func(flacBlock) bool) []flacOut {
	var kept []*flacOut
	for i, b := range ff.blocks {
		if !gone(b) {
			kept = append(kept, &flacOut{typ: b.typ, size: b.size, keep: &ff.blocks[i]})
		}
	}
	packed, ok := flacLay(append(kept[:len(kept):len(kept)], news...), ff.start+4, ff.audio)
	var moved int64
	for _, o := range packed {
		if o.keep != nil && o.keep.off != o.at {
			moved += 4 + o.size
		}
	}
	if ok && moved <= flacNear {
		return packed
	}
	return ff.stay(kept, news, gone) // nil when it doesn't fit: written again
}

// stay lays news out with every kept block where it is, in the rooms between them: each new block
// where its old one was when it fits, else in the last room, else the first with space; what a room
// has left is padding. Nil when they don't fit.
func (ff *flacFile) stay(kept, news []*flacOut, gone func(flacBlock) bool) []flacOut {
	type slot struct {
		keep    *flacOut // a kept block, else a room
		at, end int64
		fill    []*flacOut
	}
	var slots, rooms []*slot
	at := ff.start + 4
	for _, k := range kept {
		if k.keep.off > at {
			slots = append(slots, &slot{at: at, end: k.keep.off})
		}
		slots = append(slots, &slot{keep: k})
		at = k.keep.off + 4 + k.size
	}
	if at < ff.audio {
		slots = append(slots, &slot{at: at, end: ff.audio})
	}
	for _, s := range slots {
		if s.keep == nil {
			rooms = append(rooms, s)
		}
	}
	if len(rooms) == 0 {
		return nil
	}
	// home is the room that held the first old block of b's kind.
	home := func(b *flacOut) *slot {
		for _, old := range ff.blocks {
			if old.typ != b.typ || !gone(old) {
				continue
			}
			for _, r := range rooms {
				if r.at <= old.off && old.off < r.end {
					return r
				}
			}
		}
		return nil
	}
	fits := func(r *slot, b *flacOut) bool {
		left := r.end - r.at - 4 - b.size
		for _, o := range r.fill {
			left -= 4 + o.size
		}
		return left == 0 || left >= 4 && left-4 <= flacMaxBlock
	}
	place := func(order []*flacOut) bool {
		for _, r := range rooms {
			r.fill = nil
		}
		for _, b := range order {
			done := false
			for _, r := range append([]*slot{home(b), rooms[len(rooms)-1]}, rooms...) {
				if r != nil && fits(r, b) {
					r.fill, done = append(r.fill, b), true
					break
				}
			}
			if !done {
				return false
			}
		}
		return true
	}
	if !place(news) && (len(news) < 2 || !place([]*flacOut{news[1], news[0]})) {
		return nil
	}
	var outs []flacOut
	for _, s := range slots {
		if s.keep != nil {
			o := *s.keep
			o.at = o.keep.off
			outs = append(outs, o)
			continue
		}
		fill := s.fill
		if len(fill) == 2 && fill[0].typ == flacPicture {
			fill = []*flacOut{fill[1], fill[0]} // the comments before the picture, as files have them
		}
		laid, ok := flacLay(fill, s.at, s.end)
		if !ok {
			return nil
		}
		outs = append(outs, laid...)
	}
	return outs
}

// flacSeg is a piece of the metadata as it will be: same when the file has those bytes there already.
type flacSeg struct {
	at, n int64
	same  bool
	moved bool // a kept block's bytes from somewhere else
	zero  bool // padding's zeros
	write func(src io.ReaderAt, w io.Writer) error
}

// changes are the runs of pieces to write to make the metadata outs, which fill its room exactly, in
// the order to write them: the pieces that change, with what's unchanged between two of them when
// that's short; the last run first, so a new block is there before the old one goes. When a kept
// block moves, one run from the first change to the last.
func (ff *flacFile) changes(outs []flacOut) [][]flacSeg {
	old := map[int64][4]byte{}
	var pads [][2]int64 // the old padding's bodies: zeros already
	for _, b := range ff.blocks {
		old[b.off] = [4]byte(flacHeader(b.typ, b.last, int(b.size)))
		if b.typ == flacPadding {
			pads = append(pads, [2]int64{b.off + 4, b.off + 4 + b.size})
		}
	}
	var segs []flacSeg
	zeros := func(at, n int64, same bool) {
		segs = append(segs, flacSeg{at: at, n: n, same: same, zero: true, write: func(_ io.ReaderAt, w io.Writer) error {
			return writeZeros(w, n)
		}})
	}
	for i := range outs {
		o := &outs[i]
		h := flacHeader(o.typ, i == len(outs)-1, int(o.size))
		was, ok := old[o.at]
		segs = append(segs, flacSeg{at: o.at, n: 4, same: ok && was == [4]byte(h),
			write: func(_ io.ReaderAt, w io.Writer) error { _, err := w.Write(h); return err }})
		at, end := o.at+4, o.at+4+o.size
		switch {
		case o.keep != nil:
			from := o.keep.off + 4
			segs = append(segs, flacSeg{at: at, n: o.size, same: from == at, moved: from != at,
				write: func(src io.ReaderAt, w io.Writer) error { return copyBuf(w, src, from, o.size, nil) }})
		case o.body != nil:
			segs = append(segs, flacSeg{at: at, n: o.size,
				write: func(_ io.ReaderAt, w io.Writer) error { return o.body(w) }})
		default:
			// Padding: where there was padding the zeros are there already.
			for _, p := range pads {
				if p[1] <= at || p[0] >= end {
					continue
				}
				if p[0] > at {
					zeros(at, p[0]-at, false)
					at = p[0]
				}
				n := min(end, p[1]) - at
				zeros(at, n, true)
				at += n
			}
			if at < end {
				zeros(at, end-at, false)
			}
		}
	}
	var runs [][2]int
	moved := false
	for i := 0; i < len(segs); {
		if segs[i].same {
			i++
			continue
		}
		j := i + 1
		for {
			k, n := j, int64(0)
			for k < len(segs) && segs[k].same && n <= flacNear {
				n += segs[k].n
				k++
			}
			if k == len(segs) || segs[k].same || n > flacNear {
				break
			}
			j = k + 1
		}
		for _, s := range segs[i:j] {
			moved = moved || s.moved
		}
		runs = append(runs, [2]int{i, j})
		i = j
	}
	if moved && len(runs) > 1 {
		runs = [][2]int{{runs[0][0], runs[len(runs)-1][1]}}
	}
	// A run of padding's zeros alone (an old cover's bytes inside new padding) goes after the others:
	// by then the padding's header says what they are, so a save cut short while zeroing them leaves a
	// file that reads, its padding not yet all zeros.
	var out, fills [][]flacSeg
	for i := len(runs) - 1; i >= 0; i-- {
		run, zero := segs[runs[i][0]:runs[i][1]], true
		for _, s := range run {
			zero = zero && s.zero
		}
		if zero {
			fills = append([][]flacSeg{run}, fills...)
		} else {
			out = append(out, run)
		}
	}
	return append(out, fills...)
}

// patchRuns writes runs in place, in order, each a piece at a time.
func patchRuns(path string, runs [][]flacSeg) error {
	for _, run := range runs {
		at, end := run[0].at, run[len(run)-1].at+run[len(run)-1].n
		err := patchWith(path, at, end-at, func(src *os.File, w io.Writer) error {
			for _, s := range run {
				if err := s.write(src, w); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// flacZeros are zeros to write from; nothing writes into them.
var flacZeros [32 << 10]byte

// writeZeros writes n zeros to w, a piece at a time.
func writeZeros(w io.Writer, n int64) error {
	for n > 0 {
		k := min(n, int64(len(flacZeros)))
		if _, err := w.Write(flacZeros[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// copyBuf copies n bytes at off in src to w through buf (one of its own when nil and w needs it).
func copyBuf(w io.Writer, src io.ReaderAt, off, n int64, buf []byte) error {
	if n == 0 {
		return nil
	}
	if buf == nil {
		if _, ok := w.(io.ReaderFrom); !ok {
			buf = make([]byte, min(n, 32<<10))
		}
	}
	k, err := io.CopyBuffer(w, io.NewSectionReader(src, off, n), buf)
	if err == nil && k != n {
		err = io.ErrUnexpectedEOF
	}
	return err
}
