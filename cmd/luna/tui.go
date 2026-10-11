package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// tui is `luna tui`: the daemon at a glance, driven by keyboard or mouse. It talks to the
// daemon over the control socket like every other command, so it can't do anything the
// CLI can't.
type tui struct {
	app  *tview.Application
	ctl  func(request) (string, error)
	stop chan struct{}

	status, detail, daemonLog, msg *tview.TextView
	caps, events, timeline         *tview.Table
	tabs                           *tview.TextView
	pages                          *tview.Pages
	say                            *tview.InputField

	refreshMu sync.Mutex // one refresh at a time, so ring positions aren't read twice
	selected  string     // capsule whose log is shown
	evAfter   int        // events already shown, from the daemon's ring
	logAfter  int
	timelineE []tlEntry
}

type capRow struct {
	ID, Version, State, Note string
}

type tlEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	User   string    `json:"user"`
	Detail string    `json:"detail"`
	Undone bool      `json:"undone"`
	Undo   any       `json:"undo"`
}

var tabNames = []string{"events", "timeline", "log"}

func newTUI(ctl func(request) (string, error)) *tui {
	t := &tui{app: tview.NewApplication(), ctl: ctl, stop: make(chan struct{})}
	box := func(p tview.Primitive, title string) {
		if b, ok := p.(interface{ SetBorder(bool) *tview.Box }); ok {
			b.SetBorder(true)
		}
		if b, ok := p.(interface{ SetTitle(string) *tview.Box }); ok {
			b.SetTitle(" " + title + " ")
		}
	}
	t.status = tview.NewTextView().SetDynamicColors(true)
	box(t.status, "Luna")

	t.caps = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	box(t.caps, "Capsules (click one)")
	t.caps.SetSelectionChangedFunc(func(row, _ int) {
		if cell := t.caps.GetCell(row, 0); row > 0 && cell != nil && cell.Text != "" && cell.Text != t.selected {
			t.selected = cell.Text
			t.detail.SetTitle(" Log: " + t.selected + " ")
			go t.refreshDetail()
		}
	})
	t.detail = tview.NewTextView().SetScrollable(true)
	box(t.detail, "Log")
	t.detail.SetChangedFunc(func() { t.app.Draw() })

	button := func(label string, f func()) *tview.Button {
		b := tview.NewButton(label).SetSelectedFunc(f)
		b.SetBackgroundColorActivated(tcell.ColorDarkCyan)
		return b
	}
	act := func(verb string) func() {
		return func() {
			if t.selected == "" {
				t.note("[yellow]click a capsule first")
				return
			}
			go t.run(request{Cmd: "capsule", Args: []string{verb, t.selected}}, verb+" "+t.selected)
		}
	}
	buttons := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(button("Restart", act("restart")), 0, 1, false).AddItem(nil, 1, 0, false).
		AddItem(button("Enable", act("enable")), 0, 1, false).AddItem(nil, 1, 0, false).
		AddItem(button("Disable", act("disable")), 0, 1, false)

	right := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(t.detail, 0, 1, false).AddItem(buttons, 1, 0, false)
	middle := tview.NewFlex().AddItem(t.caps, 0, 1, true).AddItem(right, 0, 1, false)

	t.events = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	box(t.events, "Events, newest first")
	t.timeline = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	box(t.timeline, "Moderation timeline (select one, then Undo)")
	undo := button("Undo selected", func() {
		row, _ := t.timeline.GetSelection()
		if row < 1 || row > len(t.timelineE) {
			t.note("[yellow]select a timeline entry first")
			return
		}
		e := t.timelineE[row-1]
		go t.run(request{Cmd: "plugin", Args: []string{"luna.core", "undo", strconv.FormatInt(e.ID, 10)}}, fmt.Sprintf("undo #%d", e.ID))
	})
	tlPage := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(t.timeline, 0, 1, false).AddItem(undo, 1, 0, false)
	t.daemonLog = tview.NewTextView().SetScrollable(true)
	box(t.daemonLog, "Daemon log")
	t.pages = tview.NewPages().AddPage("events", t.events, true, true).AddPage("timeline", tlPage, true, false).
		AddPage("log", t.daemonLog, true, false)

	t.tabs = tview.NewTextView().SetRegions(true).SetDynamicColors(true)
	fmt.Fprint(t.tabs, ` ["events"] Events [""]  ["timeline"] Timeline [""]  ["log"] Daemon log [""]   [gray](click a tab, or press 1/2/3; q quits)`)
	t.tabs.SetHighlightedFunc(func(added, _, _ []string) {
		if len(added) > 0 {
			t.pages.SwitchToPage(added[0])
		}
	})
	t.tabs.Highlight("events")

	t.say = tview.NewInputField().SetLabel(" Say in chat: ").SetFieldWidth(0)
	t.say.SetDoneFunc(func(key tcell.Key) {
		text := strings.TrimSpace(t.say.GetText())
		if key != tcell.KeyEnter || text == "" {
			return
		}
		t.say.SetText("")
		go t.run(request{Cmd: "chat", Args: []string{text}}, "chat")
	})
	t.msg = tview.NewTextView().SetDynamicColors(true)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t.status, 7, 0, false).
		AddItem(middle, 0, 1, true).
		AddItem(t.tabs, 1, 0, false).
		AddItem(t.pages, 0, 1, false).
		AddItem(t.say, 1, 0, false).
		AddItem(t.msg, 1, 0, false)
	t.app.SetRoot(root, true).EnableMouse(true)
	// tview calls any two clicks within half a second a double-click, even on different
	// spots, and buttons ignore those: clicking a capsule then Restart would do nothing.
	t.app.SetMouseCapture(func(ev *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
		if action == tview.MouseLeftDoubleClick {
			action = tview.MouseLeftClick
		}
		return ev, action
	})
	t.app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if t.app.GetFocus() == t.say { // typing a message: every key is text
			if ev.Key() == tcell.KeyEscape {
				t.app.SetFocus(t.caps)
			}
			return ev
		}
		switch ev.Rune() {
		case 'q':
			t.app.Stop()
			return nil
		case '1', '2', '3':
			t.tabs.Highlight(tabNames[ev.Rune()-'1'])
			return nil
		}
		if ev.Key() == tcell.KeyTab { // cycle: capsules → current tab → chat box
			switch t.app.GetFocus() {
			case t.caps:
				name, _ := t.pages.GetFrontPage()
				t.app.SetFocus(map[string]tview.Primitive{"events": t.events, "timeline": t.timeline, "log": t.daemonLog}[name])
			default:
				t.app.SetFocus(t.say)
			}
			return nil
		}
		return ev
	})
	return t
}

func (t *tui) note(s string) {
	t.app.QueueUpdateDraw(func() { t.msg.SetText(" " + s) })
}

// run sends a request and shows how it went on the message line.
func (t *tui) run(r request, what string) {
	out, err := t.ctl(r)
	if err != nil {
		t.note("[red]" + what + ": " + tview.Escape(err.Error()))
		return
	}
	if out = strings.TrimSpace(out); out == "" {
		out = "done"
	}
	t.note("[green]" + what + ": " + tview.Escape(firstLine(out)))
	t.refresh()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// loop refreshes every second until the app stops.
func (t *tui) loop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		t.refresh()
		select {
		case <-tick.C:
		case <-t.stop:
			return
		}
	}
}

// refresh fetches everything off the UI goroutine, then applies it in one draw.
func (t *tui) refresh() {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	status, serr := t.ctl(request{Cmd: "status"})
	var caps []capRow
	raw, cerr := t.ctl(request{Cmd: "capsule", Args: []string{"list", "--json"}})
	if cerr == nil {
		json.Unmarshal([]byte(raw), &caps)
	}
	evOut, _ := t.ctl(request{Cmd: "recent", Args: []string{"--after", strconv.Itoa(t.evAfter)}})
	logOut, _ := t.ctl(request{Cmd: "logs", Args: []string{"--after", strconv.Itoa(t.logAfter)}})
	var tl []tlEntry
	if out, err := t.ctl(request{Cmd: "plugin", Args: []string{"luna.core", "timeline", "--json"}}); err == nil {
		json.Unmarshal([]byte(out), &tl)
	}
	done := make(chan struct{})
	t.app.QueueUpdateDraw(func() {
		defer close(done)
		if serr != nil {
			t.status.SetText("[red]" + tview.Escape(serr.Error()) + "[-]\n\nStart it with: luna run (this screen reconnects by itself)")
		} else {
			t.status.SetText(tview.Escape(status))
		}
		t.fillCaps(caps)
		t.evAfter = t.addEvents(evOut, t.evAfter)
		t.logAfter = appendRing(t.daemonLog, logOut, t.logAfter)
		t.fillTimeline(tl)
	})
	<-done // the ring positions are updated before the next refresh reads them
	if t.selected != "" {
		t.refreshDetail()
	}
}

func (t *tui) refreshDetail() {
	out, err := t.ctl(request{Cmd: "capsule", Args: []string{"logs", t.selected}})
	if err != nil {
		out = err.Error()
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	out = strings.Join(lines[max(0, len(lines)-200):], "\n")
	t.app.QueueUpdateDraw(func() {
		if t.detail.GetText(false) != out+"\n" && t.detail.GetText(false) != out {
			t.detail.SetText(out).ScrollToEnd()
		}
	})
}

func header(tb *tview.Table, cols ...string) {
	for i, c := range cols {
		tb.SetCell(0, i, tview.NewTableCell(c).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}
}

func (t *tui) fillCaps(caps []capRow) {
	row, _ := t.caps.GetSelection()
	t.caps.Clear()
	header(t.caps, "Capsule", "Version", "State", "")
	for i, c := range caps {
		color := tcell.ColorGreen
		if c.State != "running" {
			color = tcell.ColorRed
		}
		t.caps.SetCell(i+1, 0, tview.NewTableCell(c.ID))
		t.caps.SetCell(i+1, 1, tview.NewTableCell(c.Version))
		t.caps.SetCell(i+1, 2, tview.NewTableCell(c.State).SetTextColor(color))
		t.caps.SetCell(i+1, 3, tview.NewTableCell(c.Note).SetTextColor(tcell.ColorGray))
	}
	if row < 1 {
		row = 1
	}
	t.caps.Select(min(row, max(len(caps), 1)), 0)
}

// addEvents prepends the new events from a ring reply ("total\nline\nline...").
func (t *tui) addEvents(out string, after int) int {
	total, lines := parseRing(out, after)
	if t.events.GetRowCount() == 0 {
		header(t.events, "Time", "Event", "Who", "What")
	}
	for _, l := range lines {
		var ev struct {
			Type      string          `json:"type"`
			TS        time.Time       `json:"ts"`
			Simulated bool            `json:"simulated"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil {
			continue
		}
		who, what := summarize(ev.Data)
		typ := ev.Type
		if ev.Simulated {
			typ += " (sim)"
		}
		t.events.InsertRow(1)
		t.events.SetCell(1, 0, tview.NewTableCell(ev.TS.Local().Format("15:04:05")).SetTextColor(tcell.ColorGray))
		t.events.SetCell(1, 1, tview.NewTableCell(typ))
		t.events.SetCell(1, 2, tview.NewTableCell(who).SetTextColor(tcell.ColorAqua))
		t.events.SetCell(1, 3, tview.NewTableCell(what).SetExpansion(1))
	}
	for t.events.GetRowCount() > 501 {
		t.events.RemoveRow(t.events.GetRowCount() - 1)
	}
	return total
}

// summarize picks the user and the interesting part of an event's data.
func summarize(raw json.RawMessage) (who, what string) {
	var d map[string]any
	json.Unmarshal(raw, &d)
	if u, ok := d["user"].(map[string]any); ok {
		who, _ = u["login"].(string)
		delete(d, "user")
	}
	if text, ok := d["text"].(string); ok {
		return who, text
	}
	var parts []string
	for _, k := range []string{"reward", "input", "bits", "viewers", "count", "months", "bpm", "game", "title", "name", "value"} {
		if v, ok := d[k]; ok && v != "" {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return who, strings.Join(parts, " ")
}

func parseRing(out string, after int) (int, []string) {
	head, rest, _ := strings.Cut(out, "\n")
	total, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil {
		return after, nil
	}
	if rest == "" {
		return total, nil
	}
	return total, strings.Split(rest, "\n")
}

func appendRing(v *tview.TextView, out string, after int) int {
	total, lines := parseRing(out, after)
	if len(lines) > 0 {
		fmt.Fprintln(v, tview.Escape(strings.Join(lines, "\n")))
		v.ScrollToEnd()
	}
	return total
}

func (t *tui) fillTimeline(tl []tlEntry) {
	row, _ := t.timeline.GetSelection()
	t.timelineE = tl
	t.timeline.Clear()
	header(t.timeline, "#", "When", "By", "Action", "User", "Detail")
	for i, e := range tl {
		color := tcell.ColorWhite
		if e.Undone {
			color = tcell.ColorGray
		}
		cells := []string{strconv.FormatInt(e.ID, 10), e.At.Local().Format("Jan 2 15:04"), e.Actor, e.Action, e.User, e.Detail}
		if e.Undone {
			cells[3] += " (undone)"
		}
		for j, c := range cells {
			t.timeline.SetCell(i+1, j, tview.NewTableCell(c).SetTextColor(color))
		}
	}
	if row >= 1 && row <= len(tl) {
		t.timeline.Select(row, 0)
	}
}

func (t *tui) runApp() error {
	go t.loop()
	defer close(t.stop)
	return t.app.Run()
}

func runTUI(p paths) error {
	return newTUI(func(r request) (string, error) { return callOut(p, r) }).runApp()
}
