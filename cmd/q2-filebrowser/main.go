// q2-filebrowser is a Q2 Pod card app (contrib/shanlingq2/README), after KOReader's Filebrowser
// plugin: turned on from the Q2, it serves a file manager for the card to a phone or computer on
// the same Wi-Fi, opened from the QR code (or the address and PIN) the Q2 shows. Browse, upload
// files and folders, download (a folder as a ZIP), play, rename, move and delete; turned off from
// the Q2, or by itself once unused. It runs from the card's .filebrowser folder (copied to /tmp
// first, so the card is never kept busy) and draws the Q2's File Browser page over Q2SPOT/1.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2"
	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
	"github.com/group-a-study/q2-filebrowser/internal/qr"
)

const version = "q2-v1.0"

// settings is .filebrowser/config.json.
type settings struct {
	Key         string `json:"web_key"`      // the QR code's: signs a browser in
	PIN         string `json:"pin"`          // the Q2's screen's, typed with the address
	ReadOnly    bool   `json:"read_only"`    // the browser may only look and download
	IdleMinutes int    `json:"idle_minutes"` // stop by itself after this long unused (0: never)
}

type options struct {
	dir, card, statusFile, browse, control string
	port                                   int
}

type app struct {
	opts  options
	state *q2.State
	files *files
	port  int
	ln    net.Listener

	mu     sync.Mutex
	set    settings
	used   time.Time // the browser or the Q2's page, last
	webAt  time.Time // the browser, last
	ip     string
	qrFor  string
	qrHex  string
	qrSize int
	quit   chan struct{}
	once   sync.Once
}

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "/mnt/mmc/.filebrowser", "the add-on's folder on the card")
	flag.StringVar(&o.card, "card", "/mnt/mmc", "the card's mount point: what the browser sees")
	flag.StringVar(&o.statusFile, "status-file", "/tmp/q2-filebrowser.state", "the status file Q2 Pod reads")
	flag.StringVar(&o.browse, "browse-socket", "/tmp/q2-filebrowser.browse.sock", "Q2SPOT/1 for the Q2's page")
	flag.StringVar(&o.control, "control-socket", "/tmp/q2-filebrowser.sock", "datagrams: q quits")
	flag.IntVar(&o.port, "port", 8000, "the page's port (the next free one if taken)")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(32 << 20)
	q2.Card = o.card
	q2.SetupLog("q2-filebrowser")
	a, err := start(o)
	if err != nil {
		log.Fatal(err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; a.shutdown() }()
	a.run()
	os.Exit(0)
}

// loadSettings reads config.json, making the key and PIN the first time; they stay, so a phone
// signed in once stays signed in.
func loadSettings(dir string) settings {
	s := settings{IdleMinutes: 15}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	dirty := false
	if len(s.Key) < 12 {
		s.Key, dirty = q2.Key(12), true
	}
	if len(s.PIN) != 6 {
		s.PIN, dirty = q2.PIN(6), true
	}
	if s.IdleMinutes < 0 {
		s.IdleMinutes = 0
	}
	if dirty {
		saveSettings(dir, s)
	}
	return s
}

func saveSettings(dir string, s settings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return q2.WriteFile(filepath.Join(dir, "config.json"), b)
}

func (a *app) settings() settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set
}

func (a *app) update(fn func(*settings)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.set
	fn(&s)
	if err := saveSettings(a.opts.dir, s); err != nil {
		return err
	}
	a.set = s
	return nil
}

// touch notes a use: web is the browser's (which holds standby off a while).
func (a *app) touch(web bool) {
	a.mu.Lock()
	a.used = time.Now()
	if web {
		a.webAt = a.used
	}
	a.mu.Unlock()
}

// listen takes port, or the next free of the nine after it.
func listen(port int) (net.Listener, int) {
	for p := port; p < port+10; p++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			return l, p
		}
	}
	return nil, 0
}

func start(o options) (*app, error) {
	os.MkdirAll(o.dir, 0o755)
	// Turned on counts as the page used: the Q2 stays awake while the phone scans the code.
	a := &app{opts: o, state: &q2.State{Path: o.statusFile}, used: time.Now(), webAt: time.Now(), quit: make(chan struct{})}
	a.set = loadSettings(o.dir)
	a.files = &files{root: o.card}
	if a.ln, a.port = listen(o.port); a.ln == nil {
		return nil, fmt.Errorf("no port for the page from %d", o.port)
	}
	go serveWeb(a, a.ln)
	a.writeState() // the code there before the browse socket answers: Q2 Pod opens it then
	go func() {
		if err := q2spot.Serve(o.browse, a.device); err != nil {
			log.Printf("browse socket: %v", err)
		}
	}()
	if _, err := q2.Control(o.control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			a.shutdown()
		}
	}); err != nil {
		log.Printf("control socket: %v", err)
	}
	log.Printf("%s serving %s on port %d", version, o.card, a.port)
	return a, nil
}

func (a *app) shutdown() { a.once.Do(func() { close(a.quit) }) }

// run writes the status file every second and stops by itself once unused for IdleMinutes.
func (a *app) run() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			a.ln.Close()
			a.files.cancel() // uploads under way end, their .part files gone
			a.state.Remove()
			os.Remove(a.opts.browse)
			os.Remove(a.opts.control)
			log.Printf("stopped")
			return
		case <-t.C:
			a.writeState()
			s := a.settings()
			a.mu.Lock()
			idle := time.Since(a.used)
			a.mu.Unlock()
			if s.IdleMinutes > 0 && !a.files.busy() && idle > time.Duration(s.IdleMinutes)*time.Minute {
				log.Printf("idle: stopping")
				a.shutdown()
			}
		}
	}
}

// URL is the page's address, and with the key the one the QR code opens (signed in at once); ""
// without Wi-Fi.
func (a *app) URL(withKey bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ip == "" {
		return ""
	}
	u := fmt.Sprintf("http://%s:%d/", a.ip, a.port)
	if withKey {
		u += "?k=" + a.set.Key
	}
	return u
}

// addr is the address as the Q2 shows it: no scheme, no slash.
func (a *app) addr() string {
	u := a.URL(false)
	if u == "" {
		return ""
	}
	return u[len("http://") : len(u)-1]
}

// hold is whether the Q2 should stay awake: files moving, or the browser used (or File Browser
// turned on) in the last five minutes (standby would take the Wi-Fi from under it).
func (a *app) hold() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.files.busy() || time.Since(a.webAt) < 5*time.Minute
}

// writeState is what the Q2 reads: the page's address, PIN and QR code (addon.c's qr_open), and
// whether to hold standby off.
func (a *app) writeState() {
	ip := q2.LocalIP()
	a.mu.Lock()
	a.ip = ip
	a.mu.Unlock()
	url := a.URL(true)
	a.mu.Lock()
	if url != a.qrFor {
		a.qrFor, a.qrHex, a.qrSize = url, "", 0
		if url != "" {
			if c, err := qr.Encode([]byte(url)); err == nil {
				a.qrHex, a.qrSize = c.Hex(), c.Size
			}
		}
	}
	qrHex, qrSize, pin := a.qrHex, a.qrSize, a.set.PIN
	a.mu.Unlock()
	note := "Manage the card's files there"
	if ip == "" {
		note = "Connect the Q2 to Wi-Fi"
	}
	hold := "0"
	if a.hold() {
		hold = "1"
	}
	a.state.Write(map[string]string{"addr": a.addr(), "pin": pin, "qr": qrHex, "qrsize": fmt.Sprint(qrSize),
		"note": note, "hold": hold, "version": version})
}
