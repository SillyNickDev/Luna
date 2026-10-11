package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The dashboard against a fake daemon, on a simulated terminal: it shows what the daemon
// reports, and mouse clicks on rows, buttons and tabs send the right requests.
func TestTUI(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	ctl := func(r request) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Cmd + " " + strings.Join(r.Args, " ")
		switch {
		case r.Cmd == "status":
			return "daemon    up 5m\ntwitch    connected as winterpaw", nil
		case key == "capsule list --json":
			return `[{"id":"luna.core","version":"0.1.0","state":"running"},{"id":"luna.songs","version":"0.1.0","state":"failed","note":"last failure: crash"}]`, nil
		case strings.HasPrefix(key, "recent"):
			if strings.HasSuffix(key, "--after 0") {
				return "1\n" + `{"type":"twitch.chat.message","ts":"2026-10-11T20:00:00Z","data":{"user":{"login":"fox"},"text":"hello luna"}}`, nil
			}
			return "1\n", nil
		case strings.HasPrefix(key, "logs"):
			return "0\n", nil
		case key == "plugin luna.core timeline --json":
			return `[{"id":7,"at":"2026-10-11T20:01:00Z","actor":"mink","action":"timeout","user":"spammer","undo":{}}]`, nil
		case key == "capsule logs luna.songs":
			return "luna: plugin API connected\npanic: oops", nil
		}
		sent = append(sent, key)
		return "ok", nil
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	ui := newTUI(ctl)
	ui.app.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- ui.runApp() }()
	defer func() { ui.app.Stop(); <-done }()
	time.Sleep(100 * time.Millisecond) // tview re-inits the screen when it starts, which resets its size
	screen.SetSize(140, 40)
	ui.app.QueueUpdateDraw(func() {})

	text := func() string {
		var cells []tcell.SimCell
		var w int
		got := make(chan struct{})
		ui.app.QueueUpdate(func() { cells, w, _ = screen.GetContents(); close(got) }) // on tview's goroutine, not mid-draw
		<-got
		var b strings.Builder
		for i, c := range cells {
			if len(c.Runes) > 0 {
				b.WriteRune(c.Runes[0])
			} else {
				b.WriteByte(' ')
			}
			if (i+1)%w == 0 {
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	waitFor := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(text(), what); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never showed %q:\n%s", what, text())
			}
		}
	}
	click := func(label string) {
		t.Helper()
		waitFor(label)
		lines := strings.Split(text(), "\n")
		for y, l := range lines {
			if x := strings.Index(l, label); x >= 0 {
				x = len([]rune(l[:x])) + 1
				screen.InjectMouse(x, y, tcell.Button1, tcell.ModNone)
				screen.InjectMouse(x, y, tcell.ButtonNone, tcell.ModNone)
				return
			}
		}
	}
	sentWith := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			got := strings.Join(sent, "|")
			mu.Unlock()
			if strings.Contains(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("never sent %q (sent %q)", want, got)
			}
		}
	}

	waitFor("connected as winterpaw")
	waitFor("hello luna") // the events tab
	waitFor("last failure: crash")
	click("last failure: crash") // the luna.songs row
	waitFor("panic: oops")       // its log, on the right
	click("Restart")
	sentWith("capsule restart luna.songs")
	click("Timeline")
	waitFor("spammer")
	click("spammer")
	click("Undo selected")
	sentWith("plugin luna.core undo 7")
}
