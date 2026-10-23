package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
)

type rig struct {
	a    *app
	card string
	tmp  string
	base string
	hc   *http.Client
	done chan struct{}
	t    *testing.T
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func write(t *testing.T, path, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRig serves a card with an album, a song at its root, and the add-ons' dot folders.
func newRig(t *testing.T) *rig {
	tmp := t.TempDir()
	card := filepath.Join(tmp, "card")
	write(t, filepath.Join(card, "Music/Broadcast/01 Pendulum.flac"), "fLaC one")
	write(t, filepath.Join(card, "Music/Broadcast/02 Valerie.flac"), "fLaC two two")
	write(t, filepath.Join(card, "Music/Broadcast/.hidden"), "x")
	write(t, filepath.Join(card, "song.mp3"), strings.Repeat("ID3", 1000))
	write(t, filepath.Join(card, ".deezer/arl"), "secret")
	write(t, filepath.Join(card, "page.html"), "<script>alert(1)</script>")
	cardReady = func() bool { return true }
	o := options{dir: filepath.Join(card, ".filebrowser"), card: card, statusFile: filepath.Join(tmp, "state"),
		browse: filepath.Join(tmp, "browse.sock"), control: filepath.Join(tmp, "ctl.sock"), port: freePort(t)}
	a, err := start(o)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{a: a, card: card, tmp: tmp, base: fmt.Sprintf("http://127.0.0.1:%d", a.port), done: make(chan struct{}), t: t}
	jar, _ := cookiejar.New(nil)
	r.hc = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	go func() { a.run(); close(r.done) }()
	t.Cleanup(func() { a.shutdown(); <-r.done })
	return r
}

// signIn opens the QR code's address, as the phone's camera does.
func (r *rig) signIn() {
	resp, err := r.hc.Get(r.base + "/?k=" + r.a.settings().Key)
	if err != nil || resp.StatusCode != 302 {
		r.t.Fatalf("key: %v %v", resp, err)
	}
	resp.Body.Close()
}

func (r *rig) do(method, path string, body io.Reader, hdr bool) (int, []byte, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.base+path, body)
	if hdr {
		req.Header.Set("X-Q2", "1")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// call is an API call with a JSON body (POST) or none (GET), its reply decoded.
func (r *rig) call(path string, body any) (int, map[string]any) {
	r.t.Helper()
	method, rd := "GET", io.Reader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		method, rd = "POST", bytes.NewReader(b)
	}
	code, b, _ := r.do(method, "/api/"+path, rd, true)
	var out map[string]any
	json.Unmarshal(b, &out)
	return code, out
}

func (r *rig) upload(dir, name, data string, overwrite bool) (int, map[string]any) {
	r.t.Helper()
	q := url.Values{"dir": {dir}, "name": {name}}
	if overwrite {
		q.Set("overwrite", "1")
	}
	code, b, _ := r.do("PUT", "/api/upload?"+q.Encode(), strings.NewReader(data), true)
	var out map[string]any
	json.Unmarshal(b, &out)
	return code, out
}

func names(out map[string]any) []string {
	var ns []string
	es, _ := out["entries"].([]any)
	for _, e := range es {
		m := e.(map[string]any)
		n := m["name"].(string)
		if m["dir"] == true {
			n += "/"
		}
		if p, ok := m["path"].(string); ok {
			n = p
		}
		ns = append(ns, n)
	}
	return ns
}

func (r *rig) read(p string) string {
	b, err := os.ReadFile(filepath.Join(r.card, p))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (r *rig) state() map[string]string {
	b, _ := os.ReadFile(filepath.Join(r.tmp, "state"))
	kv := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func (r *rig) ask(op string, args ...string) ([]string, [][]string) {
	r.t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ { // the socket comes a moment after the start, as on the Q2 ("Starting…")
		if c, err = net.Dial("unix", r.a.opts.browse); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer c.Close()
	line := "5\t" + op
	for _, a := range args {
		line += "\t" + q2spot.Escape(a)
	}
	c.Write([]byte("Q2SPOT/1\n" + line + "\n"))
	c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	fields := func(l string) []string {
		fs := strings.Split(l, "\t")
		for i := range fs {
			fs[i], _ = q2spot.Unescape(fs[i])
		}
		return fs
	}
	var rows [][]string
	for _, l := range lines[2:] {
		rows = append(rows, fields(l))
	}
	return fields(lines[1]), rows
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// The page is signed in by the QR code's key or the PIN; the two stay, kept in config.json; ten
// wrong PINs lock it a minute; a change needs X-Q2.
func TestSignin(t *testing.T) {
	r := newRig(t)
	set := r.a.settings()
	if len(set.Key) != 12 || len(set.PIN) != 6 || set.IdleMinutes != 15 || set.ReadOnly {
		t.Fatalf("settings %+v", set)
	}
	var saved settings
	b, _ := os.ReadFile(filepath.Join(r.card, ".filebrowser/config.json"))
	if json.Unmarshal(b, &saved); saved != set {
		t.Fatalf("saved %+v", saved)
	}
	if again := loadSettings(filepath.Join(r.card, ".filebrowser")); again != set {
		t.Fatalf("not kept: %+v", again)
	}
	st := r.state()
	if st["pin"] != set.PIN || st["version"] != version || st["note"] == "" ||
		(st["addr"] != "" && (st["qr"] == "" || st["qrsize"] == "0" || !strings.HasSuffix(st["addr"], fmt.Sprint(":", r.a.port)))) {
		t.Fatalf("state %v", st)
	}
	if code, _ := r.call("list?path=/", nil); code != 401 {
		t.Fatalf("unsigned list: %d", code)
	}
	if code, _, _ := r.do("POST", "/api/login", strings.NewReader(`{"pin":"`+set.PIN+`"}`), false); code != 400 {
		t.Fatalf("login without X-Q2: %d", code)
	}
	for i := 0; i < 10; i++ {
		if code, _ := r.call("login", map[string]string{"pin": "000000x"}); code != 403 {
			t.Fatalf("wrong pin %d: %d", i, code)
		}
	}
	if code, _ := r.call("login", map[string]string{"pin": set.PIN}); code != 429 {
		t.Fatalf("locked: %d", code)
	}
	r.signIn() // the QR code's key still works while the PIN is locked
	if code, out := r.call("state", nil); code != 200 || out["version"] != version || out["read_only"] != false {
		t.Fatalf("state %d %v", code, out)
	}
	if code, _, _ := r.do("POST", "/api/mkdir", strings.NewReader(`{"path":"/x"}`), false); code != 400 {
		t.Fatalf("change without X-Q2: %d", code)
	}
	// A PIN typed on another browser signs it in too.
	jar, _ := cookiejar.New(nil)
	other := &rig{base: r.base, hc: &http.Client{Jar: jar}, t: t}
	if code, _ := other.call("login", map[string]string{"pin": set.PIN}); code != 429 { // still locked from above
		t.Fatalf("other: %d", code)
	}
}

func TestPIN(t *testing.T) {
	r := newRig(t)
	if code, _ := r.call("login", map[string]string{"pin": " " + r.a.settings().PIN + " "}); code != 200 {
		t.Fatalf("pin: %d", code)
	}
	if code, _ := r.call("list?path=/", nil); code != 200 {
		t.Fatalf("signed in by pin: %d", code)
	}
}

// Folders first, dot files only when asked, nothing outside the card.
func TestList(t *testing.T) {
	r := newRig(t)
	r.signIn()
	code, out := r.call("list?path=/", nil)
	if got := strings.Join(names(out), ","); code != 200 || out["path"] != "/" || got != "Music/,page.html,song.mp3" {
		t.Fatalf("root %d %v", code, out)
	}
	if _, out = r.call("list?path=/&hidden=1", nil); strings.Join(names(out), ",") != ".deezer/,.filebrowser/,Music/,page.html,song.mp3" {
		t.Fatalf("hidden %v", names(out))
	}
	_, out = r.call("list?path="+url.QueryEscape("/Music/Broadcast/"), nil)
	es := out["entries"].([]any)
	if out["path"] != "/Music/Broadcast" || len(es) != 2 || es[1].(map[string]any)["size"] != float64(12) {
		t.Fatalf("album %v", out)
	}
	// ../ can't climb out: it is the card's /etc, which isn't there.
	if code, out := r.call("list?path="+url.QueryEscape("../../../etc"), nil); code != 404 || !strings.Contains(out["error"].(string), "isn't on the card") {
		t.Fatalf("escape %d %v", code, out)
	}
	if code, _, _ := r.do("GET", "/api/file?path="+url.QueryEscape("/../../card/song.mp3"), nil, false); code != 404 {
		t.Fatalf("file escape %d", code)
	}
	if code, out := r.call("search?path=/&q=VALERIE", nil); code != 200 || strings.Join(names(out), ",") != "/Music/Broadcast/02 Valerie.flac" {
		t.Fatalf("search %d %v", code, out)
	}
	if _, out := r.call("search?path=/&q=arl", nil); len(names(out)) != 0 {
		t.Fatalf("hidden found %v", names(out))
	}
}

// A file comes whole or in ranges (a browser seeking in a song); an HTML file is only bytes.
func TestDownload(t *testing.T) {
	r := newRig(t)
	r.signIn()
	code, b, h := r.do("GET", "/api/file?path="+url.QueryEscape("/Music/Broadcast/02 Valerie.flac"), nil, false)
	if code != 200 || string(b) != "fLaC two two" || h.Get("Content-Type") != "audio/flac" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("file %d %q %v", code, b, h)
	}
	req, _ := http.NewRequest("GET", r.base+"/api/file?inline=1&path=/song.mp3", nil)
	req.Header.Set("Range", "bytes=3-5")
	resp, err := r.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "ID3" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("range %d %q", resp.StatusCode, b)
	}
	if _, _, h = r.do("GET", "/api/file?inline=1&path=/page.html", nil, false); h.Get("Content-Type") != "application/octet-stream" ||
		h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("html %v", h)
	}
	if code, _, _ = r.do("GET", "/api/file?path=/Music", nil, false); code != 400 {
		t.Fatalf("folder as file %d", code)
	}
	if r.a.files.busy() {
		t.Fatal("a download left as moving")
	}
}

func TestZip(t *testing.T) {
	r := newRig(t)
	r.signIn()
	code, b, h := r.do("GET", "/api/zip?path=/Music/Broadcast&path=/song.mp3", nil, false)
	if code != 200 || h.Get("Content-Type") != "application/zip" || !strings.Contains(h.Get("Content-Disposition"), "Q2 files.zip") {
		t.Fatalf("zip %d %v", code, h)
	}
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range z.File {
		got = append(got, fmt.Sprintf("%s %d", f.Name, f.Method))
		if f.Name == "Broadcast/01 Pendulum.flac" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if string(data) != "fLaC one" {
				t.Fatalf("data %q", data)
			}
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "Broadcast/ 0,Broadcast/01 Pendulum.flac 0,Broadcast/02 Valerie.flac 0,song.mp3 0" {
		t.Fatalf("entries %q", got)
	}
	if _, _, h = r.do("GET", "/api/zip?path=/Music/Broadcast", nil, false); !strings.Contains(h.Get("Content-Disposition"), "Broadcast.zip") {
		t.Fatalf("one folder's name %v", h)
	}
	q := url.Values{"path": {"/Music/Broadcast/01 Pendulum.flac", "/Music/Broadcast/02 Valerie.flac"}}
	if _, _, h = r.do("GET", "/api/zip?"+q.Encode(), nil, false); !strings.Contains(h.Get("Content-Disposition"), "Broadcast.zip") {
		t.Fatalf("their folder's name %v", h)
	}
	if code, _, _ := r.do("GET", "/api/zip?path=/nope", nil, false); code != 404 {
		t.Fatalf("missing %d", code)
	}
}

func TestChanges(t *testing.T) {
	r := newRig(t)
	r.signIn()
	// Folders.
	if code, out := r.call("mkdir", map[string]string{"path": "/Audiobooks/Dune"}); code != 200 || out["path"] != "/Audiobooks/Dune" {
		t.Fatalf("mkdir %d %v", code, out)
	}
	if code, out := r.call("mkdir", map[string]string{"path": "/Audiobooks"}); code != 409 || !strings.Contains(out["error"].(string), "folder") {
		t.Fatalf("mkdir again %d %v", code, out)
	}
	if code, _ := r.call("mkdir", map[string]string{"path": "/Bad:Name"}); code != 400 {
		t.Fatalf("mkdir bad name %d", code)
	}
	// Uploads: a folder's files keep their folders; a name taken is numbered, unless replaced.
	if code, out := r.upload("/Music", "Stereolab/01 French Disko.flac", "disko", false); code != 200 ||
		out["path"] != "/Music/Stereolab/01 French Disko.flac" || r.read("Music/Stereolab/01 French Disko.flac") != "disko" {
		t.Fatalf("upload %d %v", code, out)
	}
	if code, out := r.upload("/", "song.mp3", "new", false); code != 200 || out["path"] != "/song (2).mp3" || r.read("song (2).mp3") != "new" {
		t.Fatalf("taken %d %v", code, out)
	}
	if code, out := r.upload("/", "song.mp3", "replaced", true); code != 200 || out["path"] != "/song.mp3" || r.read("song.mp3") != "replaced" {
		t.Fatalf("overwrite %d %v", code, out)
	}
	if code, out := r.upload("/", "../../escape.txt", "x", false); code != 200 || out["path"] != "/escape.txt" {
		t.Fatalf("escape %d %v", code, out)
	}
	if code, _ := r.upload("/nowhere", "a.txt", "x", false); code != 404 {
		t.Fatalf("no folder %d", code)
	}
	big := strings.Repeat("0123456789abcdef", 70000) // past one 512 KB chunk
	if code, _ := r.upload("/", "big.bin", big, false); code != 200 || r.read("big.bin") != big {
		t.Fatalf("big %d", code)
	}
	if ps, _ := filepath.Glob(filepath.Join(r.card, "*"+partExt)); len(ps) > 0 {
		t.Fatalf("parts left %v", ps)
	}
	// Rename, a case change included (FAT doesn't tell case apart); a name taken, or not a name.
	if code, out := r.call("rename", map[string]string{"path": "/song (2).mp3", "name": "Song.MP3"}); code != 200 ||
		out["path"] != "/Song.MP3" || r.read("Song.MP3") != "new" {
		t.Fatalf("rename %d %v", code, out)
	}
	if code, _ := r.call("rename", map[string]string{"path": "/Song.MP3", "name": "song.mp3"}); code != 200 {
		t.Fatalf("case only %d", code)
	}
	if code, out := r.call("rename", map[string]string{"path": "/big.bin", "name": "Music"}); code != 409 || !strings.Contains(out["error"].(string), "already") {
		t.Fatalf("taken %d %v", code, out)
	}
	if code, _ := r.call("rename", map[string]string{"path": "/big.bin", "name": "a/b"}); code != 400 {
		t.Fatalf("slash %d", code)
	}
	if code, _ := r.call("rename", map[string]string{"path": "/", "name": "x"}); code != 400 {
		t.Fatalf("the card %d", code)
	}
	// Move: into a folder, numbered there if taken; a folder not into itself.
	if code, out := r.call("move", map[string]any{"paths": []string{"/big.bin", "/escape.txt"}, "to": "/Audiobooks"}); code != 200 ||
		fmt.Sprint(out["paths"]) != "[/Audiobooks/big.bin /Audiobooks/escape.txt]" || r.read("Audiobooks/big.bin") != big {
		t.Fatalf("move %d %v", code, out)
	}
	if code, _ := r.call("move", map[string]any{"paths": []string{"/Audiobooks"}, "to": "/Audiobooks/Dune"}); code != 400 {
		t.Fatalf("into itself %d", code)
	}
	r.upload("/Music", "escape.txt", "other", false)
	if _, out := r.call("move", map[string]any{"paths": []string{"/Music/escape.txt"}, "to": "/Audiobooks"}); fmt.Sprint(out["paths"]) != "[/Audiobooks/escape (2).txt]" {
		t.Fatalf("numbered %v", out)
	}
	// Delete, a folder with what it holds; never the card.
	if code, _ := r.call("delete", map[string]any{"paths": []string{"/Audiobooks", "/song (2).mp3x"}}); code != 404 || r.read("Audiobooks/big.bin") != big {
		t.Fatalf("one missing %d: none deleted", code)
	}
	if code, _ := r.call("delete", map[string]any{"paths": []string{"/Audiobooks"}}); code != 200 || r.read("Audiobooks/big.bin") == big {
		t.Fatalf("delete %d", code)
	}
	if code, _ := r.call("delete", map[string]any{"paths": []string{"/"}}); code != 400 {
		t.Fatalf("the card %d", code)
	}
	// Read only (from the Q2): looking and downloading, nothing else.
	if head, _ := r.ask("readonly"); head[1] != "ok" || head[2] != "Read only" || !r.a.settings().ReadOnly {
		t.Fatalf("readonly %q", head)
	}
	if code, out := r.call("mkdir", map[string]string{"path": "/x"}); code != 403 || !strings.Contains(out["error"].(string), "Read only") {
		t.Fatalf("read only mkdir %d", code)
	}
	if code, _ := r.upload("/", "a.txt", "x", false); code != 403 {
		t.Fatalf("read only upload %d", code)
	}
	if code, out := r.call("state", nil); code != 200 || out["read_only"] != true {
		t.Fatalf("state %v", out)
	}
	if code, _ := r.call("list?path=/", nil); code != 200 {
		t.Fatalf("read only list %d", code)
	}
	r.ask("readonly")
	// The card lent to USB storage: nothing reaches it.
	cardReady = func() bool { return false }
	if code, _ := r.call("list?path=/", nil); code != 503 {
		t.Fatalf("no card %d", code)
	}
	cardReady = func() bool { return true }
}

// The Q2's page: where to open it (its code), the card's room, what moves, Read Only, Turn Off.
func TestDevice(t *testing.T) {
	r := newRig(t)
	head, rows := r.ask("menu")
	if head[1] != "ok" || head[2] != "refresh" || len(rows) != 4 {
		t.Fatalf("menu %q %q", head, rows)
	}
	pin := r.a.settings().PIN
	if rows[0][0] != "remote" || rows[0][2] != "Open on Your Phone" ||
		(r.a.addr() != "" && rows[0][3] != "http://"+r.a.addr()+" · PIN "+pin[:3]+" "+pin[3:]) {
		t.Fatalf("remote %q", rows[0])
	}
	if rows[1][0] != "info" || !strings.HasPrefix(rows[1][2], "Card: ") || rows[2][1] != "readonly" || rows[2][2] != "Read Only: Off" ||
		rows[3][1] != "stop" || rows[3][2] != "Turn Off File Browser" || !strings.Contains(rows[3][3], "15 minutes") {
		t.Fatalf("rows %q", rows)
	}
	// Just turned on, the Q2 is held awake for the phone to scan the code.
	waitFor(t, "the start's hold", 3*time.Second, func() bool { return r.state()["hold"] == "1" })
	r.a.mu.Lock()
	r.a.webAt = time.Time{}
	r.a.mu.Unlock()
	waitFor(t, "no hold", 3*time.Second, func() bool { return r.state()["hold"] == "0" })
	// A transfer under way: what, and the Q2 held awake.
	tr := r.a.files.begin(true, "01 Pendulum.flac", 200)
	r.a.files.progress(tr, 50)
	if _, rows = r.ask("menu"); rows[1][2] != "01 Pendulum.flac" || rows[1][3] != "receiving 1 file · 25%" {
		t.Fatalf("moving %q", rows[1])
	}
	waitFor(t, "the hold", 3*time.Second, func() bool { return r.state()["hold"] == "1" })
	r.a.files.end(tr)
	waitFor(t, "the hold gone", 3*time.Second, func() bool { return r.state()["hold"] == "0" })
	// The browser in use holds it too.
	r.signIn()
	waitFor(t, "the browser's hold", 3*time.Second, func() bool { return r.state()["hold"] == "1" })
	if head, _ := r.ask("caps"); head[2] != version {
		t.Fatalf("caps %q", head)
	}
	if head, _ := r.ask("nope"); head[1] != "unsupported" {
		t.Fatalf("unknown %q", head)
	}
	// Turn Off: the reply first, then it ends, its files gone.
	if head, _ := r.ask("stop"); head[2] != "File Browser off" {
		t.Fatalf("stop %q", head)
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("still running")
	}
	if _, err := os.Stat(filepath.Join(r.tmp, "state")); !os.IsNotExist(err) {
		t.Fatal("state file left")
	}
}

// Unused for IdleMinutes, it turns itself off; a transfer keeps it on.
func TestIdle(t *testing.T) {
	r := newRig(t)
	tr := r.a.files.begin(true, "a", 0)
	r.a.mu.Lock()
	r.a.used = time.Now().Add(-time.Hour)
	r.a.mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatal("stopped during a transfer")
	default:
	}
	r.a.files.end(tr)
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("not stopped when idle")
	}
}

// TestDemo serves a folder as the card, to look at the page without a Q2:
//
//	Q2_DEMO=/some/folder go test -run TestDemo -timeout 0 ./cmd/q2-filebrowser
//
// It prints the address that signs a browser in (and the PIN), and serves until stopped.
func TestDemo(t *testing.T) {
	card := os.Getenv("Q2_DEMO")
	if card == "" {
		t.Skip("Q2_DEMO not set")
	}
	cardReady = func() bool { return true }
	tmp := t.TempDir()
	a, err := start(options{dir: filepath.Join(card, ".filebrowser"), card: card, statusFile: filepath.Join(tmp, "state"),
		browse: filepath.Join(tmp, "browse.sock"), control: filepath.Join(tmp, "ctl.sock"), port: 8000})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("http://127.0.0.1:%d/?k=%s  (PIN %s)\n", a.port, a.settings().Key, a.settings().PIN)
	a.run()
}
