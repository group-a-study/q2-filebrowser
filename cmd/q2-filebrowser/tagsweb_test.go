package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
	"github.com/group-a-study/q2-filebrowser/internal/tags"
)

// tinyJPEG is a baseline JPEG's header, 16 by 16: all the tag editor looks at.
var tinyJPEG = []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x10, 0x00, 0x10, 0x01, 0x01, 0x11, 0x00, 0xFF, 0xD9}

// tinyGIF is a GIF of one pixel, a cover the Q2 doesn't show.
var tinyGIF = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

// tagsOf is GET tags for paths: each file's reply by its path.
func (r *rig) tagsOf(paths ...string) (int, map[string]map[string]any, []string) {
	r.t.Helper()
	code, out := r.call("tags?"+url.Values{"path": paths}.Encode(), nil)
	byPath, order := map[string]map[string]any{}, []string{}
	fs, _ := out["files"].([]any)
	for _, f := range fs {
		m := f.(map[string]any)
		byPath[m["path"].(string)] = m
		order = append(order, m["path"].(string))
	}
	return code, byPath, order
}

func field(f map[string]any, k string) any {
	fs, _ := f["fields"].(map[string]any)
	return fs[k]
}

// The tag editor from the page: one song's tags, several, a folder's; saving one, several,
// numbered; the cover; nothing while read only, without X-Q2, with the card away, or outside it.
func TestTags(t *testing.T) {
	r := newRig(t)
	r.signIn()
	album := filepath.Join(r.card, "Tags/Album")
	writeFLAC(t, filepath.Join(album, "10 Ten.flac"), "TITLE=Ten", "ARTIST=A", "ALBUM=X")
	writeFLAC(t, filepath.Join(album, "01 One.flac"), "TITLE=One", "ARTIST=A", "ALBUM=X", "GENRE=Pop")
	writeFLAC(t, filepath.Join(album, "02 Two.flac"), "TITLE=Two", "ARTIST=B", "ALBUM=X")
	writeFLAC(t, filepath.Join(album, ".hidden.flac"), "TITLE=Hidden")
	write(t, filepath.Join(album, "cover.jpg"), string(tinyJPEG))
	one, two, ten := "/Tags/Album/01 One.flac", "/Tags/Album/02 Two.flac", "/Tags/Album/10 Ten.flac"
	audio := func(rel string) string { b := r.read(rel); return b[max(0, len(b)-8):] }
	tail := audio("Tags/Album/01 One.flac")

	// What the page is told: the tag editor is here, which files it takes, no library request.
	if code, out := r.call("state", nil); code != 200 || out["tags"] != true || out["library"] != false ||
		!strings.Contains(fmt.Sprint(out["tag_exts"]), ".flac") {
		t.Fatalf("state %d %v", code, out)
	}

	// Reading: one song, several, a folder's songs by name (not its dot files or pictures).
	code, fs, _ := r.tagsOf(one)
	if f := fs[one]; code != 200 || len(fs) != 1 || f["name"] != "01 One.flac" || f["format"] != "FLAC" || f["writable"] != true ||
		field(f, "title") != "One" || field(f, "genre") != "Pop" || f["picture"] != nil || f["error"] != nil {
		t.Fatalf("one %d %v", code, fs)
	}
	if code, fs, _ = r.tagsOf(one, two); code != 200 || len(fs) != 2 || field(fs[two], "artist") != "B" {
		t.Fatalf("several %d %v", code, fs)
	}
	code, fs, order := r.tagsOf("/Tags/Album", one)
	if code != 200 || strings.Join(order, ",") != one+","+two+","+ten || field(fs[ten], "title") != "Ten" {
		t.Fatalf("folder %d %v", code, order)
	}
	if code, fs, _ = r.tagsOf("/Tags/Album/cover.jpg", "/Tags/Album/nope.flac"); code != 200 ||
		!strings.Contains(fs["/Tags/Album/cover.jpg"]["error"].(string), "can't read the tags") ||
		!strings.Contains(fs["/Tags/Album/nope.flac"]["error"].(string), "isn't on the card") {
		t.Fatalf("not songs %d %v", code, fs)
	}
	if code, out := r.call("tags", nil); code != 400 {
		t.Fatalf("nothing %d %v", code, out)
	}
	many := make([]string, 501)
	for i := range many {
		many[i] = fmt.Sprintf("/a%d.flac", i)
	}
	if code, out := r.call("tags?"+url.Values{"path": many}.Encode(), nil); code != 400 || !strings.Contains(out["error"].(string), "500") {
		t.Fatalf("too many %d %v", code, out)
	}

	// Saving one: only the fields sent change, "" clears one, the audio stays as it was.
	code, out := r.call("tags", map[string]any{"paths": []string{one}, "set": map[string]string{"title": "Uno", "genre": "", "date": "2004"}})
	if code != 200 || fmt.Sprint(out["saved"]) != "["+one+"]" || fmt.Sprint(out["failed"]) != "[]" {
		t.Fatalf("save one %d %v", code, out)
	}
	if _, fs, _ = r.tagsOf(one); field(fs[one], "title") != "Uno" || field(fs[one], "genre") != nil ||
		field(fs[one], "date") != "2004" || field(fs[one], "artist") != "A" || audio("Tags/Album/01 One.flac") != tail {
		t.Fatalf("saved one %v", fs)
	}
	// Several, a missing one and a picture among them: the songs saved, the others said why.
	code, out = r.call("tags", map[string]any{"paths": []string{one, "/Tags/Album/nope.flac", two, "/Tags/Album/cover.jpg"},
		"set": map[string]string{"album": "New", "albumartist": "Various"}})
	fails, _ := out["failed"].([]any)
	if code != 200 || fmt.Sprint(out["saved"]) != "["+one+" "+two+"]" || len(fails) != 2 ||
		fails[0].(map[string]any)["path"] != "/Tags/Album/nope.flac" || fails[0].(map[string]any)["code"] != float64(404) ||
		!strings.Contains(fails[1].(map[string]any)["error"].(string), "cover.jpg") {
		t.Fatalf("save several %d %v", code, out)
	}
	if _, fs, _ = r.tagsOf(one, two); field(fs[one], "album") != "New" || field(fs[two], "album") != "New" ||
		field(fs[one], "artist") != "A" || field(fs[two], "artist") != "B" || field(fs[two], "albumartist") != "Various" {
		t.Fatalf("saved several %v", fs)
	}
	// Numbered in the order sent: 1 to N of N.
	if code, out = r.call("tags", map[string]any{"paths": []string{one, two, ten}, "number": true}); code != 200 || len(out["saved"].([]any)) != 3 {
		t.Fatalf("number %d %v", code, out)
	}
	if _, fs, _ = r.tagsOf("/Tags/Album"); field(fs[one], "track") != "1" || field(fs[two], "track") != "2" ||
		field(fs[ten], "track") != "3" || field(fs[ten], "tracktotal") != "3" {
		t.Fatalf("numbered %v", fs)
	}
	// What's wrong with the changes refuses them all, nothing written.
	for _, bad := range []map[string]any{
		{"paths": []string{one}, "set": map[string]string{"track": "two"}},
		{"paths": []string{one}, "set": map[string]string{"mood": "x"}},
		{"paths": []string{one}, "picture": "data:image/gif;base64," + base64.StdEncoding.EncodeToString([]byte("GIF89a......"))},
		{"paths": []string{one}, "picture": "not a data URL"},
		{"paths": []string{one}},
		{"paths": []string{}, "set": map[string]string{"title": "x"}},
	} {
		if code, out := r.call("tags", bad); code != 400 {
			t.Fatalf("bad %v: %d %v", bad, code, out)
		}
	}
	if _, fs, _ = r.tagsOf(one); field(fs[one], "track") != "1" || field(fs[one], "title") != "Uno" {
		t.Fatalf("changed by a bad one %v", fs)
	}
	big := `{"paths":["` + one + `"],"picture":"data:image/jpeg;base64,` + strings.Repeat("A", 7<<20) + `"}`
	if code, b, _ := r.do("POST", "/api/tags", strings.NewReader(big), true); code != 413 {
		t.Fatalf("too big %d %s", code, b)
	}

	// The cover: set from a data URL, read back, sent as it is, removed.
	pic := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(tinyJPEG)
	if code, out = r.call("tags", map[string]any{"paths": []string{one, two}, "picture": pic}); code != 200 || len(out["saved"].([]any)) != 2 {
		t.Fatalf("picture %d %v", code, out)
	}
	if _, fs, _ = r.tagsOf(one); fmt.Sprint(fs[one]["picture"]) != "map[height:16 mime:image/jpeg progressive:false size:17 width:16]" ||
		audio("Tags/Album/01 One.flac") != tail {
		t.Fatalf("picture read %v", fs[one]["picture"])
	}
	code, b, h := r.do("GET", "/api/cover?path="+url.QueryEscape(one), nil, false)
	if code != 200 || !bytes.Equal(b, tinyJPEG) || h.Get("Content-Type") != "image/jpeg" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("cover %d %q %v", code, b, h)
	}
	if code, out = r.call("tags", map[string]any{"paths": []string{one}, "remove_picture": true}); code != 200 || len(out["saved"].([]any)) != 1 {
		t.Fatalf("remove picture %d %v", code, out)
	}
	if _, fs, _ = r.tagsOf(one, two); fs[one]["picture"] != nil || fs[two]["picture"] == nil {
		t.Fatalf("picture removed %v", fs)
	}
	if code, _, _ = r.do("GET", "/api/cover?path="+url.QueryEscape(one), nil, false); code != 404 {
		t.Fatalf("no cover %d", code)
	}
	if code, _, _ = r.do("GET", "/api/cover?path=/Tags/Album/nope.flac", nil, false); code != 404 {
		t.Fatalf("no song %d", code)
	}

	// Nothing outside the card: ../ is the card's own, which isn't there.
	outside := filepath.Join(r.tmp, "outside.flac")
	writeFLAC(t, outside, "TITLE=Outside")
	was, _ := os.ReadFile(outside)
	esc := "/../outside.flac"
	if _, fs, _ = r.tagsOf(esc); !strings.Contains(fmt.Sprint(fs["/outside.flac"]["error"]), "isn't on the card") || field(fs["/outside.flac"], "title") != nil {
		t.Fatalf("escape read %v", fs)
	}
	if code, out = r.call("tags", map[string]any{"paths": []string{esc}, "set": map[string]string{"title": "Got"}}); code != 200 || len(out["saved"].([]any)) != 0 {
		t.Fatalf("escape write %d %v", code, out)
	}
	if code, _, _ = r.do("GET", "/api/cover?path="+url.QueryEscape(esc), nil, false); code != 404 {
		t.Fatalf("escape cover %d", code)
	}
	if now, _ := os.ReadFile(outside); !bytes.Equal(now, was) {
		t.Fatal("the file outside the card changed")
	}

	// A change needs X-Q2.
	body, _ := json.Marshal(map[string]any{"paths": []string{one}, "set": map[string]string{"title": "Sneaky"}})
	if code, _, _ = r.do("POST", "/api/tags", bytes.NewReader(body), false); code != 400 {
		t.Fatalf("no X-Q2 %d", code)
	}
	if code, _, _ = r.do("POST", "/api/library", strings.NewReader("{}"), false); code != 400 {
		t.Fatalf("library without X-Q2 %d", code)
	}

	// Read only (from the Q2): the tags and the cover can be looked at, not changed.
	r.ask("readonly")
	if code, out = r.call("tags", map[string]any{"paths": []string{one}, "set": map[string]string{"title": "No"}}); code != 403 ||
		!strings.Contains(out["error"].(string), "Read only") {
		t.Fatalf("read only save %d %v", code, out)
	}
	if code, out = r.call("library", map[string]any{}); code != 403 {
		t.Fatalf("read only library %d %v", code, out)
	}
	if code, fs, _ = r.tagsOf(one); code != 200 || field(fs[one], "title") != "Uno" {
		t.Fatalf("read only read %d %v", code, fs)
	}
	if code, _, _ = r.do("GET", "/api/cover?path="+url.QueryEscape(two), nil, false); code != 200 {
		t.Fatalf("read only cover %d", code)
	}
	r.ask("readonly")

	// The card lent to USB storage: nothing reaches it.
	cardReady = func() bool { return false }
	if code, _, _ = r.tagsOf(one); code != 503 {
		t.Fatalf("no card read %d", code)
	}
	if code, _ = r.call("tags", map[string]any{"paths": []string{one}, "set": map[string]string{"title": "No"}}); code != 503 {
		t.Fatalf("no card save %d", code)
	}
	if code, _, _ = r.do("GET", "/api/cover?path="+url.QueryEscape(two), nil, false); code != 503 {
		t.Fatalf("no card cover %d", code)
	}
	if code, _ = r.call("library", map[string]any{}); code != 503 {
		t.Fatalf("no card library %d", code)
	}
	if code, out = r.call("state", nil); code != 200 || out["card"] != false {
		t.Fatalf("no card state %d %v", code, out)
	}
	cardReady = func() bool { return true }

	// Nothing left behind, nothing still moving.
	if ps, _ := filepath.Glob(filepath.Join(album, "*"+partExt)); len(ps) > 0 {
		t.Fatalf("parts left %v", ps)
	}
	if r.a.files.busy() {
		t.Fatal("a tag save left as moving")
	}
}

// The library: asked for only when the Q2 Pod says it takes the request.
func TestTagsLibrary(t *testing.T) {
	r := newRig(t)
	r.signIn()
	dir := t.TempDir()
	oldCaps, oldLib := capsFile, libraryFile
	capsFile, libraryFile = filepath.Join(dir, "caps"), filepath.Join(dir, "library")
	t.Cleanup(func() { capsFile, libraryFile = oldCaps, oldLib })
	if code, out := r.call("library", map[string]any{}); code != 501 || !strings.Contains(out["error"].(string), "Update Local Music") {
		t.Fatalf("no caps %d %v", code, out)
	}
	if _, err := os.Stat(libraryFile); !os.IsNotExist(err) {
		t.Fatal("asked without the caps")
	}
	write(t, capsFile, "volume library tags\n")
	if code, out := r.call("state", nil); code != 200 || out["library"] != true {
		t.Fatalf("state %d %v", code, out)
	}
	if code, out := r.call("library", map[string]any{}); code != 200 || out["ok"] != true || out["later"] != false {
		t.Fatalf("library %d %v", code, out)
	}
	if b, _ := os.ReadFile(libraryFile); !regexp.MustCompile(`^\d{1,23}\nscan\n$`).Match(b) {
		t.Fatalf("request %q", b)
	}
	if code, _, _ := r.do("GET", "/api/library", nil, false); code != 404 {
		t.Fatalf("GET library %d", code)
	}
}

// writeMP3 makes a small MP3 at full (its folders too): an ID3v2.4 tag with a title and a front cover
// of type mime (none when pic is nil), 1 KB of padding, and a few bytes standing in for the audio.
func writeMP3(t *testing.T, full, mime string, pic []byte) {
	t.Helper()
	syncsafe := func(n int) []byte {
		return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
	}
	var fr bytes.Buffer
	frame := func(id string, body []byte) {
		fr.WriteString(id)
		fr.Write(syncsafe(len(body)))
		fr.Write([]byte{0, 0})
		fr.Write(body)
	}
	frame("TIT2", []byte("\x03Song"))
	if pic != nil {
		frame("APIC", append([]byte("\x00"+mime+"\x00\x03\x00"), pic...))
	}
	var b bytes.Buffer
	b.WriteString("ID3\x04\x00\x00")
	b.Write(syncsafe(fr.Len() + 1024))
	b.Write(fr.Bytes())
	b.Write(make([]byte, 1024))
	b.Write([]byte{0xFF, 0xFB, 0x90, 0x00})
	b.Write(make([]byte, 413))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// heldPeak is the most the heap held above where it started while do ran: the live heap as each GC
// found it, garbage left out, with the GC made eager so that it looks often.
func heldPeak(do func()) (held int64) {
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtime.GC()
	metrics.Read(live)
	base := live[0].Value.Uint64()
	var peak atomic.Uint64
	stop, sampled := make(chan struct{}), make(chan struct{})
	defer func() { // also when do fails the test
		close(stop)
		<-sampled
		held = int64(peak.Load()) - int64(base)
	}()
	go func() {
		defer close(sampled)
		s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
		for {
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	}()
	do()
	return 0
}

// liveHeap is the heap's live bytes, as a GC finds them with nothing else allocating.
func liveHeap() int64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// The cover is sent from the song a piece at a time: a big one isn't held (the Q2 has 32 MB), and
// what arrives is the picture as it is, a range of it too.
func TestTagsCoverStreams(t *testing.T) {
	r := newRig(t)
	r.signIn()
	pic := make([]byte, 4<<20)
	rand.New(rand.NewSource(1)).Read(pic)
	copy(pic, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	writeMP3(t, filepath.Join(r.card, "Big/big.mp3"), "image/jpeg", pic)
	get := func(rng string) (int, string, []byte, uint64) {
		req, _ := http.NewRequest("GET", r.base+"/api/cover?path="+url.QueryEscape("/Big/big.mp3"), nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		resp, err := r.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		io.Copy(h, resp.Body)
		resp.Body.Close()
		runtime.ReadMemStats(&after)
		return resp.StatusCode, resp.Header.Get("Content-Type"), h.Sum(nil), after.TotalAlloc - before.TotalAlloc
	}
	all, first := sha256.Sum256(pic), sha256.Sum256(pic[:4])
	code, typ, sum, alloc := get("")
	if code != 200 || typ != "image/jpeg" || !bytes.Equal(sum, all[:]) {
		t.Fatalf("cover %d %s, not the picture", code, typ)
	}
	// All that was allocated while it went, the server's and this client's: a fraction of it.
	if alloc > 1<<20 {
		t.Fatalf("sending a %d MB cover allocated %d KB", len(pic)>>20, alloc>>10)
	}
	if code, _, sum, _ = get("bytes=0-3"); code != 206 || !bytes.Equal(sum, first[:]) {
		t.Fatalf("range %d", code)
	}
	if peak := heldPeak(func() { get("") }); peak > 1<<20 {
		t.Fatalf("sending a %d MB cover held %d KB", len(pic)>>20, peak>>10)
	}
}

// countTo counts the bytes written to it.
type countTo struct{ n *atomic.Int64 }

func (c countTo) Write(b []byte) (int, error) { c.n.Add(int64(len(b))); return len(b), nil }

// Many songs' tags go out a song at a time, as each is read, and without their lyrics, which the
// page shows for one song alone: a big folder isn't held whole (the Q2 has 32 MB). Looked at with
// the last song's read held, when nothing allocates: most of the reply has reached the page, and the
// heap holds a little of it, not the songs read before (held whole, the reply took twice its size
// and more: the songs' tags, then their JSON).
func TestTagsManySongs(t *testing.T) {
	r := newRig(t)
	r.signIn()
	const songs = 200
	lyrics, comment := strings.Repeat("la ", 20<<10), strings.Repeat("c", 40<<10)
	for i := 0; i < songs; i++ {
		writeFLAC(t, filepath.Join(r.card, "Big", fmt.Sprintf("%03d song.flac", i)), "TITLE=Song", "LYRICS="+lyrics, "COMMENT="+comment)
	}
	last := fmt.Sprintf("/Big/%03d song.flac", songs-1)
	at, open := make(chan struct{}), make(chan struct{})
	read := apiReadTags
	t.Cleanup(func() { apiReadTags = read })
	apiReadTags = func(f *files, p string) tagFile {
		if p == last {
			close(at)
			<-open
		}
		return read(f, p)
	}
	var n atomic.Int64
	var code int
	fetched := make(chan error, 1)
	base := liveHeap()
	go func() {
		resp, err := r.hc.Get(r.base + "/api/tags?path=" + url.QueryEscape("/Big"))
		if err == nil {
			code = resp.StatusCode
			_, err = io.Copy(countTo{&n}, resp.Body)
			resp.Body.Close()
		}
		fetched <- err
	}()
	select {
	case <-at:
	case <-time.After(time.Minute):
		t.Fatal("the last song never read")
	}
	half, quarter := int64(songs*len(comment)/2), int64(songs*len(comment)/4)
	for end := time.Now().Add(10 * time.Second); n.Load() < half && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
	}
	early, held := n.Load(), liveHeap()-base
	close(open)
	if err := <-fetched; err != nil || code != 200 {
		t.Fatalf("%d, %v", code, err)
	}
	apiReadTags = read
	if early < half || held > quarter {
		t.Fatalf("with the last of %d songs being read, %d KB of %d KB had come, %d KB held", songs, early>>10,
			n.Load()>>10, held>>10)
	}
	// Their comments, not their lyrics.
	if got := n.Load(); got < songs*int64(len(comment)) || got > songs*int64(len(comment)+2<<10) {
		t.Fatalf("%d songs' tags: %d KB", songs, got>>10)
	}
	code, fs, order := r.tagsOf("/Big")
	if code != 200 || len(order) != 200 || order[7] != "/Big/007 song.flac" {
		t.Fatalf("folder %d %d", code, len(order))
	}
	for p, f := range fs {
		if field(f, "lyrics") != nil || field(f, "comment") != comment || field(f, "title") != "Song" {
			t.Fatalf("%s %v", p, f["fields"])
		}
	}
	if _, fs, _ = r.tagsOf("/Big/001 song.flac", "/Big/002 song.flac"); len(fs) != 2 || field(fs["/Big/001 song.flac"], "lyrics") != nil {
		t.Fatalf("two songs: lyrics sent")
	}
	if _, fs, _ = r.tagsOf("/Big/007 song.flac"); field(fs["/Big/007 song.flac"], "lyrics") != strings.TrimSpace(lyrics) {
		t.Fatalf("one song: no lyrics")
	}
}

// A cover that isn't a JPEG or a PNG comes named by its own type, with a note (once) that the Q2
// won't show it; a JPEG gets none.
func TestTagsCoverKind(t *testing.T) {
	r := devRig(t)
	r.signIn()
	writeMP3(t, filepath.Join(r.card, "Pics/gif.mp3"), "image/gif", tinyGIF)
	writeMP3(t, filepath.Join(r.card, "Pics/junk.mp3"), "text/html", []byte("<script>alert(1)</script>"))
	writeMP3(t, filepath.Join(r.card, "Pics/jpeg.mp3"), "image/jpeg", tinyJPEG)
	_, fs, _ := r.tagsOf("/Pics")
	for _, c := range []struct{ path, mime, note string }{
		{"/Pics/gif.mp3", "image/gif", "The cover is a GIF, which the Q2 doesn't show"},
		{"/Pics/junk.mp3", "text/html", "The cover isn't a JPEG or a PNG, so the Q2 doesn't show it"},
		{"/Pics/jpeg.mp3", "image/jpeg", ""},
	} {
		f := fs[c.path]
		pic, _ := f["picture"].(map[string]any)
		notes := fmt.Sprint(f["notes"])
		if pic == nil || pic["mime"] != c.mime || c.note != "" && strings.Count(notes, c.note) != 1 ||
			c.note == "" && strings.Contains(notes, "cover") || strings.Count(notes, "cover") > 1 {
			t.Fatalf("%s: picture %v, notes %s", c.path, pic, notes)
		}
	}
	// Sent as bytes the browser won't take for a page.
	if code, b, h := r.do("GET", "/api/cover?path="+url.QueryEscape("/Pics/junk.mp3"), nil, false); code != 200 ||
		h.Get("Content-Type") != "application/octet-stream" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || string(b) != "<script>alert(1)</script>" {
		t.Fatalf("junk cover %d %v", code, h)
	}
}

// The page's Update library pressed while a Save for All goes on on the Q2 asks once the songs are
// saved, so the scan reads them as saved; the page is told it waits. Nothing being saved, it asks
// at once.
func TestTagsLibraryWaits(t *testing.T) {
	r := devRig(t)
	r.signIn()
	box := devBox(t, r, 6)
	write(t, capsFile, "progress pages library goto\n")
	devWait = 20 * time.Millisecond
	at, open := devHold(t, func(p string) bool { return p == box[2] })
	if rep := devAsk(t, r.a, "setdir", "album|/Music/Box|Library Race"); rep.Message != "Still saving in the background" {
		t.Fatalf("Q2's save %+v", rep)
	}
	<-at // its third song
	code, out := r.call("library", map[string]any{})
	_, err := os.Stat(libraryFile)
	close(open)
	if code != 200 || out["ok"] != true || out["later"] != true || err == nil {
		t.Fatalf("library %d %v, asked at once: %v", code, out, err == nil)
	}
	devAsked(t, r.a)
	for _, p := range box {
		if got := devRead(t, r, p)[tags.Album]; got != "Library Race" {
			t.Fatalf("%s: %q", p, got)
		}
	}
	os.Remove(libraryFile)
	if code, out := r.call("library", map[string]any{}); code != 200 || out["later"] != false {
		t.Fatalf("idle library %d %v", code, out)
	}
	if _, err := os.Stat(libraryFile); err != nil {
		t.Fatal("not asked at once")
	}
}

// A save of several songs from the page is busy from its first song to its last, between songs too,
// as a Save for All on the Q2 is: File Browser doesn't turn itself off halfway, the Q2's menu counts
// the songs it has left, and the Q2's Update Library waits for it.
func TestTagsSaveBusy(t *testing.T) {
	r := devRig(t)
	r.signIn()
	box := devBox(t, r, 3)
	write(t, capsFile, "library\n")
	first := filepath.Join(r.card, filepath.FromSlash(box[0]))
	t.Cleanup(func() { cardReady = func() bool { return true } })
	var (
		mu           sync.Mutex
		looked, busy bool
		menu         q2spot.Row // the Q2's status row then
		library      string     // the Q2's Update Library pressed then
		asked        bool       // and asking at once
	)
	// The first look at the card once the first song is saved is the second's, before it is begun.
	cardReady = func() bool {
		mu.Lock()
		defer mu.Unlock()
		if b, _ := os.ReadFile(first); !looked && bytes.Contains(b, []byte("ALBUM=New")) {
			looked, busy = true, r.a.files.busy()
			menu = r.a.device(q2spot.Request{ID: "1", Op: "menu"}).Rows[2]
			library = r.a.device(q2spot.Request{ID: "2", Op: "library"}).Message
			_, err := os.Stat(libraryFile)
			asked = err == nil
		}
		return true
	}
	code, out := r.call("tags", map[string]any{"paths": box, "set": map[string]string{"album": "New"}})
	if code != 200 || len(out["saved"].([]any)) != 3 {
		t.Fatalf("save %d %v", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !looked || !busy {
		t.Fatalf("looked %v: busy between songs %v", looked, busy)
	}
	if menu.Title != "Saving tags · 02 song.flac" || menu.Subtitle != "2 songs left" {
		t.Fatalf("menu %q", menu)
	}
	if library != "The library updates once the tags are saved and nothing plays" || asked {
		t.Fatalf("library %q, asked at once %v", library, asked)
	}
	devAsked(t, r.a)
	if row := devAsk(t, r.a, "menu").Rows[2]; !strings.HasPrefix(row.Title, "Card: ") || r.a.files.busy() {
		t.Fatalf("after %q", row)
	}
}

// A title with a byte order mark, and lyrics whose lines end in CR alone, read as the server keeps
// them; saved back as read (alone, or with other songs), the files are left as they were.
func TestTagsSaveUnchanged(t *testing.T) {
	r := newRig(t)
	r.signIn()
	p0, p1 := filepath.Join(r.card, "U/p0.flac"), filepath.Join(r.card, "U/p1.flac")
	writeFLAC(t, p0, "TITLE=\ufeffBOM title", "ALBUM=\ufeffShared")
	writeFLAC(t, p1, "LYRICS=line one\rline two\rline three", "ALBUM=Shared")
	was0, _ := os.ReadFile(p0)
	was1, _ := os.ReadFile(p1)
	_, fs, _ := r.tagsOf("/U/p0.flac")
	_, fs1, _ := r.tagsOf("/U/p1.flac")
	if field(fs["/U/p0.flac"], "title") != "BOM title" || field(fs["/U/p0.flac"], "album") != "Shared" ||
		field(fs1["/U/p1.flac"], "lyrics") != "line one\nline two\nline three" {
		t.Fatalf("read %v %v", fs, fs1)
	}
	for _, body := range []map[string]any{
		{"paths": []string{"/U/p0.flac"}, "set": map[string]string{"title": "BOM title"}},
		{"paths": []string{"/U/p1.flac"}, "set": map[string]string{"lyrics": "line one\nline two\nline three\n"}},
		{"paths": []string{"/U/p0.flac", "/U/p1.flac"}, "set": map[string]string{"album": "Shared"}},
	} {
		if code, out := r.call("tags", body); code != 200 || len(out["failed"].([]any)) != 0 {
			t.Fatalf("%v: %d %v", body, code, out)
		}
	}
	if now0, _ := os.ReadFile(p0); !bytes.Equal(now0, was0) {
		t.Fatal("p0.flac written")
	}
	if now1, _ := os.ReadFile(p1); !bytes.Equal(now1, was1) {
		t.Fatal("p1.flac written")
	}
}

// File Browser turning off stops a save between songs: the songs saved stay so, the rest aren't
// tried, each said to be why, and none is left half written.
func TestTagsSaveStopping(t *testing.T) {
	r := newRig(t)
	r.signIn()
	dir := filepath.Join(r.card, "Stop")
	paths := []string{"/Stop/1.flac", "/Stop/2.flac", "/Stop/3.flac"}
	was := map[string][]byte{}
	for _, p := range paths {
		writeFLAC(t, filepath.Join(r.card, p), "TITLE=Old")
		was[p], _ = os.ReadFile(filepath.Join(r.card, p))
	}
	// Turned off once the first song is saved, as the next one starts.
	cardReady = func() bool {
		if b, _ := os.ReadFile(filepath.Join(dir, "1.flac")); bytes.Contains(b, []byte("TITLE=New")) {
			r.a.files.cancel()
		}
		return true
	}
	t.Cleanup(func() { cardReady = func() bool { return true } })
	code, out := r.call("tags", map[string]any{"paths": paths, "set": map[string]string{"title": "New"}})
	fails, _ := out["failed"].([]any)
	if code != 200 || fmt.Sprint(out["saved"]) != "[/Stop/1.flac]" || len(fails) != 2 {
		t.Fatalf("stopping %d %v", code, out)
	}
	for i, f := range fails {
		m := f.(map[string]any)
		if m["path"] != paths[i+1] || m["code"] != float64(503) || !strings.Contains(m["error"].(string), "turning off") {
			t.Fatalf("fail %v", m)
		}
	}
	// Once turning off, a save tries none.
	code, out = r.call("tags", map[string]any{"paths": paths[1:], "set": map[string]string{"title": "New"}})
	if fails, _ = out["failed"].([]any); code != 200 || fmt.Sprint(out["saved"]) != "[]" || len(fails) != 2 {
		t.Fatalf("stopped %d %v", code, out)
	}
	for _, p := range paths[1:] {
		if now, _ := os.ReadFile(filepath.Join(r.card, p)); !bytes.Equal(now, was[p]) {
			t.Fatalf("%s written", p)
		}
	}
	if ps, _ := filepath.Glob(filepath.Join(dir, ".*")); len(ps) > 0 {
		t.Fatalf("left behind %v", ps)
	}
}

// The page's own code: its script parses; it keeps a field's value as the server does, so a sheet
// saved untouched sends nothing; it names a cover by its type. Run where node is installed.
func TestTagsPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run the page's code")
	}
	page := string(indexHTML)
	i, j := strings.Index(page, "<script>"), strings.LastIndex(page, "</script>")
	js := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(js, []byte(page[i+len("<script>"):j]), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", js).CombinedOutput(); err != nil {
		t.Fatalf("the page's script: %v\n%s", err, out)
	}
	fn := func(name string) string {
		a := strings.Index(page, "\nfunction "+name+"(")
		if a < 0 {
			t.Fatalf("no %s in the page", name)
		}
		return page[a : a+strings.Index(page[a:], "\n}\n")+3]
	}
	type value struct {
		K tags.Field `json:"k"`
		V string     `json:"v"`
	}
	values := []value{
		{tags.Title, "\ufeffBOM title"}, {tags.Title, "  Padded \t"}, {tags.Title, "a\r\nb\nc\rd"}, {tags.Title, "x\u0085"},
		{tags.Title, "\u00a0wide\u3000"}, {tags.Title, "nul\x00in"}, {tags.Title, ""}, {tags.Album, "Ünïcödé 日本 \u2028"},
		{tags.Lyrics, "line one\rline two\rline three"}, {tags.Lyrics, "line one\nline two\nline three"},
		{tags.Lyrics, "a\r\nb\r\n\r\n  "}, {tags.Lyrics, "\ufeffla\tla\t"}, {tags.Lyrics, "  lead\n"},
		{tags.Track, "03"}, {tags.Track, "0"}, {tags.Track, " 0012 "}, {tags.DiscTotal, "02"}, {tags.Genre, "Rock "},
	}
	kinds := []string{"image/gif", "text/html", "image/jpeg", "image/png", "image/webp", ""}
	in, _ := json.Marshal(map[string]any{"values": values, "kinds": kinds})
	src := fn("tagValue") + fn("picKind") + fn("libraryAsked") + "const t = " + string(in) + ";\n" +
		"console.log(JSON.stringify({ values: t.values.map(c => tagValue(c.k, c.v)), kinds: t.kinds.map(picKind),\n" +
		"  asked: [{ ok: true, later: true }, { ok: true, later: false }, { ok: true }].map(libraryAsked) }));\n"
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct{ Values, Kinds, Asked []string }
	if err := json.Unmarshal(out, &got); err != nil || len(got.Values) != len(values) || len(got.Asked) != 3 {
		t.Fatalf("node said %s", out)
	}
	// Update library asked while tags are being saved says it waits for them.
	if !strings.Contains(got.Asked[0], "once the tags are saved") || strings.Contains(got.Asked[1], "saved") ||
		got.Asked[1] != got.Asked[2] {
		t.Errorf("library asked %q", got.Asked)
	}
	for i, v := range values {
		if want := tags.Clean(v.K, v.V); got.Values[i] != want {
			t.Errorf("%s %q: the page keeps %q, the server %q", v.K, v.V, got.Values[i], want)
		}
	}
	if s := strings.Join(got.Kinds, ","); s != "GIF,unknown kind of picture,JPEG,PNG,WebP,unknown kind of picture" {
		t.Errorf("kinds %s", s)
	}
}
