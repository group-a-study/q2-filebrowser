package main

import (
	"fmt"
	"time"

	"github.com/group-a-study/q2-filebrowser/internal/q2spot"
)

// The Q2's File Browser page (Q2 Pod's card app page, q2-pod's patch/addon.c): where to open it
// (its row opens the QR code), what is moving, the card's room, Read Only and Turn Off. Rows' kinds
// are as for Soulseek's page; "refresh" asks again every 2 s.

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// spaced is a PIN as the Q2's code page shows it: 548 001.
func spaced(pin string) string {
	if len(pin) == 6 {
		return pin[:3] + " " + pin[3:]
	}
	return pin
}

func (a *app) device(req q2spot.Request) q2spot.Reply {
	a.touch(false)
	set := a.settings()
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"menu", "readonly", "stop"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		open := q2spot.Row{Kind: "remote", Title: "Open on Your Phone",
			Subtitle: "http://" + a.addr() + " · PIN " + spaced(set.PIN)}
		if a.addr() == "" {
			open.Subtitle = "Connect the Q2 to Wi-Fi first"
		}
		rows := []q2spot.Row{open}
		free, total := a.files.usage()
		if ups, downs, name, done, sz := a.files.activity(); ups+downs > 0 {
			var what []string
			if ups > 0 {
				what = append(what, "receiving "+plural(ups, "file"))
			}
			if downs > 0 {
				what = append(what, "sending "+plural(downs, "file"))
			}
			sub := what[0]
			if len(what) > 1 {
				sub += " · " + what[1]
			}
			if sz > 0 {
				sub += fmt.Sprintf(" · %d%%", done*100/sz)
			}
			rows = append(rows, info(name, sub))
		} else if total > 0 {
			rows = append(rows, info("Card: "+size(free)+" free", "of "+size(total)))
		}
		ro := q2spot.Row{Kind: "action", URI: "readonly", Title: "Read Only: Off",
			Subtitle: "The phone can upload, rename, move and delete"}
		if set.ReadOnly {
			ro.Title, ro.Subtitle = "Read Only: On", "The phone can only look and download"
		}
		idle := "It stays on until you turn it off"
		if set.IdleMinutes > 0 {
			idle = fmt.Sprintf("It also turns off after %d minutes unused", set.IdleMinutes)
		}
		rows = append(rows, ro, q2spot.Row{Kind: "action", URI: "stop", Title: "Turn Off File Browser", Subtitle: idle})
		return q2spot.Reply{Message: "refresh", Rows: rows}
	case "readonly":
		on := !set.ReadOnly
		if err := a.update(func(s *settings) { s.ReadOnly = on }); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if on {
			return ok("Read only")
		}
		return ok("Changes allowed")
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); a.shutdown() }() // the reply first
		return ok("File Browser off")
	}
	return q2spot.Fail(q2spot.Unsupported, "Update File Browser on the card")
}
