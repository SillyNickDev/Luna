// Package hrc is luna.hrc, Heart rate: !hr, threshold alerts, a stream summary, and the
// live BPM on an avatar parameter. Readings arrive as hr.update events from Luna's
// heart rate integration (or luna sim hr).
package hrc

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

type Settings struct {
	Name           string `luna:"name"`
	Alert          bool   `luna:"alert"`
	AlertBPM       int    `luna:"alert_bpm"`
	AlertMessage   string `luna:"alert_message"`
	AlertCooldownS int    `luna:"alert_cooldown_s"`
	OSCParam       string `luna:"osc_param"`
	StaleS         int    `luna:"stale_s"`
	SummaryChat    bool   `luna:"summary_chat"`
}

// Summary is one stream's heart rate.
type Summary struct {
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Readings int       `json:"readings"`
	Resting  int       `json:"resting_bpm"` // the 10th percentile, so a few low glitches don't count
	Average  int       `json:"average_bpm"`
	Peak     int       `json:"peak_bpm"`
	PeakAt   time.Time `json:"peak_at,omitzero"`
	PeakInto string    `json:"peak_into_stream,omitempty"`
}

type hrc struct {
	bot *luna.Bot
	cfg Settings

	mu        sync.Mutex
	hidden    bool
	live      bool
	started   time.Time
	samples   []int // this stream's readings
	peak      int
	peakAt    time.Time
	above     bool // the last reading was over the alert threshold
	lastAlert time.Time
	oscOff    bool // OSC unavailable: don't retry every reading
}

func New() *luna.Bot {
	bot := luna.New()
	h := &hrc{bot: bot}
	bot.UseConfig(&h.cfg)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if _, err := bot.Store.Get(ctx, "hidden", &h.hidden); err != nil {
			return err
		}
		if s, err := bot.Twitch.Stream(ctx, ""); err == nil && s.Live {
			h.mu.Lock()
			h.live, h.started = true, s.StartedAt
			h.mu.Unlock()
		}
		return nil
	})
	bot.On("hr.update", h.update, luna.DropIfBusy()) // latest wins: a backlog of old readings is no use
	bot.On("twitch.stream.online", func(ctx *luna.Ctx, ev *luna.Event) error {
		h.mu.Lock()
		h.live, h.started, h.samples, h.peak = true, bot.Now(), nil, 0
		h.mu.Unlock()
		return nil
	})
	bot.On("twitch.stream.offline", h.offline)
	bot.ChatCommand(luna.ChatCommand{Name: "hr", Aliases: []string{"heartrate"}, Cooldown: "10s"}, h.command)
	bot.CLI(luna.CLICommand{Name: "summary", Summary: "the current or last stream's heart rate: resting, average, peak and when"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			h.mu.Lock()
			live := h.live && len(h.samples) > 0
			s := h.summary()
			h.mu.Unlock()
			if live {
				return s, nil
			}
			var last Summary
			if ok, err := bot.Store.Get(ctx, "summary:last", &last); err != nil || !ok {
				return nil, errors.Join(err, errors.New("no stream with heart rate readings yet"))
			}
			return last, nil
		})
	bot.CLI(luna.CLICommand{Name: "now", Summary: "the latest reading and the last 5 minutes"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			bpm, at, err := bot.HR.BPM(ctx)
			if err != nil {
				return nil, err
			}
			hist, err := bot.HR.History(ctx, 5)
			if err != nil {
				return nil, err
			}
			var last []int
			for _, s := range hist {
				last = append(last, s.BPM)
			}
			return map[string]any{"bpm": bpm, "at": at, "hidden": h.isHidden(), "last_5_min": last}, nil
		})
	return bot
}

// Main runs the plugin as a capsule: `luna __hrc` inside the sandbox.
func Main() { New().Run() }

func (h *hrc) isHidden() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hidden
}

func (h *hrc) update(ctx *luna.Ctx, ev *luna.Event) error {
	bpm := int(ev.Int("bpm"))
	if bpm <= 0 {
		return nil
	}
	now := h.bot.Now()
	h.mu.Lock()
	hidden := h.hidden
	if h.live {
		h.samples = append(h.samples, bpm)
		if bpm > h.peak {
			h.peak, h.peakAt = bpm, now
		}
	}
	// Alert on crossing the threshold, not on every reading above it.
	crossed := !h.above && bpm >= h.cfg.AlertBPM
	h.above = bpm >= h.cfg.AlertBPM
	alert := crossed && h.cfg.Alert && !hidden && now.Sub(h.lastAlert) >= time.Duration(h.cfg.AlertCooldownS)*time.Second
	if alert {
		h.lastAlert = now
	}
	sendOSC := h.cfg.OSCParam != "" && !hidden && !h.oscOff
	h.mu.Unlock()

	if sendOSC {
		if err := h.bot.OSC.SetParam(ctx, h.cfg.OSCParam, bpm); errors.Is(err, luna.ErrNotConnected) {
			h.mu.Lock()
			h.oscOff = true // OSC is off in Luna; restart the plugin after turning it on
			h.mu.Unlock()
			h.bot.Log.Info(ctx, "OSC is off, so the heart rate won't reach your avatar")
		} else if err != nil {
			return err
		}
	}
	if alert {
		return h.bot.Chat.Send(ctx, h.render(h.cfg.AlertMessage, bpm))
	}
	return nil
}

func (h *hrc) render(tmpl string, bpm int) string {
	return strings.NewReplacer("{name}", h.cfg.Name, "{bpm}", strconv.Itoa(bpm)).Replace(tmpl)
}

func (h *hrc) command(ctx *luna.Ctx, args []string) error {
	if len(args) > 0 {
		switch v := strings.ToLower(args[0]); v {
		case "on", "off":
			if ctx.User().Rank != "broadcaster" {
				return nil // only the streamer decides whether their heart rate is shown
			}
			h.mu.Lock()
			h.hidden = v == "off"
			h.mu.Unlock()
			if err := h.bot.Store.Set(ctx, "hidden", v == "off", 0); err != nil {
				return err
			}
			if v == "off" {
				return ctx.Reply("Heart rate hidden. !hr on to show it again.")
			}
			return ctx.Reply("Heart rate showing again.")
		}
	}
	if h.isHidden() {
		return nil
	}
	bpm, at, err := h.bot.HR.BPM(ctx)
	if err != nil {
		return err
	}
	if bpm == 0 || h.bot.Now().Sub(at) > time.Duration(h.cfg.StaleS)*time.Second {
		return ctx.Reply("No heart rate reading right now.")
	}
	return ctx.Reply(h.render("{name}'s heart rate: {bpm} bpm 💓", bpm))
}

// summary needs h.mu held.
func (h *hrc) summary() Summary {
	s := Summary{Started: h.started, Readings: len(h.samples), Peak: h.peak, PeakAt: h.peakAt}
	if len(h.samples) == 0 {
		return s
	}
	sorted := slices.Sorted(slices.Values(h.samples))
	s.Resting = sorted[len(sorted)/10]
	sum := 0
	for _, b := range h.samples {
		sum += b
	}
	s.Average = sum / len(h.samples)
	if !h.started.IsZero() && !h.peakAt.IsZero() {
		s.PeakInto = h.peakAt.Sub(h.started).Round(time.Minute).String()
	}
	return s
}

func (h *hrc) offline(ctx *luna.Ctx, ev *luna.Event) error {
	h.mu.Lock()
	if !h.live {
		h.mu.Unlock()
		return nil
	}
	h.live = false
	s := h.summary()
	s.Ended = h.bot.Now()
	hidden := h.hidden
	h.mu.Unlock()
	if s.Readings == 0 {
		return nil
	}
	if err := h.bot.Store.Set(ctx, "summary:last", s, 0); err != nil {
		return err
	}
	msg := fmt.Sprintf("Heart rate this stream: resting %d, average %d, peak %d bpm %s in.", s.Resting, s.Average, s.Peak, s.PeakInto)
	h.bot.Log.Info(ctx, msg)
	if h.cfg.SummaryChat && !hidden {
		return h.bot.Chat.Send(ctx, msg)
	}
	return nil
}
