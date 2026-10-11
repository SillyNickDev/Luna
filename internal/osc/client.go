package osc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"luna/internal/event"
)

const (
	ParamPrefix = "/avatar/parameters/"
	frame       = time.Second / 30 // outgoing parameters and outgoing events, coalesced per address per frame
	chatboxGap  = 2 * time.Second  // VRChat drops chatbox messages sent faster than about this
	ChatboxMax  = 144
)

// noisy are VRChat's built-in parameters that change many times a second; they stay in
// the cache but aren't published as events.
var noisy = map[string]bool{"VelocityX": true, "VelocityY": true, "VelocityZ": true, "VelocityMagnitude": true,
	"AngularY": true, "Upright": true, "Voice": true, "Viseme": true, "GestureLeftWeight": true, "GestureRightWeight": true,
	"EyeHeightAsMeters": true, "EyeHeightAsPercent": true, "ScaleFactor": true, "ScaleFactorInverse": true}

// Param is one avatar parameter from VRChat's OSC config for the current avatar.
type Param struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // Bool, Int, Float
	Writable bool   `json:"writable"` // has an input address, so Luna can set it
}

type Avatar struct {
	ID     string  `json:"id"`
	Name   string  `json:"name,omitempty"`
	Params []Param `json:"params"` // empty when Luna can't find the avatar's OSC config
}

// Client is the OSC integration: send and receive VRChat's parameters.
type Client struct {
	send      *net.UDPConn
	recv      *net.UDPConn // nil when the receive port is taken
	configDir string
	publish   func(event.Event)

	mu       sync.Mutex
	out      map[string]any // address → latest value, flushed each frame
	order    []string
	chatbox  *string // the latest chatbox text waiting for its slot
	nextChat time.Time
	params   map[string]any
	dirty    map[string]bool // changed since the last frame, to publish
	avatar   Avatar
	lastRecv time.Time
}

// Dial opens the send socket to sendAddr and, if it's free, listens on listenAddr.
func Dial(sendAddr, listenAddr, configDir string, publish func(event.Event)) (*Client, error) {
	ra, err := net.ResolveUDPAddr("udp", sendAddr)
	if err != nil {
		return nil, fmt.Errorf("osc.send: %w", err)
	}
	sc, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		return nil, fmt.Errorf("osc.send: %w", err)
	}
	c := &Client{send: sc, configDir: configDir, publish: publish, out: map[string]any{}, params: map[string]any{}, dirty: map[string]bool{}}
	if listenAddr != "" {
		la, err := net.ResolveUDPAddr("udp", listenAddr)
		if err == nil {
			c.recv, err = net.ListenUDP("udp", la)
		}
		if err != nil {
			slog.Warn("osc: can't listen, so avatar changes and parameters from VRChat won't be seen", "listen", listenAddr, "err", err)
		}
	}
	return c, nil
}

// Listening reports whether VRChat's messages reach Luna.
func (c *Client) Listening() bool { return c.recv != nil }

// LastHeard is when VRChat last sent anything; zero if never.
func (c *Client) LastHeard() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRecv
}

// Send queues value for address; only the latest value per address goes out each frame.
func (c *Client) Send(address string, value any) error {
	if _, err := (Message{address, []any{value}}).Encode(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.out[address]; !ok {
		c.order = append(c.order, address)
	}
	c.out[address] = value
	return nil
}

// Chatbox shows text in the VRChat chatbox, cut to its 144 characters. Messages closer
// together than VRChat allows are coalesced, latest wins.
func (c *Client) Chatbox(text string) {
	if utf8.RuneCountInString(text) > ChatboxMax {
		text = string([]rune(text)[:ChatboxMax-1]) + "…"
	}
	c.mu.Lock()
	c.chatbox = &text
	c.mu.Unlock()
}

// Param is VRChat's last reported value of an avatar parameter.
func (c *Client) Param(name string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.params[name]
	return v, ok
}

func (c *Client) Avatar() Avatar {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.avatar
}

func (c *Client) Close() {
	c.send.Close()
	if c.recv != nil {
		c.recv.Close()
	}
}

// Run flushes and receives until ctx ends.
func (c *Client) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	if c.recv != nil {
		go c.receive()
	}
	t := time.NewTicker(frame)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.flush(now)
		}
	}
}

func (c *Client) flush(now time.Time) {
	c.mu.Lock()
	var msgs []Message
	for _, a := range c.order {
		msgs = append(msgs, Message{a, []any{c.out[a]}})
	}
	c.out, c.order = map[string]any{}, nil
	if c.chatbox != nil && !now.Before(c.nextChat) {
		msgs = append(msgs, Message{"/chatbox/input", []any{*c.chatbox, true, false}}) // send now, no notification sound
		c.chatbox, c.nextChat = nil, now.Add(chatboxGap)
	}
	var evs []event.Event
	for name := range c.dirty {
		if ev, err := event.New("osc.param", "osc", false, map[string]any{"name": name, "value": c.params[name]}); err == nil {
			evs = append(evs, ev)
		}
	}
	clear(c.dirty)
	c.mu.Unlock()
	for _, m := range msgs {
		if b, err := m.Encode(); err == nil {
			c.send.Write(b)
		}
	}
	for _, ev := range evs {
		c.publish(ev)
	}
}

func (c *Client) receive() {
	buf := make([]byte, 65535)
	for {
		n, err := c.recv.Read(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("osc: receive stopped", "err", err)
			}
			return
		}
		msgs, err := Decode(buf[:n])
		if err != nil {
			continue // not ours to fix; VRChat and friends sometimes send odd packets
		}
		for _, m := range msgs {
			c.handle(m)
		}
	}
}

func (c *Client) handle(m Message) {
	c.mu.Lock()
	c.lastRecv = time.Now()
	c.mu.Unlock()
	if m.Address == "/avatar/change" && len(m.Args) == 1 {
		id, _ := m.Args[0].(string)
		a := c.loadAvatar(id)
		c.mu.Lock()
		c.avatar = a
		clear(c.params) // the new avatar's values arrive right after
		c.mu.Unlock()
		if ev, err := event.New("osc.avatar", "osc", false, map[string]any{"id": a.ID, "name": a.Name}); err == nil {
			c.publish(ev)
		}
		return
	}
	name, ok := strings.CutPrefix(m.Address, ParamPrefix)
	if !ok || name == "" || len(m.Args) != 1 || m.Args[0] == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, seen := c.params[name]; seen && old == m.Args[0] {
		return
	}
	c.params[name] = m.Args[0]
	if !noisy[name] {
		c.dirty[name] = true
	}
}

var avatarID = regexp.MustCompile(`^avtr_[0-9a-fA-F-]{36}$|^local:[0-9a-zA-Z_-]{1,64}$`)

// loadAvatar reads VRChat's OSC config for the avatar: <dir>/usr_*/Avatars/<id>.json.
func (c *Client) loadAvatar(id string) Avatar {
	a := Avatar{ID: id, Params: []Param{}}
	if c.configDir == "" || !avatarID.MatchString(id) { // the ID becomes a file name: no paths from the network
		return a
	}
	files, _ := filepath.Glob(filepath.Join(c.configDir, "usr_*", "Avatars", id+".json"))
	if len(files) == 0 {
		return a
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || len(raw) > 1<<20 {
		return a
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // VRChat writes a BOM
	var f struct {
		Name       string `json:"name"`
		Parameters []struct {
			Name  string `json:"name"`
			Input *struct {
				Type string `json:"type"`
			} `json:"input"`
			Output *struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("osc: can't read the avatar's OSC config", "file", files[0], "err", err)
		return a
	}
	a.Name = f.Name
	for _, p := range f.Parameters {
		pr := Param{Name: p.Name, Writable: p.Input != nil}
		switch {
		case p.Input != nil:
			pr.Type = p.Input.Type
		case p.Output != nil:
			pr.Type = p.Output.Type
		}
		a.Params = append(a.Params, pr)
	}
	return a
}

// DefaultConfigDir finds VRChat's OSC folder: under Proton on Linux, LocalLow on Windows.
func DefaultConfigDir() string {
	home, _ := os.UserHomeDir()
	var cands []string
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		cands = append(cands, filepath.Join(lad, "..", "LocalLow", "VRChat", "VRChat", "OSC"))
	}
	for _, steam := range []string{".steam/steam", ".local/share/Steam", ".var/app/com.valvesoftware.Steam/.local/share/Steam"} {
		cands = append(cands, filepath.Join(home, steam, "steamapps/compatdata/438100/pfx/drive_c/users/steamuser/AppData/LocalLow/VRChat/VRChat/OSC"))
	}
	for _, d := range cands {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return ""
}
