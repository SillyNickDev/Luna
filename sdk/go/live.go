package luna

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"luna/internal/capsule"
	"luna/internal/plugin/pb"
)

func toEvent(e *pb.Event) *Event {
	return &Event{ID: e.Id, Type: e.Type, Source: e.Source, TS: time.UnixMilli(e.TsMs).UTC(), Simulated: e.Simulated, Data: e.DataJson}
}

// runLive is the capsule side of the Capsules spec's control channel plus the plugin API.
func (b *Bot) runLive() error {
	conn, err := net.Dial("unix", os.Getenv("CAPSULE_CONTROL"))
	if err != nil {
		return err
	}
	ctl := capsule.NewConn(conn)
	if err := ctl.Write("hello", 0, capsule.Hello{Spec: capsule.SpecVersion, PID: os.Getpid()}); err != nil {
		return err
	}
	if f, err := ctl.Read(); err != nil || f.T != "welcome" {
		return fmt.Errorf("the runtime didn't welcome this capsule: %v", err)
	}
	gc, err := grpc.NewClient("unix://"+os.Getenv("CAPSULE_HOST"), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20)))
	if err != nil {
		return err
	}
	defer gc.Close()
	b.host = pb.NewHostClient(gc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx
	hello, err := b.host.Hello(ctx, &pb.HelloRequest{Api: API, Sdk: SDK})
	if err != nil {
		return fmt.Errorf("Luna refused the plugin API: %w", wrap(err))
	}
	b.ID, b.Dev = hello.Id, hello.Dev
	if err := b.loadConfig(hello.ConfigJson); err != nil {
		return err
	}

	var stopping bool
	var smu sync.Mutex
	fatal := func(what string, err error) {
		smu.Lock()
		defer smu.Unlock()
		if !stopping {
			fmt.Fprintf(os.Stderr, "%s ended: %v\n", what, wrap(err))
			os.Exit(1) // the supervisor restarts the capsule
		}
	}

	// Each handler gets its own ordered queue, so a slow one doesn't hold up the others.
	for _, h := range b.handlers {
		h.q = make(chan *Event, 1000)
		go func() {
			for ev := range h.q {
				b.call(ev, func(c *Ctx) error { return h.fn(c, ev) })
			}
		}()
	}
	if types := b.Describe()["events"].([]string); len(types) > 0 {
		sub, err := b.host.Subscribe(ctx, &pb.SubscribeRequest{Types: types})
		if err != nil {
			return wrap(err)
		}
		go func() {
			for {
				e, err := sub.Recv()
				if err != nil {
					fatal("event stream", err)
					return
				}
				b.dispatch(toEvent(e), false)
			}
		}()
	}
	if len(b.cli)+len(b.chat) > 0 {
		cmds, err := b.host.Commands(ctx)
		if err != nil {
			return wrap(err)
		}
		var smu sync.Mutex // gRPC streams take one sender at a time
		go func() {
			for {
				c, err := cmds.Recv()
				if err != nil {
					fatal("command stream", err)
					return
				}
				go func() {
					var ev *Event
					if c.Event != nil {
						ev = toEvent(c.Event)
					}
					r := &pb.CommandResult{Id: c.Id}
					data, err := b.command(c.Kind, c.Name, c.ArgsJson, ev)
					switch d := data.(type) {
					case nil:
					case string:
						r.Text = d
					default:
						r.DataJson, _ = json.Marshal(d)
					}
					if err != nil {
						r.Error = err.Error()
					}
					smu.Lock()
					cmds.Send(r)
					smu.Unlock()
				}()
			}
		}()
	}
	b.start()
	go func() {
		for {
			var due <-chan time.Time
			if d := b.runDue(); d >= 0 {
				due = time.After(d)
			}
			select {
			case <-due:
			case <-b.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	for _, fn := range b.onStart {
		b.call(nil, fn)
	}
	if err := ctl.Write("ready", 0, nil); err != nil {
		return err
	}
	for {
		f, err := ctl.Read()
		if err != nil || f.T == "shutdown" {
			break
		}
		switch f.T {
		case "ping":
			ctl.Write("pong", f.ID, nil)
		case "call":
			go func() { ctl.Write("result", f.ID, b.serveCall(f)) }()
		}
	}
	smu.Lock()
	stopping = true
	smu.Unlock()
	for _, fn := range b.onStop {
		b.call(nil, fn)
	}
	cancel()
	return nil
}
