package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeFLAC makes a small FLAC at full (its folders too) whose comments are list ("TITLE=One"), with
// 1 KB of padding and a few bytes standing in for the audio: enough for the tag editor.
func writeFLAC(t *testing.T, full string, list ...string) {
	t.Helper()
	var cm bytes.Buffer
	str := func(s string) {
		binary.Write(&cm, binary.LittleEndian, uint32(len(s)))
		cm.WriteString(s)
	}
	str("test")
	binary.Write(&cm, binary.LittleEndian, uint32(len(list)))
	for _, e := range list {
		str(e)
	}
	var b bytes.Buffer
	b.WriteString("fLaC")
	b.Write([]byte{0, 0, 0, 34})
	b.Write(make([]byte, 34))
	b.Write([]byte{4, byte(cm.Len() >> 16), byte(cm.Len() >> 8), byte(cm.Len())})
	b.Write(cm.Bytes())
	b.Write([]byte{0x81, 0, 4, 0})
	b.Write(make([]byte, 1024))
	b.Write([]byte{0xFF, 0xF8, 0x69, 0x08, 1, 2, 3, 4})
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
