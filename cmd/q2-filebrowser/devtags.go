package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
	"github.com/group-a-study/q2-filebrowser/internal/tags"
)

// The tag editor on the Q2's page. Edit Tags lists the card's folders and songs (at its root the
// songs playing, Find a Song and Update Library first); a song lists its fields; a field offers
// values to pick and the T9 box to type one; what is typed shows what saving it would change, with
// Save. The levels keep to q2-pod's patch/addon.c: a list row opens its uri with its context as the
// one argument, and a press does nothing when that passes ~398 bytes escaped, so a longer one goes
// as a number kept here; an action's reply is a toast with no rows, then its level is asked for
// again; the T9 box sends "search" with the text alone, so what it is for (the edit target) is
// kept here: the field level sets it, search keeps it, any other list clears it. Only actions
// write.

const (
	devMaxCtx    = 380 // a row's context, escaped: the Q2 takes 398, and a cursor goes after it
	devMaxRefs   = 512 // long contexts kept
	devMaxSeen   = 8   // other songs' values a field offers
	devSeenReads = 40  // songs read for them
)

var (
	// devWait is how long an action waits for its writes; past it the toast says they go on.
	devWait = 10 * time.Second
	// devSeenFor is how long a field reads other songs for their values.
	devSeenFor = 2 * time.Second
	// devFindFor is how long Find a Song looks before each answer, its first page's and every one
	// after: the Q2 offers to stop the app once a level takes 3 s, and the level its More… row
	// opens asks with a cursor, as the pages it asks for by itself do, so neither can take longer.
	devFindFor = 2 * time.Second
	// devLateFor is how long the menu shows how a save the Q2 stopped waiting for ended.
	devLateFor = 10 * time.Minute
	// devLibraryEvery is how often a waiting Update Library looks whether the saves are done.
	devLibraryEvery = 250 * time.Millisecond
	// devReadTags is a song's tags as the levels and saves read them; tests slow it.
	devReadTags = (*files).readTags
	// devReadDir is a folder's names as Find a Song reads them; tests slow it.
	devReadDir = os.ReadDir
)

// devFields are a song's rows, in order.
var devFields = []tags.Field{tags.Title, tags.Artist, tags.Album, tags.AlbumArtist, tags.Track, tags.Disc,
	tags.Date, tags.Genre, tags.Composer}

// devAlbumWide are the fields an album's songs share: Save for All offers them.
var devAlbumWide = map[tags.Field]bool{tags.Album: true, tags.AlbumArtist: true, tags.Artist: true,
	tags.Date: true, tags.Genre: true, tags.Composer: true, tags.Disc: true, tags.DiscTotal: true,
	tags.TrackTotal: true}

// devGenres are offered for Genre without typing, R&B among them: T9 has no &.
var devGenres = []string{"Rock", "Pop", "Jazz", "Classical", "Electronic", "Hip-Hop", "R&B", "Folk", "Metal",
	"Soundtrack", "Country", "Blues"}

// devPlayable are the extensions stock plays from a local row, case and all (navigation.c's
// songs_open finds the song in stock's own folder list).
var devPlayable = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`.mp3 .MP3 .Mp3 .wav .WAV .Wav .wma .WMA .Wma .ape .APE .Ape .flac .FLAC
		.Flac .aiff .Aiff .AIFF .aif .Aif .AIF .m4a .M4A .AAC .aac .Aac .Dff .DFF .dff .dsf .Dsf .DSF .OGG .ogg
		.Ogg .dts .DTS .Dts .mp2 .MP2 .Mp2 .ac3 .AC3 .Ac3 .opus .OPUS .Opus .tak .TAK .Tak`) {
		devPlayable[e] = true
	}
}

// devTarget is what a typed value is for.
type devTarget struct {
	path  string
	field tags.Field
}

// devRef is a context too long for a row, kept for the number the row carries instead.
type devRef struct{ op, ctx string }

// devState is the tag editor's state for one app: the edit target, the long contexts, the song
// search under way, the saves pressed, and how one the Q2 stopped waiting for ended.
type devState struct {
	mu     sync.Mutex
	target *devTarget
	// gen counts the levels asked for that set or clear the target: a field level the Q2 left
	// (Back) before it was answered doesn't set it over a later level's.
	gen     uint64
	refs    map[int]devRef
	ids     map[devRef]int
	next    int
	hunt    *devHunt       // the last song search: its next pages go on from it
	batches []*devBatch    // saves pressed (or posted) and not ended, each counted busy from the press
	late    *devLate       // how the last save that went on in the background ended
	library bool           // an Update Library waits for the saves to end
	asking  sync.WaitGroup // that Update Library, till it has asked: tests wait for it
	// saving is held by an action's writes: one action's at a time, so a second Save for All of a
	// folder waits for the first, not mixing their songs.
	saving sync.Mutex
	// finding is held while a song search walks the card; turn counts the searches asked for, so
	// a walk whose request the Q2 dropped gives way to the next at once.
	finding sync.Mutex
	turn    atomic.Uint64
}

var devStates sync.Map // *app → *devState

func (a *app) devTags() *devState {
	d, _ := devStates.LoadOrStore(a, &devState{})
	return d.(*devState)
}

// aim sets the edit target (nil clears it) for the level asked for now, and is that level's
// number for aimFor.
func (d *devState) aim(t *devTarget) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	d.target = t
	return d.gen
}

// aimFor sets the target for the level numbered gen, unless a later level was asked for since.
func (d *devState) aimFor(gen uint64, t *devTarget) {
	d.mu.Lock()
	if d.gen == gen {
		d.target = t
	}
	d.mu.Unlock()
}

func (d *devState) aimed() *devTarget {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.target
}

// forget drops the song search: a new one starts afresh, and a walk under way stops.
func (d *devState) forget() {
	d.turn.Add(1)
	d.mu.Lock()
	d.hunt = nil
	d.mu.Unlock()
}

// keep is the number standing for op's long ctx.
func (d *devState) keep(op, ctx string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := devRef{op, ctx}
	if n, ok := d.ids[r]; ok {
		return n
	}
	if d.refs == nil {
		d.refs, d.ids = map[int]devRef{}, map[devRef]int{}
	}
	d.next++
	d.refs[d.next], d.ids[r] = r, d.next
	if old, ok := d.refs[d.next-devMaxRefs]; ok {
		delete(d.ids, old)
		delete(d.refs, d.next-devMaxRefs)
	}
	return d.next
}

func (d *devState) ref(n string) (devRef, bool) {
	k, err := strconv.Atoi(n)
	if err != nil {
		return devRef{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.refs[k]
	return r, ok
}

// arg is a level's argument: itself, or the long context "@n" stands for.
func (d *devState) arg(op, s string) (string, bool) {
	if !strings.HasPrefix(s, "@") {
		return s, true
	}
	r, ok := d.ref(s[1:])
	return r.ctx, ok && r.op == op
}

func devFits(ctx string) bool { return len(q2spot.Escape(ctx)) <= devMaxCtx }

// list is a row opening the level op for ctx.
func (d *devState) list(op, ctx, title, sub string) q2spot.Row {
	if !devFits(ctx) {
		ctx = "@" + strconv.Itoa(d.keep(op, ctx))
	}
	return q2spot.Row{Kind: "list", URI: op, Title: title, Subtitle: sub, Context: ctx}
}

// action is a row for the action op (set, setdir) with ctx, or setid with a number for a long one.
func (d *devState) action(op, ctx, title, sub string) q2spot.Row {
	if !devFits(ctx) {
		op, ctx = "setid", strconv.Itoa(d.keep(op, ctx))
	}
	return q2spot.Row{Kind: "action", URI: op, Title: title, Subtitle: sub, Context: ctx}
}

// devTagOp answers the tag editor's levels (tags, song, field, search) and actions (set, setid,
// setdir, library).
func (a *app) devTagOp(req q2spot.Request) q2spot.Reply {
	d := a.devTags()
	switch req.Op {
	case "tags":
		d.aim(nil)
		d.forget()
		return a.devFolder(req)
	case "song":
		d.aim(nil)
		return a.devSong(req)
	case "field":
		return a.devField(req)
	case "search":
		return a.devSearch(req)
	case "set", "setdir":
		return a.devSave(req.Op, req.Arg(0))
	case "setid":
		r, ok := d.ref(req.Arg(0))
		if !ok || r.op != "set" && r.op != "setdir" {
			return devGone()
		}
		return a.devSave(r.op, r.ctx)
	case "library":
		return a.devLibrary()
	}
	return q2spot.Fail(q2spot.Unsupported, "Update File Browser on the card")
}

func devGone() q2spot.Reply {
	return q2spot.Fail(q2spot.Unavailable, "That's gone: go back and choose it again")
}

func devNoCard() q2spot.Reply {
	return q2spot.Fail(q2spot.Unavailable, "The card isn't there just now (USB storage?)")
}

// devFail is err as the Q2 shows it: a level's error page, or an action's toast.
func devFail(err error) q2spot.Reply {
	var e *apiErr
	if errors.As(err, &e) && e.code == 404 {
		return q2spot.Fail(q2spot.Unavailable, e.msg)
	}
	return q2spot.Fail(q2spot.Error, err.Error())
}

// devPage is a level's reply: head (its first page's own rows), then rows from off, as many as fit
// a reply (50 rows, 64 KB). When some are left it says "pages" with the offset to go on from, which
// the Q2 sends back as the level's last argument, asking for the next page by itself.
func devPage(head []q2spot.Row, n, off int, row func(i int) q2spot.Row) q2spot.Reply {
	page := append([]q2spot.Row(nil), head...)
	for i := off; i < n && len(page) < q2spot.MaxRows; i++ {
		page = append(page, row(i))
	}
	_, kept := q2spot.Reply{Message: "pages", Cursor: strconv.Itoa(n), Rows: page}.Encode("4294967295")
	r := q2spot.Reply{Rows: page[:kept]}
	if next := off + max(kept-len(head), 0); next < n {
		r.Message, r.Cursor = "pages", strconv.Itoa(next)
	}
	return r
}

// devCursor is a paged level's offset: 0 without a cursor.
func devCursor(c string) (int, bool) {
	if c == "" {
		return 0, true
	}
	n, err := strconv.Atoi(c)
	return n, err == nil && n >= 0
}

func devByName(s []string) {
	sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i]) < strings.ToLower(s[j]) })
}

// devWhere is the folder p is in, as a row's subtitle.
func devWhere(p string) string { return path.Dir(p) }

// devFolder is the level "tags <folder> [cursor]": at the card's root the songs playing, Find a
// Song and Update Library first; then the folder's folders and songs by name, names alone (no tags
// read, so a big folder lists at once), paged past 50 rows. How a save that went on in the
// background ended comes first, once.
func (a *app) devFolder(req q2spot.Request) q2spot.Reply {
	d := a.devTags()
	arg, ok := d.arg("tags", req.Arg(0))
	if !ok {
		return devGone()
	}
	off, ok := devCursor(req.Arg(1))
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if !cardReady() {
		return devNoCard()
	}
	full, dir, err := a.files.abs(arg)
	if err != nil {
		return devFail(err)
	}
	des, err := os.ReadDir(full)
	if errors.Is(err, fs.ErrNotExist) {
		return q2spot.Fail(q2spot.Unavailable, "That folder isn't on the card any more")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't read the folder: "+err.Error())
	}
	var dirs, songs []string
	for _, de := range des {
		switch name := de.Name(); {
		case hidden(name):
		case de.IsDir():
			dirs = append(dirs, name)
		case isSong(name):
			songs = append(songs, name)
		}
	}
	devByName(dirs)
	devByName(songs)
	var head []q2spot.Row
	if req.Arg(1) == "" {
		head = d.takeLate()
		if dir == "/" {
			head = append(head, a.devRootRows()...)
		} else if len(dirs)+len(songs) == 0 {
			head = append(head, info("No folders or songs here", ""))
		}
	}
	return devPage(head, len(dirs)+len(songs), off, func(i int) q2spot.Row {
		if i < len(dirs) {
			return d.list("tags", path.Join(dir, dirs[i]), dirs[i], "")
		}
		name := songs[i-len(dirs)]
		return d.list("song", path.Join(dir, name), name, "")
	})
}

// devRootRows are the root level's first rows: the songs playing, Find a Song, Update Library.
func (a *app) devRootRows() []q2spot.Row {
	d := a.devTags()
	var rows []q2spot.Row
	for _, p := range devPlaying(a.files.root) {
		rows = append(rows, d.list("song", p, "Playing: "+path.Base(p), devWhere(p)))
	}
	rows = append(rows, q2spot.Row{Kind: "search", Title: "Find a Song", Subtitle: "Type part of its name"})
	if libraryAvailable() {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "library", Title: "Update Library",
			Subtitle: "Reads every song's tags again; waits until nothing plays"})
	}
	return rows
}

// devShow is f's value as a row shows it: a track or disc with its total ("3 of 12"), else "Empty".
func devShow(f tags.Field, fields map[tags.Field]string) string {
	v := fields[f]
	of := ""
	switch f {
	case tags.Track:
		of = fields[tags.TrackTotal]
	case tags.Disc:
		of = fields[tags.DiscTotal]
	}
	switch {
	case v == "" && of == "":
		return "Empty"
	case of == "":
		return v
	case v == "":
		return "? of " + of
	}
	return v + " of " + of
}

// devSong is the level "song <path>": its name and format, a row per field (each opening the
// field's level), Play, and what the editor should know of it. How a save that went on in the
// background ended comes first, once.
func (a *app) devSong(req q2spot.Request) q2spot.Reply {
	d := a.devTags()
	arg, ok := d.arg("song", req.Arg(0))
	if !ok {
		return devGone()
	}
	if !cardReady() {
		return devNoCard()
	}
	full, p, err := a.files.existing(arg)
	if err != nil {
		return devFail(err)
	}
	tf := devReadTags(a.files, p)
	rows := d.takeLate()
	if tf.Info == nil {
		rows = append(rows, info(tf.Name, ""), info(tf.Error, ""))
		return q2spot.Reply{Rows: append(rows, a.devPlay(full)...)}
	}
	rows = append(rows, info(tf.Name, tf.Format))
	if !tf.Writable {
		rows = append(rows, info("Its tags can be read here, not changed", "Use a computer for "+tf.Format))
	}
	for _, f := range devFields {
		if tf.Writable {
			rows = append(rows, d.list("field", string(f)+"|"+p, f.Name(), devShow(f, tf.Fields)))
		} else {
			rows = append(rows, info(f.Name(), devShow(f, tf.Fields)))
		}
	}
	rows = append(rows, a.devPlay(full)...)
	for _, n := range tf.Notes {
		rows = append(rows, info(n, ""))
	}
	return q2spot.Reply{Rows: rows}
}

// devPlay is a Play row for the song at full, when stock plays it from a local row: an extension
// of its list, and a path its 256-byte buffer takes. It plays the folder in name order from there.
func (a *app) devPlay(full string) []q2spot.Row {
	if !devPlayable[filepath.Ext(full)] || len(full) >= 256 {
		return nil
	}
	return []q2spot.Row{{Kind: "local", URI: full, Title: "Play", Subtitle: "Then the rest of its folder",
		Context: "album"}}
}

// devCut is a context's field and the rest: "title|/Music/a.flac". A card path holds no "|" (FAT
// takes none), so a set's "<field>|<path>|<value>" splits at its first two.
func devCut(ctx string) (tags.Field, string, bool) {
	f, rest, ok := strings.Cut(ctx, "|")
	return tags.Field(f), rest, ok && tags.Known(tags.Field(f))
}

// devField is the level "field <field>|<path>": the value now, the T9 box for a new one (the field
// and song being the edit target), and values to pick without typing, the value now aside.
func (a *app) devField(req q2spot.Request) q2spot.Reply {
	d := a.devTags()
	gen := d.aim(nil)
	arg, ok := d.arg("field", req.Arg(0))
	if !ok {
		return devGone()
	}
	f, p, ok := devCut(arg)
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if !cardReady() {
		return devNoCard()
	}
	_, p, err := a.files.existing(p)
	if err != nil {
		return devFail(err)
	}
	tf := devReadTags(a.files, p)
	if tf.Info == nil {
		return q2spot.Fail(q2spot.Error, tf.Error)
	}
	if !tf.Writable {
		return q2spot.Reply{Rows: []q2spot.Row{info(f.Name()+": "+devShow(f, tf.Fields), tf.Name),
			info("Its tags can be read here, not changed", "Use a computer for "+tf.Format)}}
	}
	d.aimFor(gen, &devTarget{path: p, field: f})
	cur := tf.Fields[f]
	rows := []q2spot.Row{info("Now: "+devShow(f, tf.Fields), tf.Name),
		{Kind: "search", Title: "Type a New " + f.Name(),
			Subtitle: "Up to 60 letters, digits or symbols; use your phone for more, accents or &"}}
	offered := map[string]bool{}
	offer := func(v, title, sub string) {
		if v != "" && !offered[v] && devClean(f, v) != cur {
			offered[v] = true
			rows = append(rows, d.action("set", string(f)+"|"+p+"|"+v, title, sub))
		}
	}
	if cur != "" {
		rows = append(rows, d.action("set", string(f)+"|"+p+"|", "Clear", "Leaves it empty"))
	}
	switch f {
	case tags.Title:
		if v := devTitleOf(tf.Name, tf.Fields[tags.Track]); v != "" {
			offer(v, "From the File Name: "+v, "")
		}
	case tags.AlbumArtist:
		if v := tf.Fields[tags.Artist]; v != "" {
			offer(v, "Same as Artist: "+v, "")
		}
	case tags.Track:
		if songs, err := a.files.songs(path.Dir(p)); err == nil {
			for i, s := range songs {
				if s == p {
					offer(strconv.Itoa(i+1), fmt.Sprintf("%d: Its Place in the Folder", i+1), "")
				}
			}
		}
	case tags.Disc:
		for n := 1; n <= 3; n++ {
			offer(strconv.Itoa(n), fmt.Sprintf("Disc %d", n), "")
		}
	}
	if f != tags.Title && f != tags.Track {
		for _, v := range a.devSeen(p, f, cur) {
			offer(v, v, "In this folder")
		}
	}
	if f == tags.Genre {
		for _, g := range devGenres {
			offer(g, g, "")
		}
	}
	return q2spot.Reply{Rows: rows}
}

// devTitleOf is a title from a song's file name: no extension, and no leading track number
// ("01 - Pendulum.flac": "Pendulum") unless the song says it has another one.
func devTitleOf(name, track string) string {
	v := strings.TrimSuffix(name, path.Ext(name))
	n := 0
	for n < len(v) && n < 3 && v[n] >= '0' && v[n] <= '9' {
		n++
	}
	if n > 0 && n < len(v) && strings.ContainsRune(" .-_", rune(v[n])) &&
		(track == "" || strings.TrimLeft(v[:n], "0") == track) {
		if rest := strings.TrimLeft(v[n:], " .-_"); rest != "" {
			v = rest
		}
	}
	if !strings.Contains(v, " ") {
		v = strings.ReplaceAll(v, "_", " ")
	}
	return strings.TrimSpace(v)
}

// devSeen is up to devMaxSeen values of f the other songs in p's folder have, cur aside: read for
// devSeenFor at most, so a big folder doesn't keep the Q2 waiting.
func (a *app) devSeen(p string, f tags.Field, cur string) []string {
	songs, err := a.files.songs(path.Dir(p))
	if err != nil {
		return nil
	}
	end := time.Now().Add(devSeenFor)
	seen := map[string]bool{cur: true, "": true}
	var out []string
	reads := 0
	for _, s := range songs {
		if len(out) >= devMaxSeen || reads >= devSeenReads || time.Now().After(end) {
			break
		}
		if s == p {
			continue
		}
		reads++
		if tf := devReadTags(a.files, s); tf.Info != nil && !seen[tf.Fields[f]] {
			seen[tf.Fields[f]] = true
			out = append(out, tf.Fields[f])
		}
	}
	return out
}

// devNumOf reads "3/12" or "3 of 12" as 3 and 12.
func devNumOf(v string) (n, of string, ok bool) {
	n, of, ok = strings.Cut(v, "/")
	if !ok {
		n, of, ok = strings.Cut(strings.ToLower(v), " of ")
	}
	n, of = strings.TrimSpace(n), strings.TrimSpace(of)
	return n, of, ok && n != "" && of != ""
}

// devChanges are what saving v to f does: "3/12" for Track or Disc sets its total too.
func devChanges(f tags.Field, v string) (tags.Changes, error) {
	set := map[tags.Field]string{f: v}
	if n, of, ok := devNumOf(v); ok && (f == tags.Track || f == tags.Disc) {
		total := tags.TrackTotal
		if f == tags.Disc {
			total = tags.DiscTotal
		}
		set[f], set[total] = n, of
	}
	c := tags.Changes{Set: set}
	if err := tags.Check(c); err != nil {
		return c, tagErr("", err)
	}
	return c, nil
}

// devClean is v as a song keeps it, to tell a change from none.
func devClean(f tags.Field, v string) string { return tags.Clean(f, v) }

// devHas is whether the song tf already holds c's values.
func devHas(tf tagFile, c tags.Changes) bool {
	if tf.Info == nil {
		return false
	}
	for f, v := range c.Set {
		if tf.Fields[f] != devClean(f, v) {
			return false
		}
	}
	return true
}

// devSearch is the level "search <text> [cursor]", the T9 box's text. Typed for a field (the edit
// target) it shows what saving it would change, writing nothing; typed anywhere else, or for a song
// gone since, it finds songs by name.
func (a *app) devSearch(req q2spot.Request) q2spot.Reply {
	d := a.devTags()
	text := req.Arg(0)
	if t := d.aimed(); t != nil && req.Arg(1) == "" {
		if _, _, err := a.files.existing(t.path); err == nil && cardReady() {
			return a.devPreview(t, text)
		}
		d.aim(nil)
	}
	return a.devFind(text, req.Arg(1))
}

// devPreview is what saving text to the target would change: Save, Save for All (a field an
// album's songs share), and Return to leave it.
func (a *app) devPreview(t *devTarget, text string) q2spot.Reply {
	d := a.devTags()
	tf := devReadTags(a.files, t.path)
	if tf.Info == nil {
		return q2spot.Fail(q2spot.Error, tf.Error)
	}
	c, err := devChanges(t.field, text)
	if err != nil {
		return q2spot.Reply{Rows: []q2spot.Row{info(err.Error(), "Return to type it again")}}
	}
	after := map[tags.Field]string{}
	for f, v := range tf.Fields {
		after[f] = v
	}
	for f, v := range c.Set {
		after[f] = devClean(f, v)
	}
	rows := []q2spot.Row{info(t.field.Name()+": "+devShow(t.field, tf.Fields)+" → "+devShow(t.field, after), tf.Name),
		d.action("set", string(t.field)+"|"+t.path+"|"+text, "Save", tf.Name)}
	if dir := path.Dir(t.path); devAlbumWide[t.field] {
		if songs, err := a.files.songs(dir); err == nil && len(songs) > 1 {
			rows = append(rows, d.action("setdir", string(t.field)+"|"+dir+"|"+text,
				fmt.Sprintf("Save for All %d Songs in This Folder", len(songs)), path.Base(dir)))
		}
	}
	return q2spot.Reply{Rows: append(rows, info("Return to leave it as it was", ""))}
}

// devHunt is a song search: the folders it is in, each with the names it has still to look at,
// and the songs found so far. It stops where its time runs out and goes on from there when the Q2
// asks for the next page.
type devHunt struct {
	q     string // lower case
	dirs  []devHuntDir
	found []string
	cut   bool // the card went away under it: a folder it came to wasn't looked in
}

type devHuntDir struct {
	path  string
	names []fs.DirEntry
}

// done is whether the search has looked everywhere, or found as many as it keeps; one the card's
// going cut short never is.
func (h *devHunt) done() bool { return !h.cut && (len(h.dirs) == 0 || len(h.found) >= maxFound) }

// devStep looks at a song search's next name: a folder is gone into, a song whose name holds the
// text is found. Dot files and folders are left out, as the browser leaves them. A folder that
// can't be read is passed over, unless the card has gone (USB storage): that cuts the search.
func (a *app) devStep(h *devHunt) {
	top := &h.dirs[len(h.dirs)-1]
	if len(top.names) == 0 {
		h.dirs = h.dirs[:len(h.dirs)-1]
		return
	}
	de := top.names[0]
	top.names = top.names[1:]
	name := de.Name()
	switch p := path.Join(top.path, name); {
	case hidden(name):
	case de.IsDir():
		des, err := devReadDir(filepath.Join(a.files.root, filepath.FromSlash(p)))
		switch {
		case err == nil:
			h.dirs = append(h.dirs, devHuntDir{p, des})
		case !cardReady():
			h.cut = true
		}
	case isSong(name) && strings.Contains(strings.ToLower(name), h.q):
		h.found = append(h.found, p)
	}
}

// devFind is the level "search <text> [cursor]" with no edit target: the songs whose names hold
// text, as song rows, a page at a time. Each answer comes within devFindFor (and one folder's
// read): it looks till then or until it has a page, then answers with what it has and "pages", so
// the Q2 asks again and the search goes on where it stopped; only once it has looked everywhere
// does it say nothing was found. A search the card's going cut short is forgotten, so the same one
// once the card is back (Retry, More…) looks again.
func (a *app) devFind(text, cursor string) q2spot.Reply {
	end := time.Now().Add(devFindFor) // from the request: waiting for a walk the Q2 left counts
	d := a.devTags()
	off, ok := devCursor(cursor)
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	q := strings.ToLower(strings.TrimSpace(text))
	if q == "" {
		return q2spot.Reply{Rows: []q2spot.Row{info("Search for what?", "Return to type part of a song's name")}}
	}
	if !cardReady() {
		return devNoCard()
	}
	turn := d.turn.Add(1)
	d.finding.Lock()
	defer d.finding.Unlock()
	d.mu.Lock()
	h := d.hunt
	d.mu.Unlock()
	if h == nil || h.q != q {
		des, err := devReadDir(a.files.root)
		switch {
		case err != nil && !cardReady():
			return devNoCard()
		case err != nil:
			return q2spot.Fail(q2spot.Error, "Couldn't read the card: "+err.Error())
		}
		h = &devHunt{q: q, dirs: []devHuntDir{{"/", des}}}
		d.mu.Lock()
		d.hunt = h
		d.mu.Unlock()
	}
	for !h.cut && !h.done() && len(h.found)-off < q2spot.MaxRows && d.turn.Load() == turn && !a.files.stopped() &&
		time.Now().Before(end) {
		a.devStep(h)
	}
	if h.cut || !cardReady() {
		d.mu.Lock()
		if d.hunt == h {
			d.hunt = nil
		}
		d.mu.Unlock()
		return devNoCard()
	}
	var rows []q2spot.Row
	for i := off; i < len(h.found) && len(rows) < q2spot.MaxRows; i++ {
		rows = append(rows, d.list("song", h.found[i], path.Base(h.found[i]), devWhere(h.found[i])))
	}
	last := h.done() && off+len(rows) >= len(h.found)
	page := rows
	switch {
	case !last:
	case len(h.found) == 0:
		page = append(page, info("No song's name holds “"+text+"”", "Return to type it again"))
	case len(h.found) >= maxFound:
		page = append(page, info(fmt.Sprintf("The first %d songs found", len(h.found)),
			"Type more of its name to find others"))
	}
	_, kept := q2spot.Reply{Message: "pages", Cursor: "4294967295", Rows: page}.Encode("4294967295")
	if last && kept == len(page) {
		return q2spot.Reply{Rows: page}
	}
	kept = min(kept, len(rows))
	return q2spot.Reply{Message: "pages", Cursor: strconv.Itoa(off + kept), Rows: rows[:kept]}
}

// devBatch is one save's songs: a Q2 action's (pressed, waiting their turn or being saved) or a
// tags POST's from the phone. From the press (or the POST) to its end it is a tag save as the files
// count them, so File Browser is busy (it doesn't turn itself off, the Q2 stays awake, and Update
// Library waits) between its songs too.
type devBatch struct {
	total, done int    // done: songs before the one under way
	name        string // the song under way, or first
	running     bool   // its turn has come
	writing     bool   // a song's tags being written just now, which the files count as well
	ended       bool   // its result is there for the toast
	left        bool   // the Q2 stopped waiting: its end is kept in late
	mark        *transfer
}

// devResult is how a batch's saves went.
type devResult struct {
	total, saved int
	first        error
}

// toast is r as the action's reply says it.
func (r devResult) toast() string {
	switch {
	case r.first == nil && r.total == 1:
		return "Saved"
	case r.first == nil:
		return "Saved " + plural(r.total, "song")
	case r.total == 1:
		return r.first.Error()
	}
	return fmt.Sprintf("Saved %d of %d · %v", r.saved, r.total, r.first)
}

// row is r as a level shows it once the Q2 has stopped waiting for it.
func (r devResult) row() q2spot.Row {
	switch {
	case r.first == nil:
		return info("Saved "+plural(r.total, "song"), "Finished in the background")
	case r.total == 1:
		return info("Not saved", r.first.Error())
	}
	return info(fmt.Sprintf("Saved %d of %d songs", r.saved, r.total), r.first.Error())
}

// devLate is how a save that went on in the background ended, and when.
type devLate struct {
	row q2spot.Row
	at  time.Time
}

// takeLate is how a save that went on in the background ended, as a row for a level to show
// once, and forgotten.
func (d *devState) takeLate() []q2spot.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.late == nil {
		return nil
	}
	row := d.late.row
	d.late = nil
	return []q2spot.Row{row}
}

// lateRow is how a save that went on in the background ended, for the menu while it is recent.
func (d *devState) lateRow() (q2spot.Row, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.late == nil || time.Since(d.late.at) > devLateFor {
		return q2spot.Row{}, false
	}
	return d.late.row, true
}

// pending is the saves pressed and not ended: the songs they have left (the one under way among
// them), the name of the one under way, and how many of the files' tag saves are theirs (a mark
// each, and a song being written).
func (d *devState) pending() (left int, name string, theirs int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, b := range d.batches {
		left += b.total - b.done
		if i == 0 || b.running {
			name = b.name
		}
		theirs++
		if b.writing {
			theirs++
		}
	}
	return left, name, theirs
}

// step is where the batch b is: at song i (those before it done), named name, its tags being
// written or not.
func (d *devState) step(b *devBatch, i int, name string, writing bool) {
	d.mu.Lock()
	b.running, b.done, b.name, b.writing = true, i, name, writing
	d.mu.Unlock()
}

// devBegin is the batch of paths pressed (or posted): busy from now till devEnd.
func (a *app) devBegin(paths []string) *devBatch {
	d := a.devTags()
	b := &devBatch{total: len(paths), name: path.Base(paths[0])}
	a.files.writers.Add(1)
	b.mark = a.files.beginTags(b.name)
	d.mu.Lock()
	d.batches = append(d.batches, b)
	d.mu.Unlock()
	return b
}

// devEnd ends the batch b, which went as r: kept to show when the Q2 stopped waiting for it.
func (a *app) devEnd(b *devBatch, r devResult) {
	d := a.devTags()
	d.mu.Lock()
	for i, o := range d.batches {
		if o == b {
			d.batches = append(d.batches[:i:i], d.batches[i+1:]...)
			break
		}
	}
	b.ended = true
	if b.left {
		d.late = &devLate{row: r.row(), at: time.Now()}
	}
	d.mu.Unlock()
	a.files.end(b.mark)
	a.files.writers.Done()
}

// devSave is the actions set ("<field>|<path>|<value>") and setdir ("<field>|<folder>|<value>":
// each song in the folder, not its folders'). A song that holds the value already is left as it is,
// so pressing again changes nothing. Past devWait the toast says the save goes on; how it ended
// shows on the next level asked for.
func (a *app) devSave(op, ctx string) q2spot.Reply {
	if a.settings().ReadOnly {
		return q2spot.Fail(q2spot.Refused, "Read Only is on")
	}
	f, rest, ok := devCut(ctx)
	p, v, ok2 := strings.Cut(rest, "|")
	if !ok || !ok2 {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	c, err := devChanges(f, v)
	if err != nil {
		return devFail(err)
	}
	paths := []string{p}
	if op == "setdir" {
		if !devAlbumWide[f] {
			return q2spot.Fail(q2spot.Bad, f.Name()+" isn't saved for a whole folder")
		}
		if paths, err = a.files.songs(p); err != nil {
			return devFail(err)
		}
		if len(paths) == 0 {
			return q2spot.Fail(q2spot.Unavailable, "No songs in that folder")
		}
	}
	if a.files.stopped() {
		return q2spot.Fail(q2spot.Unavailable, "File Browser is turning off")
	}
	d := a.devTags()
	b := a.devBegin(paths)
	done := make(chan devResult, 1)
	go func() {
		r := a.devWrite(paths, c, b)
		a.devEnd(b, r)
		done <- r
	}()
	select {
	case r := <-done:
		return q2spot.Reply{Message: r.toast()}
	case <-time.After(devWait):
	}
	d.mu.Lock()
	b.left = !b.ended
	ended := b.ended
	d.mu.Unlock()
	if ended {
		return q2spot.Reply{Message: (<-done).toast()}
	}
	return q2spot.Reply{Message: "Still saving in the background"}
}

// devWrite makes c's changes to each of paths in turn, after any action's before, and says how it
// went. It stops between songs when File Browser is turning off.
func (a *app) devWrite(paths []string, c tags.Changes, b *devBatch) devResult {
	d := a.devTags()
	d.saving.Lock()
	defer d.saving.Unlock()
	r := devResult{total: len(paths)}
	for i, p := range paths {
		if a.files.stopped() {
			if r.first == nil {
				r.first = errors.New("File Browser turned off; the rest are as they were")
			}
			break
		}
		d.step(b, i, path.Base(p), false)
		if devHas(devReadTags(a.files, p), c) {
			r.saved++
			continue
		}
		d.step(b, i, path.Base(p), true)
		err := a.files.writeTags(p, c)
		d.step(b, i, path.Base(p), false)
		if err != nil {
			if r.first == nil {
				r.first = err
			}
			continue
		}
		r.saved++
	}
	return r
}

// devSaving is whether tags are being saved: the saves pressed (or posted) and not ended, or a song
// being written.
func (a *app) devSaving() bool {
	left, _, _ := a.devTags().pending()
	n, _ := a.files.saving()
	return left > 0 || n > 0
}

// devLibrary is the action library: Update Local Music, which Q2 Pod runs once nothing plays.
func (a *app) devLibrary() q2spot.Reply {
	if a.settings().ReadOnly {
		return q2spot.Fail(q2spot.Refused, "Read Only is on")
	}
	later, err := a.askLibrary()
	switch {
	case err != nil:
		return devFail(err)
	case later:
		return q2spot.Reply{Message: "The library updates once the tags are saved and nothing plays"}
	}
	return q2spot.Reply{Message: "The library updates once nothing plays"}
}

// askLibrary asks the Q2 to update its library, or, while tags are being saved (the Q2's or the
// phone's), once they are, so the scan reads them as saved: later says so. The Q2's Update Library
// and the page's ask alike.
func (a *app) askLibrary() (later bool, err error) {
	if !libraryAvailable() || !a.devSaving() {
		return false, requestLibrary()
	}
	d := a.devTags()
	d.mu.Lock()
	waiting := d.library
	d.library = true
	if !waiting {
		d.asking.Add(1)
	}
	d.mu.Unlock()
	if !waiting {
		go a.devLibraryLater()
	}
	return true, nil
}

// devLibraryLater asks for Update Local Music once no tags are being saved (or File Browser is
// turning off, the rest not to be saved).
func (a *app) devLibraryLater() {
	d := a.devTags()
	defer d.asking.Done()
	for a.devSaving() && !a.files.stopped() {
		time.Sleep(devLibraryEvery)
	}
	d.mu.Lock()
	d.library = false
	d.mu.Unlock()
	if err := requestLibrary(); err != nil {
		log.Printf("library: %v", err)
	}
}
