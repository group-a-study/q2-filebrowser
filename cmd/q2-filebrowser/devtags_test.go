package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
	"github.com/group-a-study/q2-filebrowser/internal/tags"
)

// devRig is a rig whose album is two small FLACs with tags, nothing playing, and a firmware that
// takes no library requests until a test writes the caps.
func devRig(t *testing.T) *rig {
	r := newRig(t)
	writeFLAC(t, filepath.Join(r.card, "Music/Broadcast/01 Pendulum.flac"), "TITLE=Pendulum", "ARTIST=Broadcast",
		"ALBUM=Tender Buttons", "TRACKNUMBER=1", "GENRE=Electronic")
	writeFLAC(t, filepath.Join(r.card, "Music/Broadcast/02 Valerie.flac"), "TITLE=Valerie", "ARTIST=Broadcast",
		"ALBUM=Tender Buttons", "TRACKNUMBER=2", "GENRE=Indie")
	proc, caps, lib, wait := devProcRoot, capsFile, libraryFile, devWait
	devProcRoot = t.TempDir()
	capsFile, libraryFile = filepath.Join(r.tmp, "caps"), filepath.Join(r.tmp, "library")
	t.Cleanup(func() { devProcRoot, capsFile, libraryFile, devWait = proc, caps, lib, wait })
	// The Q2's socket comes a moment after start returns: a test shorter than that would leave it
	// to be made in the temp folder as that is removed.
	waitFor(t, "the Q2's socket", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(r.tmp, "browse.sock"))
		return err == nil
	})
	return r
}

const devPendulum = "/Music/Broadcast/01 Pendulum.flac"

// devAsk is the app's reply to op, checked as the Q2 takes one: within 64 KB and 50 rows, every op
// a row names one it can send (15 bytes, never "stop" but Turn Off's), every context one it can send
// back (398 bytes escaped, with room for a cursor after it), and no rows for an action.
func devAsk(t *testing.T, a *app, op string, args ...string) q2spot.Reply {
	t.Helper()
	rep := a.device(q2spot.Request{ID: "7", Op: op, Args: args})
	b, kept := rep.Encode("4294967295")
	if len(b) > q2spot.MaxReply || kept != len(rep.Rows) || len(rep.Rows) > q2spot.MaxRows {
		t.Fatalf("%s %q: %d bytes, %d of %d rows", op, args, len(b), kept, len(rep.Rows))
	}
	switch op {
	case "set", "setid", "setdir", "library", "readonly":
		if len(rep.Rows) > 0 {
			t.Fatalf("action %s has rows %q", op, rep.Rows)
		}
	}
	for _, row := range rep.Rows {
		if n := len(q2spot.Escape(row.Context)); n+len("\t9999999") > 398 {
			t.Fatalf("%s: a context of %d bytes escaped: %q", op, n, row)
		}
		if row.Kind != "list" && row.Kind != "action" {
			continue
		}
		if row.URI == "" || len(row.URI) > 15 {
			t.Fatalf("%s: op %q", op, row.URI)
		}
		if row.URI == "stop" && op != "menu" {
			t.Fatalf("%s: a row named stop %q", op, row)
		}
	}
	return rep
}

// devFind is the row titled title.
func devFind(t *testing.T, rep q2spot.Reply, title string) q2spot.Row {
	t.Helper()
	for _, row := range rep.Rows {
		if row.Title == title {
			return row
		}
	}
	t.Fatalf("no row %q in %q", title, rep.Rows)
	return q2spot.Row{}
}

func devTitles(rep q2spot.Reply) []string {
	var out []string
	for _, row := range rep.Rows {
		out = append(out, row.Title)
	}
	return out
}

// devPress is what pressing row does: its level, or its action.
func devPress(t *testing.T, a *app, row q2spot.Row) q2spot.Reply {
	t.Helper()
	if row.Kind != "list" && row.Kind != "action" {
		t.Fatalf("not a press: %q", row)
	}
	return devAsk(t, a, row.URI, row.Context)
}

func devRead(t *testing.T, r *rig, p string) map[tags.Field]string {
	t.Helper()
	info, err := tags.Read(filepath.Join(r.card, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return info.Fields
}

func devBytes(t *testing.T, r *rig, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.card, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The menu's Edit Tags opens the card's root; a song's tags being saved show where moves do.
func TestDevTagsMenu(t *testing.T) {
	r := devRig(t)
	rep := devAsk(t, r.a, "menu")
	if row := rep.Rows[1]; row.Kind != "list" || row.URI != "tags" || row.Context != "/" || row.Title != "Edit Tags" ||
		rep.Rows[0].Title != "Open on Your Phone" {
		t.Fatalf("menu %q", rep.Rows)
	}
	tr := r.a.files.beginTags("01 Pendulum.flac")
	if row := devAsk(t, r.a, "menu").Rows[2]; row.Kind != "info" || row.Title != "Saving tags · 01 Pendulum.flac" ||
		row.Subtitle != "1 song" {
		t.Fatalf("saving %q", row)
	}
	up := r.a.files.begin(true, "x.flac", 0)
	if row := devAsk(t, r.a, "menu").Rows[2]; row.Title != "x.flac" || row.Subtitle != "receiving 1 file · saving tags" {
		t.Fatalf("both %q", row)
	}
	r.a.files.end(up)
	r.a.files.end(tr)
	if row := devAsk(t, r.a, "menu").Rows[2]; !strings.HasPrefix(row.Title, "Card: ") {
		t.Fatalf("after %q", row)
	}
	// No op is named stop but Turn Off's.
	stops := 0
	for _, row := range devAsk(t, r.a, "caps").Rows {
		if row.URI == "stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("caps name stop %d times", stops)
	}
}

// Folders, then songs, by name, names alone; at the root Find a Song first, and Update Library
// when the firmware takes it.
func TestDevTagsFolders(t *testing.T) {
	r := devRig(t)
	rep := devAsk(t, r.a, "tags", "/")
	want := []string{"Find a Song", "Music"}
	if isSong("song.mp3") { // once MP3s are read
		want = append(want, "song.mp3")
	}
	if got := devTitles(rep); strings.Join(got, ",") != strings.Join(want, ",") || rep.Cursor != "" ||
		rep.Rows[0].Kind != "search" || rep.Rows[1].URI != "tags" || rep.Rows[1].Context != "/Music" {
		t.Fatalf("root %q", rep.Rows)
	}
	os.WriteFile(capsFile, []byte("progress pages library goto\n"), 0o644)
	rep = devAsk(t, r.a, "tags", "/")
	if row := rep.Rows[1]; row.Kind != "action" || row.URI != "library" || row.Title != "Update Library" ||
		row.Subtitle != "Reads every song's tags again; waits until nothing plays" {
		t.Fatalf("library row %q", rep.Rows)
	}
	rep = devAsk(t, r.a, "tags", "/Music/Broadcast/")
	if got := devTitles(rep); strings.Join(got, ",") != "01 Pendulum.flac,02 Valerie.flac" ||
		rep.Rows[0].URI != "song" || rep.Rows[0].Context != devPendulum {
		t.Fatalf("album %q", rep.Rows)
	}
	os.Mkdir(filepath.Join(r.card, "Empty"), 0o755)
	if rep = devAsk(t, r.a, "tags", "/Empty"); len(rep.Rows) != 1 || rep.Rows[0].Kind != "info" {
		t.Fatalf("empty %q", rep.Rows)
	}
	if rep = devAsk(t, r.a, "tags", "/Nowhere"); rep.Status != q2spot.Unavailable {
		t.Fatalf("missing %+v", rep)
	}
	cardReady = func() bool { return false }
	rep = devAsk(t, r.a, "tags", "/")
	cardReady = func() bool { return true }
	if rep.Status != q2spot.Unavailable || !strings.Contains(rep.Message, "USB") {
		t.Fatalf("no card %+v", rep)
	}
	// Update Library asks the Q2, which isn't a change Read Only allows.
	if rep = devAsk(t, r.a, "library"); rep.Message != "The library updates once nothing plays" {
		t.Fatalf("library %+v", rep)
	}
	if b, _ := os.ReadFile(libraryFile); !strings.HasSuffix(string(b), "\nscan\n") {
		t.Fatalf("request %q", b)
	}
	r.a.update(func(s *settings) { s.ReadOnly = true })
	os.Remove(libraryFile)
	if rep = devAsk(t, r.a, "library"); rep.Message != "Read Only is on" {
		t.Fatalf("read only library %+v", rep)
	}
	if _, err := os.Stat(libraryFile); err == nil {
		t.Fatal("asked in Read Only")
	}
	r.a.update(func(s *settings) { s.ReadOnly = false })
	os.Remove(capsFile)
	if rep = devAsk(t, r.a, "library"); !strings.Contains(rep.Message, "Update Local Music") {
		t.Fatalf("old firmware %+v", rep)
	}
}

// A big folder comes 50 rows a reply, each with the offset to go on from, folders first.
func TestDevTagsPages(t *testing.T) {
	r := devRig(t)
	var want []string
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("Disc %d", i)
		os.MkdirAll(filepath.Join(r.card, "Big", name), 0o755)
		want = append(want, name)
	}
	var songs []string
	for i := 0; i < 130; i++ {
		name := fmt.Sprintf("Song %03d.flac", i)
		write(t, filepath.Join(r.card, "Big", name), "")
		songs = append(songs, name)
	}
	write(t, filepath.Join(r.card, "Big", "cover.jpg"), "")
	sort.Strings(songs)
	want = append(want, songs...)
	var got []string
	cursor, replies := "", 0
	for {
		args := []string{"/Big"}
		if cursor != "" {
			args = append(args, cursor)
		}
		rep := devAsk(t, r.a, "tags", args...)
		replies++
		if len(rep.Rows) > q2spot.MaxRows || (rep.Cursor != "" && rep.Message != "pages") {
			t.Fatalf("page %d: %d rows, %q %q", replies, len(rep.Rows), rep.Message, rep.Cursor)
		}
		got = append(got, devTitles(rep)...)
		if cursor = rep.Cursor; cursor == "" {
			break
		}
	}
	if replies != 3 || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%d replies: %q", replies, got)
	}
	if rep := devAsk(t, r.a, "tags", "/Big", "x"); rep.Status != q2spot.Bad {
		t.Fatalf("bad cursor %+v", rep)
	}
}

// A song: its name and format, its fields' values, Play when stock plays it from the card.
func TestDevTagsSong(t *testing.T) {
	r := devRig(t)
	before := devBytes(t, r, devPendulum)
	rep := devAsk(t, r.a, "song", devPendulum)
	if row := rep.Rows[0]; row.Kind != "info" || row.Title != "01 Pendulum.flac" || row.Subtitle != "FLAC" {
		t.Fatalf("head %q", rep.Rows)
	}
	for title, sub := range map[string]string{"Title": "Pendulum", "Artist": "Broadcast", "Album": "Tender Buttons",
		"Album Artist": "Empty", "Track": "1", "Disc": "Empty", "Year": "Empty", "Genre": "Electronic", "Composer": "Empty"} {
		if row := devFind(t, rep, title); row.Kind != "list" || row.URI != "field" || row.Subtitle != sub {
			t.Fatalf("%s %q", title, row)
		}
	}
	if row := devFind(t, rep, "Album Artist"); row.Context != "albumartist|"+devPendulum {
		t.Fatalf("context %q", row)
	}
	full := filepath.Join(r.card, "Music", "Broadcast", "01 Pendulum.flac")
	if row := devFind(t, rep, "Play"); row.Kind != "local" || row.URI != full || row.Context != "album" {
		t.Fatalf("play %q", row)
	}
	if !bytes.Equal(before, devBytes(t, r, devPendulum)) {
		t.Fatal("looking wrote")
	}
	writeFLAC(t, filepath.Join(r.card, "Odd/a.fLaC"), "TITLE=A", "TRACKNUMBER=3", "TRACKTOTAL=12")
	writeFLAC(t, filepath.Join(r.card, "Odd/b.FLAC"), "TITLE=B")
	rep = devAsk(t, r.a, "song", "/Odd/a.fLaC")
	if devFind(t, rep, "Track").Subtitle != "3 of 12" {
		t.Fatalf("track %q", rep.Rows)
	}
	for _, row := range rep.Rows {
		if row.Kind == "local" {
			t.Fatalf("stock doesn't play .fLaC: %q", row)
		}
	}
	devFind(t, devAsk(t, r.a, "song", "/Odd/b.FLAC"), "Play")
	if rep = devAsk(t, r.a, "song", "/Music/gone.flac"); rep.Status != q2spot.Unavailable {
		t.Fatalf("missing %+v", rep)
	}
}

// A field: the value now, the T9 box (the field becomes the edit target), values to pick.
func TestDevTagsField(t *testing.T) {
	r := devRig(t)
	d := r.a.devTags()
	devAsk(t, r.a, "set", "title|"+devPendulum+"|Pendulum Live")
	rep := devPress(t, r.a, devFind(t, devAsk(t, r.a, "song", devPendulum), "Title"))
	if got := devTitles(rep); strings.Join(got, ",") != "Now: Pendulum Live,Type a New Title,Clear,From the File Name: Pendulum" {
		t.Fatalf("title %q", got)
	}
	if row := rep.Rows[1]; row.Kind != "search" {
		t.Fatalf("search %q", row)
	}
	if row := rep.Rows[3]; row.Kind != "action" || row.URI != "set" || row.Context != "title|"+devPendulum+"|Pendulum" {
		t.Fatalf("from name %q", row)
	}
	if row := rep.Rows[2]; row.Context != "title|"+devPendulum+"|" {
		t.Fatalf("clear %q", row)
	}
	if tg := d.aimed(); tg == nil || tg.path != devPendulum || tg.field != tags.Title {
		t.Fatalf("target %+v", tg)
	}
	// Any other list clears it; the field sets it again.
	devAsk(t, r.a, "song", devPendulum)
	if d.aimed() != nil {
		t.Fatal("song kept the target")
	}
	rep = devAsk(t, r.a, "field", "albumartist|"+devPendulum)
	if got := devTitles(rep); strings.Join(got, ",") != "Now: Empty,Type a New Album Artist,Same as Artist: Broadcast" {
		t.Fatalf("album artist %q", got)
	}
	devAsk(t, r.a, "menu")
	if d.aimed() != nil {
		t.Fatal("menu kept the target")
	}
	devAsk(t, r.a, "set", "track|/Music/Broadcast/02 Valerie.flac|7")
	rep = devAsk(t, r.a, "field", "track|/Music/Broadcast/02 Valerie.flac")
	devFind(t, rep, "2: Its Place in the Folder")
	rep = devAsk(t, r.a, "field", "disc|"+devPendulum)
	if got := devTitles(rep); strings.Join(got[2:], ",") != "Disc 1,Disc 2,Disc 3" {
		t.Fatalf("disc %q", got)
	}
	rep = devAsk(t, r.a, "field", "genre|"+devPendulum)
	if row := devFind(t, rep, "Indie"); row.Subtitle != "In this folder" || row.Context != "genre|"+devPendulum+"|Indie" {
		t.Fatalf("folder's %q", row)
	}
	devFind(t, rep, "R&B")
	seen := map[string]bool{}
	for _, title := range devTitles(rep) {
		if seen[title] {
			t.Fatalf("twice %q", title)
		}
		seen[title] = true
	}
	if rep = devAsk(t, r.a, "field", "nope|"+devPendulum); rep.Status != q2spot.Bad {
		t.Fatalf("no field %+v", rep)
	}
}

// Typed for a field, a value is shown with what it would change, nothing written; typed anywhere
// else, it finds songs by name.
func TestDevTagsSearch(t *testing.T) {
	r := devRig(t)
	before := devBytes(t, r, devPendulum)
	devAsk(t, r.a, "field", "album|"+devPendulum)
	for i := 0; i < 2; i++ { // asked again (after an action, say), it is the same
		rep := devAsk(t, r.a, "search", "Tender Buttons Deluxe")
		if got := devTitles(rep); strings.Join(got, ",") != "Album: Tender Buttons → Tender Buttons Deluxe,Save,"+
			"Save for All 2 Songs in This Folder,Return to leave it as it was" {
			t.Fatalf("preview %q", got)
		}
		if row := rep.Rows[1]; row.URI != "set" || row.Context != "album|"+devPendulum+"|Tender Buttons Deluxe" {
			t.Fatalf("save %q", row)
		}
		if row := rep.Rows[2]; row.URI != "setdir" || row.Context != "album|/Music/Broadcast|Tender Buttons Deluxe" {
			t.Fatalf("save all %q", row)
		}
	}
	if !bytes.Equal(before, devBytes(t, r, devPendulum)) {
		t.Fatal("a preview wrote")
	}
	devAsk(t, r.a, "field", "title|"+devPendulum)
	if got := devTitles(devAsk(t, r.a, "search", "Pendulum Two")); strings.Join(got, ",") !=
		"Title: Pendulum → Pendulum Two,Save,Return to leave it as it was" {
		t.Fatalf("title %q", got)
	}
	devAsk(t, r.a, "field", "track|"+devPendulum)
	if rep := devAsk(t, r.a, "search", "x"); len(rep.Rows) != 1 || rep.Rows[0].Title != "Track must be a number" {
		t.Fatalf("not a number %q", rep.Rows)
	}
	if got := devTitles(devAsk(t, r.a, "search", "3/12")); got[0] != "Track: 1 → 3 of 12" {
		t.Fatalf("of %q", got)
	}
	// No target: a song search.
	devAsk(t, r.a, "tags", "/")
	rep := devAsk(t, r.a, "search", "VALERIE")
	if len(rep.Rows) != 1 || rep.Rows[0].URI != "song" || rep.Rows[0].Context != "/Music/Broadcast/02 Valerie.flac" ||
		rep.Rows[0].Subtitle != "/Music/Broadcast" {
		t.Fatalf("find %q", rep.Rows)
	}
	if rep = devAsk(t, r.a, "search", "zzz"); len(rep.Rows) != 1 || rep.Rows[0].Kind != "info" {
		t.Fatalf("none %q", rep.Rows)
	}
	// The target's song gone: a song search too.
	writeFLAC(t, filepath.Join(r.card, "Gone/x.flac"), "TITLE=X")
	devAsk(t, r.a, "field", "title|/Gone/x.flac")
	os.Remove(filepath.Join(r.card, "Gone/x.flac"))
	if rep = devAsk(t, r.a, "search", "pendulum"); len(rep.Rows) != 1 || rep.Rows[0].Context != devPendulum {
		t.Fatalf("stale %q", rep.Rows)
	}
	// Many found: paged, from what was found.
	for i := 0; i < 70; i++ {
		write(t, filepath.Join(r.card, "Many", fmt.Sprintf("Hit %02d.flac", i)), "")
	}
	rep = devAsk(t, r.a, "search", "hit")
	if len(rep.Rows) != 50 || rep.Message != "pages" || rep.Cursor != "50" {
		t.Fatalf("first %d %q %q", len(rep.Rows), rep.Message, rep.Cursor)
	}
	if rep = devAsk(t, r.a, "search", "hit", "50"); len(rep.Rows) != 20 || rep.Cursor != "" || rep.Rows[19].Title != "Hit 69.flac" {
		t.Fatalf("next %d %q", len(rep.Rows), rep.Cursor)
	}
}

// Save, Save for All and the picked values write, once: again they change nothing; Read Only
// refuses them.
func TestDevTagsSave(t *testing.T) {
	r := devRig(t)
	const valerie = "/Music/Broadcast/02 Valerie.flac"
	devAsk(t, r.a, "field", "album|"+devPendulum)
	preview := devAsk(t, r.a, "search", "Deluxe")
	if rep := devPress(t, r.a, devFind(t, preview, "Save")); rep.Message != "Saved" {
		t.Fatalf("save %+v", rep)
	}
	if got := devRead(t, r, devPendulum); got[tags.Album] != "Deluxe" || got[tags.Title] != "Pendulum" {
		t.Fatalf("saved %v", got)
	}
	once := devBytes(t, r, devPendulum)
	if rep := devPress(t, r.a, devFind(t, preview, "Save")); rep.Message != "Saved" || !bytes.Equal(once, devBytes(t, r, devPendulum)) {
		t.Fatalf("again %+v", rep)
	}
	if r.a.devTags().aimed() == nil {
		t.Fatal("an action cleared the target")
	}
	if rep := devPress(t, r.a, devFind(t, preview, "Save for All 2 Songs in This Folder")); rep.Message != "Saved 2 songs" {
		t.Fatalf("save all %+v", rep)
	}
	if got := devRead(t, r, valerie); got[tags.Album] != "Deluxe" || got[tags.Title] != "Valerie" {
		t.Fatalf("all %v", got)
	}
	both := devBytes(t, r, valerie)
	devPress(t, r.a, devFind(t, preview, "Save for All 2 Songs in This Folder"))
	if !bytes.Equal(both, devBytes(t, r, valerie)) {
		t.Fatal("save all again wrote")
	}
	// Picked values: Clear, the file's name.
	rep := devAsk(t, r.a, "field", "title|"+devPendulum)
	devPress(t, r.a, devFind(t, rep, "Clear"))
	if got := devRead(t, r, devPendulum); got[tags.Title] != "" {
		t.Fatalf("cleared %v", got)
	}
	rep = devAsk(t, r.a, "field", "title|"+devPendulum)
	if got := devTitles(rep); strings.Join(got, ",") != "Now: Empty,Type a New Title,From the File Name: Pendulum" {
		t.Fatalf("after clear %q", got)
	}
	devPress(t, r.a, devFind(t, rep, "From the File Name: Pendulum"))
	if got := devRead(t, r, devPendulum); got[tags.Title] != "Pendulum" {
		t.Fatalf("from name %v", got)
	}
	if rep := devAsk(t, r.a, "set", "track|"+devPendulum+"|3/12"); rep.Message != "Saved" {
		t.Fatalf("of %+v", rep)
	}
	if got := devRead(t, r, devPendulum); got[tags.Track] != "3" || got[tags.TrackTotal] != "12" {
		t.Fatalf("of %v", got)
	}
	// Refused.
	if rep := devAsk(t, r.a, "set", "track|"+devPendulum+"|x"); rep.Message != "Track must be a number" {
		t.Fatalf("not a number %+v", rep)
	}
	if rep := devAsk(t, r.a, "setdir", "title|/Music/Broadcast|Same"); rep.Status != q2spot.Bad {
		t.Fatalf("title for all %+v", rep)
	}
	if rep := devAsk(t, r.a, "setid", "9999"); rep.Status != q2spot.Unavailable {
		t.Fatalf("stale id %+v", rep)
	}
	if rep := devAsk(t, r.a, "set", "title|/Music/gone.flac|X"); !strings.Contains(rep.Message, "isn't on the card") {
		t.Fatalf("gone %+v", rep)
	}
	r.a.update(func(s *settings) { s.ReadOnly = true })
	now := devBytes(t, r, devPendulum)
	for _, op := range []string{"set", "setdir"} {
		if rep := devPress(t, r.a, q2spot.Row{Kind: "action", URI: op, Context: "album|/Music/Broadcast|Other"}); rep.Message != "Read Only is on" {
			t.Fatalf("read only %s %+v", op, rep)
		}
	}
	if rep := devAsk(t, r.a, "set", "album|"+devPendulum+"|Other"); rep.Message != "Read Only is on" ||
		!bytes.Equal(now, devBytes(t, r, devPendulum)) {
		t.Fatalf("read only wrote %+v", rep)
	}
	if r.a.files.busy() {
		t.Fatal("a save left as moving")
	}
}

// A path too long for a row's context goes as a number the app keeps: pressed, it works the same.
func TestDevTagsLong(t *testing.T) {
	r := devRig(t)
	a, b := strings.Repeat("a", 150), strings.Repeat("b", 150)
	p := "/" + a + "/" + b + "/" + strings.Repeat("c", 100) + ".flac"
	writeFLAC(t, filepath.Join(r.card, filepath.FromSlash(p)), "TITLE=Long", "ALBUM=L")
	writeFLAC(t, filepath.Join(r.card, "/"+a+"/"+b+"/d.flac"), "TITLE=D", "ALBUM=L")
	pct := strings.Repeat("%", 130) // short, but three times as long escaped
	write(t, filepath.Join(r.card, pct, "x.flac"), "")
	rep := devAsk(t, r.a, "tags", "/"+a)
	rep = devPress(t, r.a, devFind(t, rep, b))
	row := devFind(t, rep, path.Base(p))
	if !strings.HasPrefix(row.Context, "@") {
		t.Fatalf("song %q", row)
	}
	rep = devPress(t, r.a, row)
	if rep.Rows[0].Title != path.Base(p) {
		t.Fatalf("song level %q", rep.Rows)
	}
	rep = devPress(t, r.a, devFind(t, rep, "Title"))
	if tg := r.a.devTags().aimed(); tg == nil || tg.path != p {
		t.Fatalf("target %+v", tg)
	}
	rep = devAsk(t, r.a, "search", "Longer")
	save := devFind(t, rep, "Save")
	if save.URI != "setid" {
		t.Fatalf("save %q", save)
	}
	if rep := devPress(t, r.a, save); rep.Message != "Saved" {
		t.Fatalf("saved %+v", rep)
	}
	if got := devRead(t, r, p); got[tags.Title] != "Longer" {
		t.Fatalf("title %v", got)
	}
	devAsk(t, r.a, "field", "album|"+p)
	all := devFind(t, devAsk(t, r.a, "search", "Long Album"), "Save for All 2 Songs in This Folder")
	if rep := devPress(t, r.a, all); rep.Message != "Saved 2 songs" {
		t.Fatalf("save all %q %+v", all, rep)
	}
	if got := devRead(t, r, "/"+a+"/"+b+"/d.flac"); got[tags.Album] != "Long Album" {
		t.Fatalf("album %v", got)
	}
	rep = devAsk(t, r.a, "tags", "/")
	if row := devFind(t, rep, pct); !strings.HasPrefix(row.Context, "@") {
		t.Fatalf("escaped %q", row)
	} else if got := devTitles(devPress(t, r.a, row)); strings.Join(got, ",") != "x.flac" {
		t.Fatalf("%% folder %q", got)
	}
	// Over the socket, as the Q2 asks.
	head, rows := r.ask("tags", "/")
	var ctx string
	for _, row := range rows {
		if row[2] == pct {
			ctx = row[4]
		}
	}
	if head[1] != "ok" || ctx == "" {
		t.Fatalf("socket %q %q", head, rows)
	}
	if head, rows = r.ask("tags", ctx); head[1] != "ok" || len(rows) != 1 || rows[0][2] != "x.flac" {
		t.Fatalf("socket %q %q", head, rows)
	}
	// A long value too.
	long := strings.Repeat("v", 390)
	devAsk(t, r.a, "field", "comment|"+devPendulum)
	if save := devFind(t, devAsk(t, r.a, "search", long), "Save"); save.URI != "setid" {
		t.Fatalf("long value %q", save)
	} else if devPress(t, r.a, save); devRead(t, r, devPendulum)[tags.Comment] != long {
		t.Fatal("long value not saved")
	}
	if rep := devAsk(t, r.a, "song", "@99999"); rep.Status != q2spot.Unavailable {
		t.Fatalf("stale %+v", rep)
	}
}

// devHold makes reading the tags of a song held matches wait, as a slow card would there, until
// open is closed; each song held is sent on at first.
func devHold(t *testing.T, held func(p string) bool) (at chan string, open chan struct{}) {
	at, open = make(chan string, 64), make(chan struct{})
	read := devReadTags
	devReadTags = func(f *files, p string) tagFile {
		if held(p) {
			at <- p
			<-open
		}
		return read(f, p)
	}
	t.Cleanup(func() { devReadTags = read })
	return at, open
}

// devBox is a folder of n songs, "01 song.flac" on, with album Old and genre Indie.
func devBox(t *testing.T, r *rig, n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("/Music/Box/%02d song.flac", i)
		writeFLAC(t, filepath.Join(r.card, filepath.FromSlash(p)), "TITLE=Song", "ALBUM=Old", "GENRE=Indie")
		out = append(out, p)
	}
	return out
}

// devAsked waits for an Update Library that waited for the saves to ask the Q2.
func devAsked(t *testing.T, a *app) {
	t.Helper()
	waitFor(t, "the library request", 10*time.Second, func() bool { _, err := os.Stat(libraryFile); return err == nil })
	a.devTags().asking.Wait() // done with it
}

// devSettled waits for the Q2's saves pressed to end.
func devSettled(t *testing.T, a *app) {
	t.Helper()
	waitFor(t, "the saves to end", 10*time.Second, func() bool { left, _, _ := a.devTags().pending(); return left == 0 })
}

// A save past devWait goes on; the Q2 is told so, and how it ended shows once on the next level
// (and on the menu meanwhile).
func TestDevTagsWait(t *testing.T) {
	r := devRig(t)
	devWait = 20 * time.Millisecond
	at, open := devHold(t, func(p string) bool { return p == devPendulum })
	if rep := devAsk(t, r.a, "set", "album|"+devPendulum+"|Deluxe"); rep.Message != "Still saving in the background" {
		t.Fatalf("slow %+v", rep)
	}
	<-at
	close(open)
	devSettled(t, r.a)
	if got := devRead(t, r, devPendulum)[tags.Album]; got != "Deluxe" {
		t.Fatalf("album %q", got)
	}
	if row := devAsk(t, r.a, "menu").Rows[2]; row.Title != "Saved 1 song" || row.Subtitle != "Finished in the background" {
		t.Fatalf("menu %q", row)
	}
	rep := devAsk(t, r.a, "song", devPendulum)
	if row := rep.Rows[0]; row.Kind != "info" || row.Title != "Saved 1 song" || rep.Rows[1].Title != "01 Pendulum.flac" {
		t.Fatalf("song %q", rep.Rows)
	}
	if rep = devAsk(t, r.a, "song", devPendulum); rep.Rows[0].Title != "01 Pendulum.flac" {
		t.Fatalf("shown twice %q", rep.Rows)
	}
	if row := devAsk(t, r.a, "menu").Rows[2]; !strings.HasPrefix(row.Title, "Card: ") {
		t.Fatalf("menu after %q", row)
	}
	// A failure: the folder's broken song named, on the root's first row.
	write(t, filepath.Join(r.card, "Music/Broadcast/03 Broken.flac"), "not a flac")
	at, open = devHold(t, func(p string) bool { return p == devPendulum })
	if rep := devAsk(t, r.a, "setdir", "album|/Music/Broadcast|Other"); rep.Message != "Still saving in the background" {
		t.Fatalf("slow all %+v", rep)
	}
	<-at
	close(open)
	devSettled(t, r.a)
	rep = devAsk(t, r.a, "tags", "/")
	if row := rep.Rows[0]; row.Title != "Saved 2 of 3 songs" || !strings.Contains(row.Subtitle, "03 Broken.flac") {
		t.Fatalf("root %q", rep.Rows)
	}
	if rep = devAsk(t, r.a, "tags", "/"); rep.Rows[0].Title != "Find a Song" {
		t.Fatalf("root again %q", devTitles(rep))
	}
	// In time, the toast says it all, and nothing is kept.
	devWait = 5 * time.Second
	if rep := devAsk(t, r.a, "set", "album|"+devPendulum+"|Quick"); rep.Message != "Saved" {
		t.Fatalf("quick %+v", rep)
	}
	if rep := devAsk(t, r.a, "song", devPendulum); rep.Rows[0].Title != "01 Pendulum.flac" {
		t.Fatalf("kept %q", rep.Rows)
	}
}

// A field level the Q2 left (Back) while it was still reading its song, answered after the levels
// asked for since, doesn't make that song the edit target: what is typed next is for the level on
// screen.
func TestDevTagsFieldLeft(t *testing.T) {
	r := devRig(t)
	d := r.a.devTags()
	const slow = "/Music/Race/slow.flac"
	writeFLAC(t, filepath.Join(r.card, "Music/Race/slow.flac"), "TITLE=Slow One", "ARTIST=Stereolab")
	at, open := devHold(t, func(p string) bool { return p == slow })
	done := make(chan q2spot.Reply, 1)
	go func() { done <- r.a.device(q2spot.Request{ID: "1", Op: "field", Args: []string{"title|" + slow}}) }()
	<-at
	devAsk(t, r.a, "tags", "/Music/Race")
	devAsk(t, r.a, "tags", "/")
	close(open)
	<-done
	if tg := d.aimed(); tg != nil {
		t.Fatalf("a left level aimed at %+v", tg)
	}
	if rep := devAsk(t, r.a, "search", "Pendulum"); len(rep.Rows) != 1 || rep.Rows[0].URI != "song" ||
		rep.Rows[0].Context != devPendulum {
		t.Fatalf("find %q", rep.Rows)
	}
	// Left for another song's field: the field on screen stays the target.
	at, open = devHold(t, func(p string) bool { return p == slow })
	go func() { done <- r.a.device(q2spot.Request{ID: "2", Op: "field", Args: []string{"artist|" + slow}}) }()
	<-at
	devAsk(t, r.a, "tags", "/")
	devAsk(t, r.a, "field", "artist|"+devPendulum)
	close(open)
	<-done
	if tg := d.aimed(); tg == nil || tg.path != devPendulum || tg.field != tags.Artist {
		t.Fatalf("target %+v", tg)
	}
	if row := devFind(t, devAsk(t, r.a, "search", "Stereolab"), "Save"); row.Context != "artist|"+devPendulum+"|Stereolab" {
		t.Fatalf("save %q", row)
	}
}

// Find a Song counts songs alone: folders whose names hold the text don't crowd out the song.
func TestDevTagsFindSongs(t *testing.T) {
	r := devRig(t)
	for i := 0; i < 160; i++ {
		for _, disc := range []string{"Disc 1", "Disc 2"} {
			os.MkdirAll(filepath.Join(r.card, fmt.Sprintf("Artist %03d/Album/%s", i, disc)), 0o755)
		}
	}
	write(t, filepath.Join(r.card, "The Trammps/Disco Inferno.flac"), "")
	var got []string
	for cursor, n := "", 0; n < 20; n++ {
		args := []string{"disc"}
		if cursor != "" {
			args = append(args, cursor)
		}
		rep := devAsk(t, r.a, "search", args...)
		got = append(got, devTitles(rep)...)
		if cursor = rep.Cursor; cursor == "" {
			break
		}
	}
	if strings.Join(got, ",") != "Disco Inferno.flac" {
		t.Fatalf("found %q", got)
	}
}

// On a slow card Find a Song answers within its time with what it has found, and goes on where it
// stopped as the Q2 asks for the next pages; only once it has looked everywhere does it say no
// song's name holds the text. A search asked for again (Back) starts from what was found; one asked
// for while another walks has the card at once.
func TestDevTagsFindSlow(t *testing.T) {
	if devFindFor > 2500*time.Millisecond {
		t.Fatalf("Find a Song looks %v before an answer", devFindFor)
	}
	r := devRig(t)
	for i := 0; i < 40; i++ {
		write(t, filepath.Join(r.card, fmt.Sprintf("Lib/Artist %02d/Album/01 Song.flac", i)), "")
	}
	write(t, filepath.Join(r.card, "Lib/Artist 39/Album/zzz Top.flac"), "")
	readDir, findFor := devReadDir, devFindFor
	t.Cleanup(func() { devReadDir, devFindFor = readDir, findFor })
	delay := 4 * time.Millisecond // a folder read on a slow card
	var rooted atomic.Int64       // when a search last began to walk: the card's root read
	devReadDir = func(name string) ([]os.DirEntry, error) {
		if name == r.card {
			rooted.Store(time.Now().UnixNano())
		}
		time.Sleep(delay)
		return readDir(name)
	}
	devFindFor = 30 * time.Millisecond
	follow := func(text string) (pages int, got []string) {
		cursor := ""
		for pages = 1; pages < 200; pages++ {
			args := []string{text}
			if cursor != "" {
				args = append(args, cursor)
			}
			start := time.Now()
			rep := devAsk(t, r.a, "search", args...)
			if took, limit := time.Since(start), devFindFor+300*time.Millisecond; took > limit {
				t.Fatalf("page %d of %q took %v", pages, text, took)
			}
			if rep.Cursor != "" && rep.Message != "pages" {
				t.Fatalf("cursor without pages %+v", rep)
			}
			got = append(got, devTitles(rep)...)
			if cursor = rep.Cursor; cursor == "" {
				return pages, got
			}
			for _, title := range got {
				if strings.HasPrefix(title, "No song") {
					t.Fatalf("page %d of %q, still looking: %q", pages, text, got)
				}
			}
		}
		t.Fatalf("%q never ended: %q", text, got)
		return
	}
	start := time.Now()
	rep := devAsk(t, r.a, "search", "zzz")
	if took := time.Since(start); took > devFindFor+300*time.Millisecond || rep.Message != "pages" || rep.Cursor == "" {
		t.Fatalf("first answer after %v: %+v", took, rep)
	}
	devAsk(t, r.a, "tags", "/")
	if pages, got := follow("zzz"); pages < 2 || strings.Join(got, ",") != "zzz Top.flac" {
		t.Fatalf("%d pages: %q", pages, got)
	}
	if pages, got := follow("zzz"); pages != 1 || strings.Join(got, ",") != "zzz Top.flac" { // Back to it
		t.Fatalf("again %d pages: %q", pages, got)
	}
	devAsk(t, r.a, "tags", "/")
	if _, got := follow("nowhere"); strings.Join(got, ",") != "No song's name holds “nowhere”" {
		t.Fatalf("none %q", got)
	}
	// A walk the Q2 left (Back, then a new search; 8 s of folders, 5 s an answer) gives way to the
	// new search at once: it begins to walk within a folder's read, not once the left one's time is up.
	devAsk(t, r.a, "tags", "/")
	delay, devFindFor = 100*time.Millisecond, 5*time.Second
	start = time.Now()
	go r.a.device(q2spot.Request{ID: "1", Op: "search", Args: []string{"song"}})
	waitFor(t, "the first search to walk", 5*time.Second, func() bool { return rooted.Load() >= start.UnixNano() })
	time.Sleep(300 * time.Millisecond)
	start = time.Now()
	go r.a.device(q2spot.Request{ID: "2", Op: "search", Args: []string{"top"}})
	waitFor(t, "the new search to walk", 5*time.Second, func() bool { return rooted.Load() >= start.UnixNano() })
	if took := time.Duration(rooted.Load() - start.UnixNano()); took > 2*delay+time.Second {
		t.Fatalf("a new search waited %v", took)
	}
	devAsk(t, r.a, "tags", "/")  // that one left too
	r.a.devTags().finding.Lock() // the walks over
	r.a.devTags().finding.Unlock()
}

// Every answer of Find a Song comes within devFindFor (and a folder's read), not only its first: the
// Q2 shows its Stop row once a level takes 3 s, and the level its More… row opens (after the ten
// pages it asks for by itself) asks with a cursor, as those pages do.
func TestDevTagsFindEveryPage(t *testing.T) {
	if devFindFor > 2500*time.Millisecond {
		t.Fatalf("Find a Song looks %v before an answer", devFindFor)
	}
	r := devRig(t)
	for i := 0; i < 400; i++ {
		os.MkdirAll(filepath.Join(r.card, fmt.Sprintf("Lib/A%04d", i)), 0o755)
	}
	write(t, filepath.Join(r.card, "Lib/A0399/zzz Top.flac"), "")
	readDir, findFor := devReadDir, devFindFor
	t.Cleanup(func() { devReadDir, devFindFor = readDir, findFor })
	const delay = 5 * time.Millisecond // a folder read on a slow card: 2 s or more for them all
	devReadDir = func(name string) ([]os.DirEntry, error) { time.Sleep(delay); return readDir(name) }
	devFindFor = 400 * time.Millisecond
	var got []string
	cursors := 0
	for cursor, n := "", 0; n == 0 || cursor != ""; n++ {
		args := []string{"zzz"}
		if cursor != "" {
			args = append(args, cursor) // a page the Q2 asks for itself, or More…: alike
			cursors++
		}
		start := time.Now()
		rep := devAsk(t, r.a, "search", args...)
		if took := time.Since(start); took > devFindFor+delay+300*time.Millisecond {
			t.Fatalf("answer %d (%q) took %v", n+1, args, took)
		}
		if n > 100 {
			t.Fatalf("never ended: %q", got)
		}
		got = append(got, devTitles(rep)...)
		cursor = rep.Cursor
	}
	if cursors < 3 || strings.Join(got, ",") != "zzz Top.flac" {
		t.Fatalf("%d answers with a cursor: %q", cursors, got)
	}
}

// The card lent to USB storage while Find a Song walks cuts the search short: the Q2 is told the
// card isn't there, and the same search once it is back (Retry, or More… after a later page) looks
// again, not answering from the walk cut short.
func TestDevTagsFindCardAway(t *testing.T) {
	var gone atomic.Bool
	var reads, goAt atomic.Int32 // goAt: the folder read at which the card goes
	readDir, ready := devReadDir, cardReady
	t.Cleanup(func() { devReadDir = readDir })
	devReadDir = func(name string) ([]os.DirEntry, error) {
		if reads.Add(1) == goAt.Load() {
			gone.Store(true)
		}
		if gone.Load() { // the mount point empty: nothing under it is there
			return nil, os.ErrNotExist
		}
		return readDir(name)
	}
	fresh := func() *rig {
		r := devRig(t)
		gone.Store(false)
		reads.Store(0)
		goAt.Store(0)
		cardReady = func() bool { return !gone.Load() }
		t.Cleanup(func() { cardReady = ready })
		return r
	}
	follow := func(r *rig, cursor string) (got []string) {
		for n := 0; n < 20; n++ {
			args := []string{"zzz"}
			if cursor != "" {
				args = append(args, cursor)
			}
			rep := devAsk(t, r.a, "search", args...)
			if rep.Status != "" && rep.Status != q2spot.OK {
				t.Fatalf("%q: %+v", args, rep)
			}
			got = append(got, devTitles(rep)...)
			if cursor = rep.Cursor; cursor == "" {
				return got
			}
		}
		t.Fatalf("never ended: %q", got)
		return
	}

	// Gone on the first page.
	r := fresh()
	for i := 0; i < 40; i++ {
		write(t, filepath.Join(r.card, fmt.Sprintf("Lib/Artist %02d/Album/01 Song.flac", i)), "")
	}
	write(t, filepath.Join(r.card, "Lib/Artist 39/Album/zzz Top.flac"), "")
	goAt.Store(10)
	if rep := devAsk(t, r.a, "search", "zzz"); rep.Status != q2spot.Unavailable || !strings.Contains(rep.Message, "USB") {
		t.Fatalf("card gone %+v", rep)
	}
	gone.Store(false) // back: Retry asks the same
	if got := follow(r, ""); strings.Join(got, ",") != "zzz Top.flac" {
		t.Fatalf("card back %q", got)
	}

	// Gone on a later page: the Q2 keeps the rows it has and offers More…, which asks with the
	// cursor it had.
	r = fresh()
	var want []string
	for i := 0; i < 60; i++ {
		write(t, filepath.Join(r.card, fmt.Sprintf("Many/zzz %02d.flac", i)), "")
		if i >= q2spot.MaxRows {
			want = append(want, fmt.Sprintf("zzz %02d.flac", i))
		}
	}
	write(t, filepath.Join(r.card, "Zed/zzz Late.flac"), "") // after Music, which the walk reads first
	want = append(want, "zzz Late.flac")
	first := devAsk(t, r.a, "search", "zzz")
	if len(first.Rows) != q2spot.MaxRows || first.Cursor != strconv.Itoa(q2spot.MaxRows) {
		t.Fatalf("first page %d rows, %q", len(first.Rows), first.Cursor)
	}
	goAt.Store(reads.Load() + 1) // as the next page walks
	if rep := devAsk(t, r.a, "search", "zzz", first.Cursor); rep.Status != q2spot.Unavailable {
		t.Fatalf("card gone %+v", rep)
	}
	gone.Store(false)
	if got := follow(r, first.Cursor); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("card back, More… %q", got)
	}
}

// A song whose cover is neither a JPEG nor a PNG says so on its level: the Q2 won't show it.
func TestDevTagsCoverNote(t *testing.T) {
	r := devRig(t)
	writeMP3(t, filepath.Join(r.card, "Pics/gif.mp3"), "image/gif", tinyGIF)
	writeMP3(t, filepath.Join(r.card, "Pics/jpeg.mp3"), "image/jpeg", tinyJPEG)
	rep := devAsk(t, r.a, "song", "/Pics/gif.mp3")
	if row := devFind(t, rep, "The cover is a GIF, which the Q2 doesn't show: pick a new one"); row.Kind != "info" {
		t.Fatalf("note %q", row)
	}
	for _, title := range devTitles(devAsk(t, r.a, "song", "/Pics/jpeg.mp3")) {
		if strings.Contains(title, "cover") {
			t.Fatalf("a JPEG's note %q", title)
		}
	}
}

// A Save for All is busy from the press to its last song, between songs too: File Browser doesn't
// turn itself off in the middle, and the menu counts the songs it has left all along.
func TestDevTagsBatchBusy(t *testing.T) {
	r := devRig(t)
	box := devBox(t, r, 5)
	devWait = 20 * time.Millisecond
	at, open := devHold(t, func(p string) bool { return p == box[2] })
	if rep := devAsk(t, r.a, "setdir", "genre|/Music/Box|Rock"); rep.Message != "Still saving in the background" {
		t.Fatalf("save %+v", rep)
	}
	<-at // between the second song and the third
	if !r.a.files.busy() {
		t.Fatal("not busy between songs")
	}
	if row := devAsk(t, r.a, "menu").Rows[2]; row.Title != "Saving tags · 03 song.flac" || row.Subtitle != "3 songs left" {
		t.Fatalf("menu %q", row)
	}
	tr := r.a.files.beginTags("x.flac") // the phone's, at once
	if row := devAsk(t, r.a, "menu").Rows[2]; row.Subtitle != "4 songs left" {
		t.Fatalf("menu with the phone's %q", row)
	}
	r.a.files.end(tr)
	r.a.mu.Lock()
	r.a.used = time.Now().Add(-time.Hour)
	r.a.mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatal("turned off in the middle of a save")
	default:
	}
	close(open)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("not turned off once saved")
	}
	for _, p := range box {
		if got := devRead(t, r, p)[tags.Genre]; got != "Rock" {
			t.Fatalf("%s: %q", p, got)
		}
	}
}

// Turning off stops a Save for All between songs: the song under way is let finish, the rest are
// left as they were.
func TestDevTagsBatchStops(t *testing.T) {
	r := devRig(t)
	box := devBox(t, r, 12)
	devWait = 20 * time.Millisecond
	at, open := devHold(t, func(p string) bool { return p == box[2] })
	devAsk(t, r.a, "setdir", "album|/Music/Box|New")
	<-at
	start := time.Now()
	r.a.shutdown()
	waitFor(t, "turning off", time.Second, r.a.files.stopped)
	close(open)
	<-r.done
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("turning off took %v", took)
	}
	for i, p := range box {
		if got, want := devRead(t, r, p)[tags.Album], map[bool]string{true: "New", false: "Old"}[i < 2]; got != want {
			t.Fatalf("%s: %q", p, got)
		}
	}
}

// Update Library pressed while tags are being saved asks once they are, so the scan reads them as
// saved.
func TestDevTagsLibraryWaits(t *testing.T) {
	r := devRig(t)
	box := devBox(t, r, 6)
	os.WriteFile(capsFile, []byte("progress pages library goto\n"), 0o644)
	devWait = 20 * time.Millisecond
	at, open := devHold(t, func(p string) bool { return p == box[2] })
	devAsk(t, r.a, "setdir", "album|/Music/Box|Library Race")
	<-at
	for i := 0; i < 2; i++ {
		if rep := devAsk(t, r.a, "library"); rep.Message != "The library updates once the tags are saved and nothing plays" {
			t.Fatalf("library %+v", rep)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(libraryFile); err == nil {
		t.Fatal("asked while saving")
	}
	close(open)
	devAsked(t, r.a)
	for _, p := range box {
		if got := devRead(t, r, p)[tags.Album]; got != "Library Race" {
			t.Fatalf("%s: %q", p, got)
		}
	}
	// The phone's save, likewise.
	os.Remove(libraryFile)
	tr := r.a.files.beginTags("x.flac")
	if rep := devAsk(t, r.a, "library"); rep.Message != "The library updates once the tags are saved and nothing plays" {
		t.Fatalf("phone's %+v", rep)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(libraryFile); err == nil {
		t.Fatal("asked while the phone saves")
	}
	r.a.files.end(tr)
	devAsked(t, r.a)
	if rep := devAsk(t, r.a, "library"); rep.Message != "The library updates once nothing plays" {
		t.Fatalf("idle %+v", rep)
	}
}

// A field offers no pick that is its value now, and the T9 row and box say how much it takes;
// Save for All says how many songs it saved.
func TestDevTagsOffers(t *testing.T) {
	r := devRig(t)
	devAsk(t, r.a, "set", "albumartist|"+devPendulum+"|Broadcast")
	devAsk(t, r.a, "set", "disc|"+devPendulum+"|2")
	for ctx, gone := range map[string]string{
		"title|" + devPendulum:       "From the File Name: Pendulum",
		"albumartist|" + devPendulum: "Same as Artist: Broadcast",
		"track|" + devPendulum:       "1: Its Place in the Folder",
		"disc|" + devPendulum:        "Disc 2",
		"genre|" + devPendulum:       "Electronic",
	} {
		rep := devAsk(t, r.a, "field", ctx)
		for _, title := range devTitles(rep) {
			if title == gone {
				t.Fatalf("%s offers %q: %q", ctx, gone, devTitles(rep))
			}
		}
		if row := rep.Rows[1]; row.Kind != "search" || !strings.HasPrefix(row.Subtitle, "Up to 60 letters") {
			t.Fatalf("%s: T9 row %q", ctx, row)
		}
	}
	if got := devTitles(devAsk(t, r.a, "field", "disc|"+devPendulum)); strings.Join(got[2:], ",") != "Clear,Disc 1,Disc 3" {
		t.Fatalf("disc %q", got)
	}
	b, err := os.ReadFile("../../contrib/shanlingq2/q2app")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if hint, ok := strings.CutPrefix(l, "search="); ok && (len(hint) > 31 || !strings.Contains(hint, "60")) {
			t.Fatalf("hint %q", hint)
		}
	}
	devAsk(t, r.a, "field", "album|"+devPendulum)
	if rep := devPress(t, r.a, devFind(t, devAsk(t, r.a, "search", "Five"), "Save for All 2 Songs in This Folder")); rep.Message != "Saved 2 songs" {
		t.Fatalf("save all %+v", rep)
	}
}

// The songs playing: the card's songs other processes hold open, this one's aside, three by name.
func TestDevPlaying(t *testing.T) {
	r := devRig(t)
	proc := devProcRoot
	link := func(pid, fd, to string) {
		os.MkdirAll(filepath.Join(proc, pid, "fd"), 0o755)
		if err := os.Symlink(to, filepath.Join(proc, pid, "fd", fd)); err != nil {
			t.Fatal(err)
		}
	}
	card := func(p string) string { return filepath.Join(r.card, filepath.FromSlash(p)) }
	for _, p := range []string{"/Music/Y/y.flac", "/Music/Z/z.flac", "/Music/Mine/m.flac", "/.deezer/d.flac"} {
		writeFLAC(t, card(p), "TITLE=x")
	}
	link("100", "3", card("/Music/Broadcast/02 Valerie.flac"))
	link("100", "4", "/dev/null")
	link("100", "5", "pipe:[1234]")
	link("101", "0", card(devPendulum)+" (deleted)") // written again since: the new one is there
	link("101", "1", card("/Music/Old/old.flac")+" (deleted)")
	link("102", "1", card("/page.html"))
	link("103", "2", card("/.deezer/d.flac"))
	link("104", "7", card("/Music/Z/z.flac"))
	link("104", "8", card("/Music/Y/y.flac"))
	link("104", "9", card("/Music/Y/y.flac"))
	link(strconv.Itoa(os.Getpid()), "5", card("/Music/Mine/m.flac"))
	link("self", "5", card("/Music/Mine/m.flac"))
	got := devPlaying(r.card)
	if strings.Join(got, ",") != devPendulum+",/Music/Broadcast/02 Valerie.flac,/Music/Y/y.flac" {
		t.Fatalf("playing %q", got)
	}
	rep := devAsk(t, r.a, "tags", "/")
	if row := rep.Rows[0]; row.Kind != "list" || row.URI != "song" || row.Title != "Playing: 01 Pendulum.flac" ||
		row.Context != devPendulum || row.Subtitle != "/Music/Broadcast" {
		t.Fatalf("row %q", rep.Rows)
	}
	if rep.Rows[3].Title != "Find a Song" {
		t.Fatalf("rows %q", devTitles(rep))
	}
	devProcRoot = filepath.Join(proc, "nowhere")
	if got := devPlaying(r.card); len(got) != 0 {
		t.Fatalf("no proc %q", got)
	}
}
