package main

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What the Q2 is playing, as near as File Browser can tell: the songs on the card another process
// holds open, by /proc/<pid>/fd's links. A song whose tags were written again since shows as
// "<path> (deleted)" there (the player still reads its old copy); the new one is at the same path.
// The links don't say which was opened last, so they come by name.

// devProcRoot is /proc; tests point it at a tree of their own.
var devProcRoot = "/proc"

// devPlayingMax is how many playing songs the tag editor offers.
const devPlayingMax = 3

// devPlaying is up to devPlayingMax songs open under root, the card, as card paths by name. It looks
// for at most half a second.
func devPlaying(root string) []string {
	root = filepath.Clean(root)
	pids, err := os.ReadDir(devProcRoot)
	if err != nil {
		return nil
	}
	self := strconv.Itoa(os.Getpid())
	end := time.Now().Add(500 * time.Millisecond)
	seen := map[string]bool{}
	var out []string
	for _, pid := range pids {
		if pid.Name() == self || !devDigits(pid.Name()) {
			continue
		}
		if time.Now().After(end) {
			break
		}
		dir := filepath.Join(devProcRoot, pid.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			to, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !filepath.IsAbs(to) {
				continue
			}
			to = filepath.Clean(strings.TrimSuffix(to, " (deleted)"))
			rel, err := filepath.Rel(root, to)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			p := "/" + filepath.ToSlash(rel)
			if seen[p] || !devShown(p) || !isSong(path.Base(p)) {
				continue
			}
			if st, err := os.Stat(to); err != nil || st.IsDir() {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	if len(out) > devPlayingMax {
		out = out[:devPlayingMax]
	}
	return out
}

// devShown is whether the card path p is outside dot folders, as the browser shows the card.
func devShown(p string) bool {
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if hidden(part) {
			return false
		}
	}
	return true
}

func devDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
