package luna

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"luna/internal/capsule"
)

// ErrLinkUnavailable: the provider isn't installed, isn't running, or the link wasn't approved.
var ErrLinkUnavailable = errors.New("link unavailable")

// Links are calls between plugins (Capsules spec, Control channel and links). Luna's
// broker sits in the middle, so neither side gets a path into the other.
type Links struct{ b *Bot }

type provider func(c *Ctx, caller string, args Args) (any, error)

// Provide answers calls on the link name ("spend" serves luna.points:spend; add it to
// [links].provides). caller is the calling plugin's ID, stamped by Luna so it can't be faked.
func (l Links) Provide(name string, fn func(c *Ctx, caller string, args Args) (any, error)) {
	l.b.provides[name] = fn
}

// Use declares links this plugin calls, as "provider:name" (also in [links].uses).
func (l Links) Use(uses ...string) {
	for _, u := range uses {
		if !slices.Contains(l.b.uses, u) {
			l.b.uses = append(l.b.uses, u)
		}
	}
}

// Call calls use ("luna.points:spend") with args and decodes the provider's answer into out (nil to ignore it).
func (l Links) Call(c *Ctx, use string, args any, out any) error {
	if !slices.Contains(l.b.uses, use) {
		return fmt.Errorf("%w: declare it first: bot.Links.Use(%q) and add it to [links].uses", ErrPermissionDenied, use)
	}
	raw, err := l.b.linkCall(use, args)
	if err != nil || out == nil || raw == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// linkError maps the broker's and provider's error strings onto the SDK's errors.
func linkError(use, e string) error {
	switch e {
	case "link_unavailable":
		return fmt.Errorf("%w: %s", ErrLinkUnavailable, use)
	case "timeout":
		return fmt.Errorf("%w: %s", ErrTimeout, use)
	case "rate_limited":
		return fmt.Errorf("%w: %s", ErrRateLimited, use)
	}
	return fmt.Errorf("%s: %s", use, e)
}

var anyMap = cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}

// cborToJSON re-encodes a link body as JSON, the form the rest of the SDK decodes.
func cborToJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dm, _ := anyMap.DecMode()
	var v any
	if err := dm.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// jsonToCBOR goes the other way, through a plain value so json tags are honoured.
func jsonToCBOR(v any) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var plain any
	if err := json.Unmarshal(j, &plain); err != nil {
		return nil, err
	}
	return cbor.Marshal(plain)
}

// liveLinks is the consumer side over $CAPSULE_LINKS: one connection per link, one call at a time on each.
type liveLinks struct {
	mu    sync.Mutex
	conns map[string]linkConn
	seq   uint64
}

type linkConn struct {
	nc net.Conn
	*capsule.Conn
}

func (ll *liveLinks) call(use string, args any) ([]byte, error) {
	body, err := jsonToCBOR(args)
	if err != nil {
		return nil, err
	}
	ll.mu.Lock()
	defer ll.mu.Unlock()
	conn, ok := ll.conns[use]
	if !ok {
		nc, err := net.Dial("unix", filepath.Join(os.Getenv("CAPSULE_LINKS"), use))
		if err != nil {
			return nil, fmt.Errorf("%w: %s isn't approved or isn't in [links].uses", ErrLinkUnavailable, use)
		}
		conn = linkConn{nc, capsule.NewConn(nc)}
		ll.conns[use] = conn
	}
	_, method, _ := capsule.SplitUse(use)
	ll.seq++
	drop := func() error {
		conn.nc.Close()
		delete(ll.conns, use)
		return fmt.Errorf("%w: %s: the link dropped", ErrLinkUnavailable, use)
	}
	if err := conn.Write("call", ll.seq, capsule.Call{Method: method, Body: body}); err != nil {
		return nil, drop()
	}
	conn.nc.SetReadDeadline(time.Now().Add(10 * time.Second)) // the broker times calls out at 5 s
	f, err := conn.Read()
	if err != nil {
		return nil, drop()
	}
	var r capsule.Result
	if err := capsule.Decode(f.B, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, linkError(use, r.Error)
	}
	return cborToJSON(r.Body)
}

// serveCall answers one call frame from the broker on the control channel.
func (b *Bot) serveCall(f capsule.Frame) capsule.Result {
	var lc capsule.LinkCall
	if err := capsule.Decode(f.B, &lc); err != nil {
		return capsule.Result{Error: "bad_call"}
	}
	j, err := cborToJSON(lc.Body)
	if err != nil {
		return capsule.Result{Error: "bad_call"}
	}
	out, err := b.provide(lc.Link, lc.Caller, j)
	if err != nil {
		return capsule.Result{Error: err.Error()}
	}
	body, err := jsonToCBOR(out)
	if err != nil {
		return capsule.Result{Error: err.Error()}
	}
	return capsule.Result{OK: true, Body: body}
}

// provide runs the provider for link with JSON args.
func (b *Bot) provide(link, caller string, args []byte) (out any, err error) {
	fn := b.provides[link]
	if fn == nil {
		return nil, errors.New("no such link")
	}
	a := Args{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
	}
	// Not b.call: a provider's error ("not enough points") is an answer for the caller, not a fault to log.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(&Ctx{Context: b.ctx, bot: b}, caller, a)
}

// TestLinks replaces the link transport: fake answers each call as the provider would.
func (b *Bot) TestLinks(fake func(use string, args Args) (any, error)) {
	b.linkCall = func(use string, args any) ([]byte, error) {
		j, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		a := Args{}
		json.Unmarshal(j, &a)
		out, err := fake(use, a)
		if err != nil || out == nil {
			return nil, err
		}
		return json.Marshal(out)
	}
}

// TestProvide calls this plugin's provider for link as caller would.
func (b *Bot) TestProvide(link, caller string, args any) (any, error) {
	j, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return b.provide(link, caller, j)
}
