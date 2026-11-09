package tags

import (
	"encoding/binary"
	"io"
)

// WAV: a RIFF file of chunks ("fmt ", "data" with the audio, and others), read and written as AIFF
// is (aiff.go): the tags are an ID3v2 tag in an "id3 " (or "ID3 ") chunk, a new one added at the end
// as mutagen adds it. A LIST INFO chunk (INAM, IART…) is read when there's no ID3 tag and never
// changed. An RF64 WAV, whose sizes are in a ds64 chunk, is read but not written.

func init() { register(iffCodec{}, ".wav") }

// wavInfoIDs are the INFO items read, by the field they hold; ITRK before IPRT, both a track.
var wavInfoIDs = []struct {
	id string
	f  Field
}{{"INAM", Title}, {"IART", Artist}, {"IPRD", Album}, {"ICRD", Date}, {"IGNR", Genre},
	{"ICMT", Comment}, {"ITRK", Track}, {"IPRT", Track}}

// wavInfo are the fields the first LIST INFO chunk holds, and ["LIST"] when it holds one.
func wavInfo(r io.ReaderAt, chunks []iffChunk) (map[Field]string, []string) {
	out := map[Field]string{}
	for _, c := range chunks {
		if c.id != "LIST" || c.size < 4 || c.size > 64<<10 {
			continue
		}
		if kind, err := readAt(r, c.off+8, 4); err != nil || string(kind) != "INFO" {
			continue // another kind of list (adtl, …) is left unread
		}
		b, err := readAt(r, c.off+8, c.size)
		if err != nil {
			continue
		}
		items := map[string]string{}
		for b = b[4:]; len(b) >= 8; {
			// The size is checked before it becomes an int: the Q2's is 32 bits, so a size with its top
			// bit set would turn negative and pass.
			id, size := string(b[:4]), binary.LittleEndian.Uint32(b[4:8])
			if int64(size) > int64(len(b)-8) {
				break
			}
			n := int(size)
			if _, ok := items[id]; !ok {
				items[id] = iffText(b[8 : 8+n])
			}
			b = b[min(8+n+n&1, len(b)):]
		}
		for _, x := range wavInfoIDs {
			v := items[x.id]
			if v == "" || out[x.f] != "" {
				continue
			}
			if x.f == Track {
				n, of := splitNum(v)
				if n != "" {
					out[Track] = n
				}
				if of != "" {
					out[TrackTotal] = of
				}
				continue
			}
			out[x.f] = v
		}
		break
	}
	if len(out) == 0 {
		return out, nil
	}
	return out, []string{"LIST"}
}
