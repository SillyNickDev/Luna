// Package luna is the Go SDK for Luna plugins: the same bot object as the Python SDK,
// in Go's idioms. Every call is checked by Luna against the plugin's plugin.toml.
//
//	bot := luna.New()
//	bot.On("twitch.channel.follow", func(c *luna.Ctx, ev *luna.Event) error {
//		return bot.Chat.Send(c, "Welcome to the pack, "+ev.User().DisplayName()+"!")
//	})
//	bot.Run()
//
// shortcut: no links or wait_for yet; they come with the full Go SDK in Phase 5.
package luna

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"luna/internal/plugin/pb"
)

const (
	API = "1"
	SDK = "luna-go/0.1.0"
)

// Event is a bus event; Data is the payload object.
type Event struct {
	ID        string
	Type      string
	Source    string
	TS        time.Time
	Simulated bool
	Data      json.RawMessage
}

// Decode reads the payload into v.
func (e *Event) Decode(v any) error { return json.Unmarshal(e.Data, v) }

// User is the event's user, or nil if it has none.
func (e *Event) User() *User {
	var d struct {
		User *User `json:"user"`
	}
	json.Unmarshal(e.Data, &d)
	return d.User
}

// Str and Int read one top-level payload field, empty or 0 if missing.
func (e *Event) Str(k string) string {
	var d map[string]any
	json.Unmarshal(e.Data, &d)
	s, _ := d[k].(string)
	return s
}

func (e *Event) Int(k string) int64 {
	var d map[string]any
	json.Unmarshal(e.Data, &d)
	n, _ := d[k].(float64)
	return int64(n)
}

func (e *Event) Bool(k string) bool {
	var d map[string]any
	json.Unmarshal(e.Data, &d)
	b, _ := d[k].(bool)
	return b
}

type User struct {
	ID    string   `json:"id"`
	Login string   `json:"login"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
	Rank  string   `json:"rank"` // chat messages: viewer, moderator, luna-mod, head-mod, broadcaster
}

// DisplayName falls back to the login when Twitch gave no display name.
func (u *User) DisplayName() string {
	if u == nil {
		return ""
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Login
}

var ranks = []string{"viewer", "moderator", "luna-mod", "head-mod", "broadcaster"}

// RankAtLeast compares Luna ranks; with no rank stamped, Twitch badges decide.
func (u *User) RankAtLeast(rank string) bool {
	if u == nil {
		return false
	}
	have := slices.Index(ranks, u.Rank)
	if have < 0 {
		have = 0
		switch {
		case slices.Contains(u.Roles, "broadcaster"):
			have = 4
		case slices.Contains(u.Roles, "moderator"), slices.Contains(u.Roles, "lead_moderator"):
			have = 1
		}
	}
	return have >= slices.Index(ranks, rank)
}

func (u *User) IsMod() bool { return u.RankAtLeast("moderator") }
func (u *User) Has(role string) bool {
	return u != nil && slices.Contains(u.Roles, role)
}

// Ctx is what every handler gets: a context cancelled on shutdown, plus the event
// that triggered it, so replies to simulated events stay in the simulator.
type Ctx struct {
	context.Context
	Event *Event
	bot   *Bot
}

// User is the triggering event's user.
func (c *Ctx) User() *User {
	if c.Event == nil {
		return nil
	}
	return c.Event.User()
}

func (c *Ctx) simulated() bool { return c.Event != nil && c.Event.Simulated }

// Reply answers the triggering chat message as a threaded reply.
func (c *Ctx) Reply(text string) error {
	id := ""
	if c.Event != nil {
		id = c.Event.Str("message_id")
	}
	return c.bot.Chat.send(c, &pb.ChatSendRequest{Text: text, ReplyTo: id})
}

// Errors, matched with errors.Is.
var (
	ErrPermissionDenied = errors.New("permission denied")
	ErrRateLimited      = errors.New("rate limited")
	ErrNotConnected     = errors.New("not connected")
	ErrTimeout          = errors.New("timed out")
	ErrConfig           = errors.New("bad config")
)

type HandlerOpt func(*handler)

// When filters events in the subscription instead of an if at the top of the handler.
func When(f func(*Event) bool) HandlerOpt { return func(h *handler) { h.when = f } }

// DropIfBusy skips events that arrive while the handler is still running.
func DropIfBusy() HandlerOpt { return func(h *handler) { h.dropIfBusy = true } }

type handler struct {
	pattern    string
	fn         func(*Ctx, *Event) error
	when       func(*Event) bool
	once       bool
	dropIfBusy bool
	done       bool
	q          chan *Event // live: one ordered queue per handler
}

func match(pattern, typ string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(typ, p)
	}
	return pattern == typ
}

// Arg is a CLI or chat command argument; Type is int, float, str or bool.
type Arg struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

type CLICommand struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Args    []Arg  `json:"args,omitempty"`
	Flags   []Arg  `json:"flags,omitempty"`
	Mutates bool   `json:"mutates,omitempty"`
}

// Args holds a CLI command's values, already checked against its declaration by Luna.
type Args map[string]any

func (a Args) Str(k string) string { s, _ := a[k].(string); return s }
func (a Args) Int(k string) int64  { n, _ := a[k].(float64); return int64(n) }
func (a Args) Bool(k string) bool  { b, _ := a[k].(bool); return b }
func (a Args) Has(k string) bool   { _, ok := a[k]; return ok }

type ChatCommand struct {
	Name         string
	Aliases      []string
	Capability   string // checked by Luna before the plugin sees the command
	Cooldown     string // e.g. "10s", global
	UserCooldown string
}

type cliCmd struct {
	CLICommand
	fn func(*Ctx, Args) (any, error)
}

type chatCmd struct {
	ChatCommand
	fn func(*Ctx, []string) error
}

// Bot is the plugin. Make one with New, declare handlers and commands, then Run.
type Bot struct {
	Chat    Chat
	Mod     Mod
	Store   Store
	Twitch  Twitch
	Viewers Viewers
	Log     Log
	Timers  Timers
	Links   Links
	OSC     OSC
	HR      HR
	Spotify Spotify
	// HTTP reaches the hosts in [capsule].net, HTTPS only. Tests may swap it for a fake.
	HTTP *http.Client

	CrashOnError bool   // tests: a handler error panics instead of being logged
	ID           string // set once connected
	Dev          bool   // running under luna capsule dev

	host     pb.HostClient
	now      func() time.Time
	handlers []*handler
	cli      map[string]*cliCmd
	chat     map[string]*chatCmd
	config   any // pointer to the plugin's settings struct
	onStart  []func(*Ctx) error
	onStop   []func(*Ctx) error
	ctx      context.Context
	provides map[string]provider
	uses     []string
	linkCall func(use string, args any) ([]byte, error)

	tmu     sync.Mutex
	timers  timerHeap
	seq     uint64
	wake    chan struct{}
	started bool
	pending []*timer // declared before the bot started; scheduled when it does
}

func New() *Bot {
	b := &Bot{cli: map[string]*cliCmd{}, chat: map[string]*chatCmd{}, now: time.Now, ctx: context.Background(),
		wake: make(chan struct{}, 1), provides: map[string]provider{}}
	b.Chat, b.Mod, b.Store, b.Twitch, b.Viewers, b.Log, b.Timers = Chat{b}, Mod{b}, Store{b}, Twitch{b}, Viewers{b}, Log{b}, Timers{b}
	b.Links, b.OSC, b.HR, b.HTTP, b.Spotify = Links{b}, OSC{b}, HR{b}, newHTTP(), Spotify{b}
	b.linkCall = (&liveLinks{conns: map[string]linkConn{}}).call
	return b
}

// On handles events whose type matches pattern ("twitch.channel.follow" or "twitch.*").
func (b *Bot) On(pattern string, fn func(*Ctx, *Event) error, opts ...HandlerOpt) {
	h := &handler{pattern: pattern, fn: fn}
	for _, o := range opts {
		o(h)
	}
	b.handlers = append(b.handlers, h)
}

// Once is On for the first matching event only.
func (b *Bot) Once(pattern string, fn func(*Ctx, *Event) error, opts ...HandlerOpt) {
	b.On(pattern, fn, opts...)
	b.handlers[len(b.handlers)-1].once = true
}

// CLI declares `luna <plugin> <name>`. A returned map or slice prints as a table (or JSON
// with --json); a string prints as is.
func (b *Bot) CLI(c CLICommand, fn func(*Ctx, Args) (any, error)) {
	b.cli[c.Name] = &cliCmd{c, fn}
}

// ChatCommand declares !name; fn gets the words after the command.
func (b *Bot) ChatCommand(c ChatCommand, fn func(*Ctx, []string) error) {
	b.chat[c.Name] = &chatCmd{c, fn}
}

// UseConfig loads the plugin's settings into the struct cfg points to. Exported fields
// of type int, int64, float64, string and bool become [config] entries, named by their
// `luna:"name"` tag; the values cfg holds now are the defaults.
func (b *Bot) UseConfig(cfg any) {
	if _, err := configSchema(cfg); err != nil {
		panic(err)
	}
	b.config = cfg
}

func (b *Bot) OnStart(fn func(*Ctx) error) { b.onStart = append(b.onStart, fn) }
func (b *Bot) OnStop(fn func(*Ctx) error)  { b.onStop = append(b.onStop, fn) }

// Now is the plugin's clock: real time live, the fake clock in tests.
func (b *Bot) Now() time.Time { return b.now() }

// Emit publishes plugin.<id>.<name> with data as its payload object.
func (b *Bot) Emit(c *Ctx, name string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = b.host.Emit(c, &pb.EmitRequest{Name: name, DataJson: raw})
	return wrap(err)
}

var configTypes = map[reflect.Kind]string{reflect.Int: "int", reflect.Int64: "int", reflect.Float64: "float",
	reflect.String: "str", reflect.Bool: "bool", reflect.Slice: "list"}

func configSchema(cfg any) (map[string]map[string]any, error) {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return nil, errors.New("luna: UseConfig wants a pointer to a struct")
	}
	v = v.Elem()
	out := map[string]map[string]any{}
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		name := f.Tag.Get("luna")
		if name == "" || !f.IsExported() {
			continue
		}
		typ, ok := configTypes[f.Type.Kind()]
		if ok && typ == "list" && f.Type.Elem().Kind() != reflect.Struct {
			ok = false
		}
		if !ok {
			return nil, fmt.Errorf("luna: config field %s is a %s; use int, float64, string, bool or a slice of structs", f.Name, f.Type)
		}
		def := v.Field(i).Interface()
		if typ == "list" && v.Field(i).Len() == 0 {
			def = []any{}
		}
		out[name] = map[string]any{"type": typ, "default": def}
	}
	return out, nil
}

// loadConfig applies Luna's effective config (JSON keyed by tag) onto the settings struct.
func (b *Bot) loadConfig(raw []byte) error {
	if b.config == nil || len(raw) == 0 {
		return nil
	}
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vals); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	v := reflect.ValueOf(b.config).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Tag.Get("luna")
		if r, ok := vals[name]; ok && name != "" {
			if err := json.Unmarshal(r, v.Field(i).Addr().Interface()); err != nil {
				return fmt.Errorf("%w: %s: %v", ErrConfig, name, err)
			}
		}
	}
	return nil
}

// Describe is what luna capsule dev writes into plugin.toml.
func (b *Bot) Describe() map[string]any {
	events := []string{}
	for _, h := range b.handlers {
		if !slices.Contains(events, h.pattern) {
			events = append(events, h.pattern)
		}
	}
	slices.Sort(events)
	cli := []CLICommand{}
	for _, c := range b.cli {
		cli = append(cli, c.CLICommand)
	}
	slices.SortFunc(cli, func(a, b CLICommand) int { return strings.Compare(a.Name, b.Name) })
	chat := []map[string]any{}
	for _, c := range b.chat {
		m := map[string]any{"name": c.Name, "aliases": append([]string{}, c.Aliases...)}
		for k, v := range map[string]string{"capability": c.Capability, "cooldown": c.Cooldown, "user_cooldown": c.UserCooldown} {
			if v != "" {
				m[k] = v
			}
		}
		chat = append(chat, m)
	}
	slices.SortFunc(chat, func(a, b map[string]any) int { return strings.Compare(a["name"].(string), b["name"].(string)) })
	cfg := map[string]map[string]any{}
	if b.config != nil {
		cfg, _ = configSchema(b.config)
	}
	provides := slices.Sorted(maps.Keys(b.provides))
	uses := slices.Sorted(slices.Values(b.uses))
	return map[string]any{"events": events, "cli": cli, "chat": chat, "config": cfg,
		"links": map[string][]string{"provides": append([]string{}, provides...), "uses": append([]string{}, uses...)}}
}

// call runs plugin code with the triggering event in context. Errors and panics are
// logged with the event and the plugin keeps running, unless CrashOnError.
func (b *Bot) call(ev *Event, fn func(*Ctx) error) (err error) {
	c := &Ctx{Context: b.ctx, Event: ev, bot: b}
	defer func() {
		if r := recover(); r != nil {
			if b.CrashOnError {
				panic(r)
			}
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
		if err != nil && !b.CrashOnError {
			where := ""
			if ev != nil {
				where = " handling " + ev.Type + " " + ev.ID
			}
			b.Log.Error(c, "error"+where+": "+err.Error())
		}
	}()
	err = fn(c)
	if err != nil && b.CrashOnError {
		panic(err)
	}
	return err
}

// answer runs a command: its error is the answer the user sees, not a crash. Panics
// still are.
func (b *Bot) answer(ev *Event, fn func(*Ctx) error) (err error) {
	c := &Ctx{Context: b.ctx, Event: ev, bot: b}
	defer func() {
		if r := recover(); r != nil {
			if b.CrashOnError {
				panic(r)
			}
			err = fmt.Errorf("the command crashed: %v", r)
			b.Log.Error(c, fmt.Sprintf("panic in a command: %v\n%s", r, debug.Stack()))
		}
	}()
	return fn(c)
}

// dispatch hands ev to every handler that wants it, inline (tests) or onto its queue.
func (b *Bot) dispatch(ev *Event, inline bool) {
	for _, h := range b.handlers {
		if h.done || !match(h.pattern, ev.Type) || h.when != nil && !h.when(ev) {
			continue
		}
		if h.once {
			h.done = true
		}
		if inline {
			b.call(ev, func(c *Ctx) error { return h.fn(c, ev) })
			continue
		}
		if h.dropIfBusy && len(h.q) > 0 {
			continue
		}
		select {
		case h.q <- ev:
		default:
			log.Printf("handler for %s is behind; dropped %s %s", h.pattern, ev.Type, ev.ID)
		}
	}
}

// command runs a CLI or chat command; it returns data to render, or text.
func (b *Bot) command(kind, name string, args []byte, ev *Event) (data any, err error) {
	switch kind {
	case "cli":
		cmd := b.cli[name]
		if cmd == nil {
			return nil, fmt.Errorf("no CLI command %q", name)
		}
		var a Args
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
		}
		var out any
		err := b.answer(nil, func(c *Ctx) (err error) { out, err = cmd.fn(c, a); return err })
		return out, err
	case "chat":
		cmd := b.chat[name]
		if cmd == nil {
			return nil, fmt.Errorf("no chat command %q", name)
		}
		var a struct {
			Args []string `json:"args"`
		}
		json.Unmarshal(args, &a)
		return nil, b.answer(ev, func(c *Ctx) error { return cmd.fn(c, a.Args) })
	}
	return nil, fmt.Errorf("unknown command kind %q", kind)
}

// Run connects to Luna and serves until told to stop. With LUNA_DESCRIBE=1 it prints
// the plugin's description for luna capsule dev instead.
func (b *Bot) Run() {
	if os.Getenv("LUNA_DESCRIBE") != "" {
		json.NewEncoder(os.Stdout).Encode(b.Describe())
		return
	}
	if os.Getenv("CAPSULE_CONTROL") == "" {
		fmt.Fprintln(os.Stderr, "this is a Luna plugin; run it with: luna capsule dev")
		os.Exit(2)
	}
	if err := b.runLive(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---------- timers ----------

type timer struct {
	due      time.Time
	seq      uint64
	interval time.Duration
	fn       func(*Ctx) error
	ev       *Event // the event that set it, so replies to a simulated one stay simulated
	dead     bool
}

type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	return h[i].due.Before(h[j].due) || h[i].due.Equal(h[j].due) && h[i].seq < h[j].seq
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)   { *h = append(*h, x.(*timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	*h = old[:len(old)-1]
	return t
}

// Timers run on the plugin's clock; the test harness's fake clock drives them instantly.
// shortcut: timers live in the capsule, not Luna, so they reset on restart; no cron yet.
type Timers struct{ b *Bot }

// Every runs fn every d, first after d. It returns a cancel func.
func (t Timers) Every(d time.Duration, fn func(*Ctx) error) func() {
	return t.b.schedule(d, d, fn, nil)
}

// After runs fn once after d, with c's triggering event (c may be nil). It returns a cancel func.
func (t Timers) After(c *Ctx, d time.Duration, fn func(*Ctx) error) func() {
	var ev *Event
	if c != nil {
		ev = c.Event
	}
	return t.b.schedule(d, 0, fn, ev)
}

func (b *Bot) schedule(delay, interval time.Duration, fn func(*Ctx) error, ev *Event) func() {
	if interval < 0 || interval > 0 && interval < time.Second {
		panic("luna: timer intervals are at least 1s")
	}
	b.tmu.Lock()
	b.seq++
	t := &timer{due: b.now().Add(delay), seq: b.seq, interval: interval, fn: fn, ev: ev}
	if b.started {
		heap.Push(&b.timers, t)
	} else {
		t.due = time.Time{}.Add(delay) // relative until start
		b.pending = append(b.pending, t)
	}
	b.tmu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return func() { b.tmu.Lock(); t.dead = true; b.tmu.Unlock() }
}

// start schedules the timers declared before the bot started, from now.
func (b *Bot) start() {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	b.timers, b.started = nil, true
	now := b.now()
	for _, t := range b.pending {
		t.due = now.Add(t.due.Sub(time.Time{})) // in place, so its cancel func still works
		heap.Push(&b.timers, t)
	}
	b.pending = nil
}

// runDue runs every timer that's due and returns how long until the next one (-1: none).
func (b *Bot) runDue() time.Duration {
	for {
		b.tmu.Lock()
		if len(b.timers) == 0 {
			b.tmu.Unlock()
			return -1
		}
		t := b.timers[0]
		if wait := t.due.Sub(b.now()); wait > 0 && !t.dead {
			b.tmu.Unlock()
			return wait
		}
		heap.Pop(&b.timers)
		if t.dead {
			b.tmu.Unlock()
			continue
		}
		if t.interval > 0 { // re-arm the same timer, so its cancel func still reaches it
			b.seq++
			t.due, t.seq = t.due.Add(t.interval), b.seq
			heap.Push(&b.timers, t)
		}
		fn, ev := t.fn, t.ev
		b.tmu.Unlock()
		b.call(ev, fn)
	}
}

// ---------- hooks for luna/sdk/go/lunatest; plugins don't call these ----------

// TestConnect points the bot at a host and clock, loads its config and runs OnStart.
func (b *Bot) TestConnect(host pb.HostClient, now func() time.Time, id string, config []byte) error {
	b.host, b.now, b.ID = host, now, id
	for _, h := range b.handlers {
		h.done = false
	}
	b.start()
	if err := b.loadConfig(config); err != nil {
		return err
	}
	for _, fn := range b.onStart {
		b.call(nil, fn)
	}
	return nil
}

func (b *Bot) TestDispatch(ev *Event) { b.dispatch(ev, true) }
func (b *Bot) TestCommand(kind, name string, args []byte, ev *Event) (any, error) {
	return b.command(kind, name, args, ev)
}
func (b *Bot) TestRunDue() time.Duration { return b.runDue() }
func (b *Bot) TestChatCommands() []ChatCommand {
	var out []ChatCommand
	for _, c := range b.chat {
		out = append(out, c.ChatCommand)
	}
	return out
}
