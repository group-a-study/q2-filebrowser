package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/tags"
)

// The tag editor's routes for the page: GET tags (songs' tags, a folder's songs for the folder),
// POST tags (changes, made one song at a time), GET cover (a song's picture) and POST library (the
// Q2 asked to read every song's tags again).

const (
	apiMaxSongs  = 500     // songs one tags call takes
	apiTagsLimit = 6 << 20 // a tags POST's body: a cover comes as a data URL
)

// apiReadTags is a song's tags as GET tags reads them; tests hold it.
var apiReadTags = (*files).readTags

// apiTagExts are the extensions the tag editor takes (".flac"), for the page.
func apiTagExts() []string {
	e := tags.Exts()
	sort.Strings(e)
	return e
}

// getTags replies with the tags of each of paths, a folder standing for its songs (by name, not
// its folders'); each song once. The reply goes out a song at a time, as each is read, so a big
// folder isn't held whole; with more than one song it leaves out their lyrics, which the page shows
// for one song alone (and asks for that one by itself).
func (w *web) getTags(rw http.ResponseWriter, r *http.Request, paths []string) {
	f := w.a.files
	if len(paths) == 0 {
		refuse(rw, http.StatusBadRequest, "Nothing chosen")
		return
	}
	var songs []string
	seen := map[string]bool{}
	add := func(p string) {
		if _, clean, err := f.abs(p); err != nil || !seen[clean] {
			seen[clean] = true
			songs = append(songs, p)
		}
	}
	for _, p := range paths {
		full, _, err := f.abs(p)
		if st, serr := os.Stat(full); err == nil && serr == nil && st.IsDir() {
			in, err := f.songs(p)
			if err != nil {
				answer(rw, err)
				return
			}
			for _, s := range in {
				add(s)
			}
		} else {
			add(p)
		}
		if len(songs) > apiMaxSongs {
			refuse(rw, http.StatusBadRequest, "That's more than 500 songs: choose fewer at a time")
			return
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(http.StatusOK)
	io.WriteString(rw, `{"files":[`)
	enc := json.NewEncoder(rw)
	for i, p := range songs {
		if r.Context().Err() != nil {
			return // the page went away: nobody to read the rest
		}
		tf := apiReadTags(f, p)
		if tf.Path == "" { // not there: named as asked for, so the page knows which
			_, clean, err := f.abs(p)
			if err != nil {
				clean = p
			}
			tf.Path, tf.Name = clean, path.Base(clean)
		}
		if tf.Info != nil && len(songs) > 1 {
			delete(tf.Fields, tags.Lyrics)
		}
		if i > 0 {
			io.WriteString(rw, ",")
		}
		if enc.Encode(tf) != nil {
			return
		}
	}
	io.WriteString(rw, "]}\n")
}

// apiPicture is a picture sent as a data URL ("data:image/jpeg;base64,…"), its bytes decoded
// straight from the body, so a big one is held once more, not twice; null or "" is none.
type apiPicture []byte

func (p *apiPicture) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return errors.New("the picture isn't a data URL")
	}
	s := b[1 : len(b)-1]
	if bytes.IndexByte(s, '\\') >= 0 { // escaped ("\/"): the slow way
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = []byte(str)
	}
	if len(s) == 0 {
		return nil
	}
	head, data, ok := bytes.Cut(s, []byte(";base64,"))
	if !ok || !bytes.HasPrefix(head, []byte("data:")) {
		return errors.New("the picture isn't a data URL")
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(out, data)
	if err != nil {
		return err
	}
	*p = out[:n]
	return nil
}

// apiTagBody is a tags POST: the songs and what to change in each. Number makes the songs, in the
// order given, tracks 1 to N of N.
type apiTagBody struct {
	Paths         []string              `json:"paths"`
	Set           map[tags.Field]string `json:"set"`
	Picture       apiPicture            `json:"picture"`
	RemovePicture bool                  `json:"remove_picture"`
	Number        bool                  `json:"number"`
}

// apiTagFail is a song a tags POST didn't save, and why, with the status that would have been
// the reply's.
type apiTagFail struct {
	Path  string `json:"path"`
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// apiReadBody is r's body, of at most limit bytes: read at its length when that's known, so a
// picture isn't copied about as the buffer grows.
func apiReadBody(rw http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, fail(http.StatusRequestEntityTooLarge, "That's too much to send at once: pick a smaller picture")
	}
	body := http.MaxBytesReader(rw, r.Body, limit)
	var b []byte
	var err error
	if r.ContentLength > 0 {
		b = make([]byte, r.ContentLength)
		_, err = io.ReadFull(body, b)
	} else {
		b, err = io.ReadAll(body)
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return nil, fail(http.StatusRequestEntityTooLarge, "That's too much to send at once: pick a smaller picture")
	case err != nil:
		return nil, fail(http.StatusBadRequest, "bad body")
	}
	return b, nil
}

// saveTags makes a tags POST's changes to its songs, one after another, and replies with the songs
// saved and those not (and why). What's wrong with the changes themselves refuses them all first.
func (w *web) saveTags(rw http.ResponseWriter, r *http.Request) {
	f := w.a.files
	var body apiTagBody
	b, err := apiReadBody(rw, r, apiTagsLimit)
	if err == nil && json.Unmarshal(b, &body) != nil {
		err = fail(http.StatusBadRequest, "bad body")
	}
	if err != nil {
		answer(rw, err)
		return
	}
	switch {
	case len(body.Paths) == 0:
		refuse(rw, http.StatusBadRequest, "Nothing chosen")
		return
	case len(body.Paths) > apiMaxSongs:
		refuse(rw, http.StatusBadRequest, "That's more than 500 songs: choose fewer at a time")
		return
	}
	c := tags.Changes{Set: map[tags.Field]string{}, RemovePicture: body.RemovePicture}
	for k, v := range body.Set {
		c.Set[k] = v
	}
	if len(body.Picture) > 0 {
		c.Picture, c.RemovePicture = &tags.Picture{Data: body.Picture}, false
	}
	if body.Number {
		c.Set[tags.Track], c.Set[tags.TrackTotal] = "1", strconv.Itoa(len(body.Paths))
	}
	if c.Empty() {
		refuse(rw, http.StatusBadRequest, "Nothing to change")
		return
	}
	if err := tags.Check(c); err != nil {
		answer(rw, tagErr("", err))
		return
	}
	saved, failed := []string{}, []apiTagFail{}
	var stop *apiErr // the card gone, or File Browser turning off: the rest not tried
	// The songs are one batch, as a Save for All on the Q2 is: busy from the first to the last,
	// between songs too, so File Browser doesn't turn itself off halfway, nor Update Library ask.
	d, batch := w.a.devTags(), w.a.devBegin(body.Paths)
	for i, p := range body.Paths {
		clean := p
		if _, cl, err := f.abs(p); err == nil {
			clean = cl
		}
		// Between songs, not within one: a song is saved whole or left as it was.
		if stop == nil && f.stopped() {
			stop = &apiErr{http.StatusServiceUnavailable, "File Browser is turning off"}
		}
		if stop != nil {
			failed = append(failed, apiTagFail{clean, stop.msg, stop.code})
			continue
		}
		one := c
		if body.Number {
			one.Set = maps.Clone(c.Set)
			one.Set[tags.Track] = strconv.Itoa(i + 1)
		}
		d.step(batch, i, path.Base(clean), true)
		err := f.writeTags(p, one)
		d.step(batch, i, path.Base(clean), false)
		if err != nil {
			e := &apiErr{http.StatusInternalServerError, err.Error()}
			errors.As(err, &e)
			failed = append(failed, apiTagFail{clean, e.msg, e.code})
			if e.code == http.StatusServiceUnavailable {
				stop = e
			}
			continue
		}
		saved = append(saved, clean)
	}
	w.a.devEnd(batch, devResult{}) // before the reply: the page may ask for Update Library next
	reply(rw, http.StatusOK, map[string]any{"saved": saved, "failed": failed})
}

// cover sends the song at p's picture (its front cover, else its first), sandboxed as a file is,
// read from the song a piece at a time: a big cover isn't held.
func (w *web) cover(rw http.ResponseWriter, r *http.Request, p string) {
	full, clean, err := w.a.files.existing(p)
	if err != nil {
		answer(rw, err)
		return
	}
	name := path.Base(clean)
	pic, err := tags.OpenPicture(full)
	if err != nil {
		answer(rw, tagErr(name, err))
		return
	}
	if pic == nil {
		refuse(rw, http.StatusNotFound, name+" has no cover")
		return
	}
	defer pic.Close()
	typ := pic.MIME
	if typ != "image/jpeg" && typ != "image/png" {
		typ = "application/octet-stream"
	}
	rw.Header().Set("Content-Type", typ)
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(rw, r, "", time.Time{}, pic)
}

// library asks the Q2 to update its library, so its lists show the tags as they are now: while tags
// are being saved (a Save for All on the Q2, say), once they are, which later says.
func (w *web) library(rw http.ResponseWriter) {
	later, err := w.a.askLibrary()
	if err != nil {
		answer(rw, err)
		return
	}
	reply(rw, http.StatusOK, map[string]bool{"ok": true, "later": later})
}
