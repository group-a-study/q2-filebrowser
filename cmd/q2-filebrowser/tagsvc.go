package main

import (
	"errors"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/group-a-study/q2-filebrowser/internal/tags"
)

// The tag editor's side of the card, for the phone's page and the Q2's alike: which files it takes,
// reading a song's tags, and writing them as a transfer the Q2's page shows (which keeps the Q2 awake).

func init() {
	tags.CardReady = func() bool { return cardReady() }
}

// isSong is whether the tag editor takes name.
func isSong(name string) bool { return !hidden(name) && tags.Supported(name) }

// songs are the songs in the folder p, by name: not its folders' or dot files. Paths are the card's.
func (f *files) songs(p string) ([]string, error) {
	full, clean, err := f.abs(p)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail(404, "That folder isn't on the card")
		}
		return nil, fail(500, "Couldn't read the folder: %v", err)
	}
	var out []string
	for _, d := range des {
		if !d.IsDir() && isSong(d.Name()) {
			out = append(out, path.Join(clean, d.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, nil
}

// tagFile is one song's tags as the page gets them; Error is why they couldn't be read.
type tagFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	*tags.Info
	Error string `json:"error,omitempty"`
}

// readTags is the song at p's tags.
func (f *files) readTags(p string) tagFile {
	var tf tagFile
	if _, clean, err := f.abs(p); err == nil {
		tf.Path, tf.Name = clean, path.Base(clean)
	}
	full, _, err := f.existing(p)
	if err != nil {
		tf.Error = err.Error()
		return tf
	}
	info, err := tags.Read(full)
	if err != nil {
		tf.Error = tagErr(tf.Name, err).Error()
		return tf
	}
	if n := tags.PictureNote(info.Picture); n != "" {
		info.Notes = append(info.Notes, n)
	}
	if info.Fields == nil {
		info.Fields = map[tags.Field]string{}
	}
	tf.Info = info
	return tf
}

// writeTags makes c's changes to the song at p.
func (f *files) writeTags(p string, c tags.Changes) error {
	full, clean, err := f.existing(p)
	if err != nil {
		return err
	}
	name := path.Base(clean)
	if !tags.Supported(full) {
		return fail(415, "File Browser can't change the tags of %s", name)
	}
	if !cardReady() {
		return fail(503, "The card isn't there just now (USB storage?)")
	}
	if f.stopped() {
		return fail(503, "File Browser is turning off")
	}
	f.writers.Add(1)
	defer f.writers.Done()
	t := f.beginTags(name)
	defer f.end(t)
	if err := tags.Write(full, c); err != nil {
		return tagErr(name, err)
	}
	return nil
}

// tagErr is a tags failure as the page says it.
func tagErr(name string, err error) error {
	switch {
	case errors.Is(err, tags.ErrUnsupported):
		return fail(415, "File Browser can't read the tags of %s", name)
	case errors.Is(err, tags.ErrReadOnly):
		return fail(415, "%s's tags can be read here, not changed", name)
	case errors.Is(err, tags.ErrNoRoom):
		return fail(507, "Not enough room on the card to save %s", name)
	case errors.Is(err, tags.ErrNoCard):
		return fail(503, "The card went away (USB storage?)")
	case errors.Is(err, tags.ErrStopped):
		return fail(503, "File Browser turned off before %s was saved; it is as it was", name)
	case errors.Is(err, tags.ErrTooBig):
		return fail(413, "%s is too big to change here", name)
	case errors.Is(err, tags.ErrCorrupt):
		return fail(422, "%s isn't laid out as its format says, so it was left as it was", name)
	case errors.Is(err, os.ErrNotExist):
		return fail(404, "%s isn't on the card any more", name)
	case errors.Is(err, os.ErrPermission):
		return fail(403, "The card won't let %s be changed (is it write-protected?)", name)
	case strings.HasPrefix(err.Error(), "tags: "):
		msg := strings.TrimPrefix(err.Error(), "tags: ")
		return fail(400, "%s", strings.ToUpper(msg[:1])+msg[1:])
	}
	// The words without the card's path on disk, which means nothing to the page.
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fail(500, "Couldn't save %s: %v", name, err)
}
