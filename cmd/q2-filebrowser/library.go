package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2"
)

// The Q2's library is stock's database, filled by Library → Update Local Music, which reads every
// song's tags again. A card app asks for that scan by writing /tmp/q2-<id>.library whole (q2-pod's
// patch/addon_library.c): a new sequence number, then "scan". Q2 Pod runs it once nothing is playing
// and answers in .library.done ("<seq> ok" or "<seq> error <why>"). Firmware that takes the request
// lists "library" in /tmp/q2-addon.caps.
var (
	capsFile    = "/tmp/q2-addon.caps"
	libraryFile = "/tmp/q2-filebrowser.library"
)

// hasCap is whether the firmware lists word among the card-app features it has.
func hasCap(word string) bool {
	b, err := os.ReadFile(capsFile)
	if err != nil {
		return false
	}
	for _, w := range strings.Fields(string(b)) {
		if w == word {
			return true
		}
	}
	return false
}

// libraryAvailable is whether this Q2 Pod takes a library update asked for by an app.
func libraryAvailable() bool { return hasCap("library") }

// requestLibrary asks the Q2 to update its library.
func requestLibrary() error {
	if !libraryAvailable() {
		return fail(501, "This Q2 Pod can't be asked from here: on the Q2, Library → Update Local Music")
	}
	seq := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := q2.WriteFile(libraryFile, []byte(seq+"\nscan\n")); err != nil {
		return fail(500, "Couldn't ask the Q2: %v", err)
	}
	return nil
}
