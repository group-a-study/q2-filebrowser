package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const cookieName = "q2fb"

func init() {
	// The firmware has no mime.types: what a browser plays or shows, named.
	for ext, typ := range map[string]string{".mp3": "audio/mpeg", ".flac": "audio/flac", ".m4a": "audio/mp4",
		".m4b": "audio/mp4", ".aac": "audio/aac", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".oga": "audio/ogg",
		".wav": "audio/wav", ".aif": "audio/aiff", ".aiff": "audio/aiff", ".mp4": "video/mp4", ".m4v": "video/mp4",
		".txt": "text/plain; charset=utf-8", ".cue": "text/plain; charset=utf-8", ".lrc": "text/plain; charset=utf-8",
		".m3u": "text/plain; charset=utf-8", ".m3u8": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
		".ini": "text/plain; charset=utf-8", ".epub": "application/epub+zip", ".bmp": "image/bmp"} {
		mime.AddExtensionType(ext, typ)
	}
}

// web is the page: index.html and a JSON API behind the key the QR code carries (or the PIN on
// the Q2's screen), kept in a cookie. A change must carry X-Q2: 1, which a page elsewhere can't
// send here without asking first, and the cookie is SameSite=Strict.
type web struct {
	a *app

	mu      sync.Mutex
	fails   int
	lockout time.Time
}

func serveWeb(a *app, l net.Listener) {
	w := &web{a: a}
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/api/login", w.login)
	mux.HandleFunc("/api/", w.api)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	srv.Serve(l)
}

func (w *web) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.a.settings().Key)) == 1
}

func (w *web) setCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: w.a.settings().Key, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
}

func (w *web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	w.a.touch(true)
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(w.a.settings().Key)) == 1 {
			w.setCookie(rw)
		}
		http.Redirect(rw, r, "/", http.StatusFound)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Write(indexHTML)
}

func reply(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func refuse(rw http.ResponseWriter, code int, msg string) {
	reply(rw, code, map[string]string{"error": msg})
}

// answer replies with err's status and words, or 500.
func answer(rw http.ResponseWriter, err error) {
	var e *apiErr
	if errors.As(err, &e) {
		refuse(rw, e.code, e.msg)
		return
	}
	refuse(rw, http.StatusInternalServerError, err.Error())
}

// login takes the Q2's PIN; ten wrong ones lock it for a minute.
func (w *web) login(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	w.a.touch(true)
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1024)).Decode(&body)
	w.mu.Lock()
	locked := time.Now().Before(w.lockout)
	ok := !locked && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.PIN)), []byte(w.a.settings().PIN)) == 1
	if !ok && !locked {
		if w.fails++; w.fails >= 10 {
			w.fails, w.lockout = 0, time.Now().Add(time.Minute)
		}
	}
	if ok {
		w.fails = 0
	}
	w.mu.Unlock()
	switch {
	case locked:
		refuse(rw, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
	case !ok:
		refuse(rw, http.StatusForbidden, "That's not the PIN on the Q2's screen.")
	default:
		w.setCookie(rw)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	}
}

// changes are the routes that change the card, refused while read only.
var changes = map[string]bool{"upload": true, "mkdir": true, "rename": true, "move": true, "delete": true}

func (w *web) api(rw http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Q2") != "1" {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	if !w.authed(r) {
		refuse(rw, http.StatusUnauthorized, "sign in")
		return
	}
	w.a.touch(true)
	f := w.a.files
	q := r.URL.Query()
	set := w.a.settings()
	if route == "state" {
		free, total := f.usage()
		reply(rw, http.StatusOK, map[string]any{"version": version, "read_only": set.ReadOnly, "free": free,
			"total": total, "card": cardReady()})
		return
	}
	if !cardReady() {
		refuse(rw, http.StatusServiceUnavailable, "The card isn't there just now (USB storage?)")
		return
	}
	if changes[route] && set.ReadOnly {
		refuse(rw, http.StatusForbidden, "Read only: allow changes on the Q2 (Streaming → File Browser)")
		return
	}
	hiddenToo := q.Get("hidden") == "1"
	var body struct {
		Path  string   `json:"path"`
		Name  string   `json:"name"`
		Paths []string `json:"paths"`
		To    string   `json:"to"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 256<<10)).Decode(&body); err != nil {
			refuse(rw, http.StatusBadRequest, "bad body")
			return
		}
	}
	post := r.Method == http.MethodPost
	switch {
	case route == "list" && r.Method == http.MethodGet:
		entries, clean, err := f.list(q.Get("path"), hiddenToo)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]any{"path": clean, "entries": entries})
	case route == "search" && r.Method == http.MethodGet:
		found, err := f.search(q.Get("path"), q.Get("q"), hiddenToo)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]any{"entries": found, "more": len(found) >= maxFound})
	case route == "file" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.file(rw, r, q.Get("path"), q.Get("inline") == "1")
	case route == "zip" && r.Method == http.MethodGet:
		w.zip(rw, q["path"])
	case route == "upload" && r.Method == http.MethodPut:
		size := r.ContentLength
		if size < 0 {
			size = -1
		}
		p, err := f.upload(q.Get("dir"), q.Get("name"), size, q.Get("overwrite") == "1", r.Body)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]string{"path": p})
	case route == "mkdir" && post:
		p, err := f.mkdir(body.Path)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]string{"path": p})
	case route == "rename" && post:
		p, err := f.rename(body.Path, body.Name)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]string{"path": p})
	case route == "move" && post:
		moved, err := f.move(body.Paths, body.To)
		if err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]any{"paths": moved})
	case route == "delete" && post:
		if err := f.remove(body.Paths); err != nil {
			answer(rw, err)
			return
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(rw, r)
	}
}

// file sends one file, with ranges (so a browser can seek in a song), as a download unless inline.
// Whatever it is, the browser runs nothing of it: the response is sandboxed.
func (w *web) file(rw http.ResponseWriter, r *http.Request, p string, inline bool) {
	f := w.a.files
	full, clean, err := f.existing(p)
	if err != nil {
		answer(rw, err)
		return
	}
	in, err := os.Open(full)
	if err != nil {
		answer(rw, fail(500, "Couldn't open it: %v", err))
		return
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || info.IsDir() {
		answer(rw, fail(400, "That's a folder"))
		return
	}
	name := path.Base(clean)
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	rw.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	rw.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	if typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); typ == "" || strings.Contains(typ, "html") ||
		strings.Contains(typ, "xml") || strings.Contains(typ, "javascript") {
		rw.Header().Set("Content-Type", "application/octet-stream")
	}
	t := f.begin(false, name, info.Size())
	defer f.end(t)
	http.ServeContent(rw, r, name, info.ModTime(), &counter{f: f, t: t, r: in})
}

// zip sends paths as one ZIP, named after the one, or the folder they are in.
func (w *web) zip(rw http.ResponseWriter, paths []string) {
	f := w.a.files
	if len(paths) == 0 {
		refuse(rw, http.StatusBadRequest, "Nothing chosen")
		return
	}
	// One item's own name; several, their folder's (the card's: Q2 files).
	name, dirs := "Q2 files", map[string]bool{}
	for _, p := range paths {
		_, clean, err := f.existing(p)
		if err != nil {
			answer(rw, err)
			return
		}
		dirs[path.Dir(clean)] = true
		if len(paths) == 1 {
			name = path.Base(clean)
		}
	}
	for d := range dirs {
		if len(paths) > 1 && len(dirs) == 1 && d != "/" {
			name = path.Base(d)
		}
	}
	rw.Header().Set("Content-Type", "application/zip")
	rw.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	if err := f.zip(paths, rw); err != nil {
		// The headers are gone: a ZIP cut short is what the browser gets, as for a dropped download.
		log.Printf("zip: %v", err)
	}
}

// size is n bytes as the Q2's page and the browser show it.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}
