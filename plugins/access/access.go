// Package access is luna.access, Accessibility: viewers' pronouns from pronouns.alejo.io,
// and chat translation with the streamer's DeepL key (!translate, or every message when
// auto_translate is on).
package access

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

type Settings struct {
	Language      string `luna:"language"`
	AutoTranslate bool   `luna:"auto_translate"`
	MinChars      int    `luna:"min_chars"`
}

// pronounsAPI is pronouns.alejo.io; tests point it at a fake.
var pronounsAPI = "https://api.pronouns.alejo.io/v1"

type pronoun struct {
	Subject  string `json:"subject"`
	Object   string `json:"object"`
	Singular bool   `json:"singular"`
}

type access struct {
	bot *luna.Bot
	cfg Settings

	mu    sync.Mutex
	kinds map[string]pronoun // pronoun id → its words, fetched once
}

func New() *luna.Bot {
	bot := luna.New()
	a := &access{bot: bot}
	bot.UseConfig(&a.cfg)
	bot.On("twitch.chat.message", a.auto)
	bot.ChatCommand(luna.ChatCommand{Name: "pronouns", UserCooldown: "10s"}, a.pronouns)
	bot.ChatCommand(luna.ChatCommand{Name: "translate", Aliases: []string{"tr"}, UserCooldown: "10s"}, a.translate)
	return bot
}

// Main runs the plugin as a capsule: `luna __access` inside the sandbox.
func Main() { New().Run() }

func (a *access) get(ctx *luna.Ctx, path string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", pronounsAPI+path, nil)
	if err != nil {
		return false, err
	}
	resp, err := a.bot.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("pronouns.alejo.io: %s", resp.Status)
	}
	return true, json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
}

// of says someone's pronouns as viewers expect them ("She/Her", "He/They"), "" if they haven't set any.
func (a *access) of(ctx *luna.Ctx, login string) (string, error) {
	var cached string
	if ok, err := a.bot.Store.Get(ctx, "pr:"+login, &cached); err != nil || ok {
		return cached, err
	}
	a.mu.Lock()
	kinds := a.kinds
	a.mu.Unlock()
	if kinds == nil {
		if _, err := a.get(ctx, "/pronouns", &kinds); err != nil {
			return "", err
		}
		a.mu.Lock()
		a.kinds = kinds
		a.mu.Unlock()
	}
	var u struct {
		Main string  `json:"pronoun_id"`
		Alt  *string `json:"alt_pronoun_id"`
	}
	found, err := a.get(ctx, "/users/"+url.PathEscape(login), &u)
	if err != nil {
		return "", err
	}
	text := ""
	if p, ok := kinds[u.Main]; found && ok {
		switch {
		case u.Alt != nil && kinds[*u.Alt].Subject != "":
			text = p.Subject + "/" + kinds[*u.Alt].Subject
		case p.Singular:
			text = p.Subject
		default:
			text = p.Subject + "/" + p.Object
		}
	}
	// Remembered for 6 hours, set or not, so chat can't make Luna hammer the API.
	return text, a.bot.Store.Set(ctx, "pr:"+login, text, 6*time.Hour)
}

func (a *access) pronouns(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	login := u.Login
	if len(args) > 0 {
		login = strings.ToLower(strings.TrimPrefix(args[0], "@"))
	}
	p, err := a.of(ctx, login)
	if err != nil {
		return ctx.Reply("Couldn't reach pronouns.alejo.io: " + strings.TrimPrefix(err.Error(), "luna: "))
	}
	if p == "" {
		return ctx.Reply(login + " hasn't set pronouns; anyone can at pr.alejo.io")
	}
	return ctx.Reply(login + ": " + p)
}

// translate is !translate <text> (into the stream's language) or !translate <lang> <text>.
func (a *access) translate(ctx *luna.Ctx, args []string) error {
	if len(args) == 0 {
		return ctx.Reply("Usage: !translate [language code] <text>, like !translate es good luck!")
	}
	target := a.cfg.Language
	if code := strings.ToUpper(args[0]); len(args) > 1 && isLang(code) {
		target, args = code, args[1:]
	}
	out, src, err := a.bot.Translate(ctx, strings.Join(args, " "), target)
	if err != nil {
		return ctx.Reply("Couldn't translate: " + strings.TrimPrefix(err.Error(), "luna: "))
	}
	return ctx.Reply(fmt.Sprintf("🌐 (%s→%s) %s", src, target, out))
}

// isLang is the DeepL target codes, so "!translate hi there" isn't read as Hindi.
func isLang(code string) bool {
	switch code {
	case "AR", "BG", "CS", "DA", "DE", "EL", "EN", "EN-GB", "EN-US", "ES", "ET", "FI", "FR", "HU", "ID", "IT", "JA", "KO",
		"LT", "LV", "NB", "NL", "PL", "PT", "PT-BR", "PT-PT", "RO", "RU", "SK", "SL", "SV", "TR", "UK", "ZH":
		return true
	}
	return false
}

// auto translates chat that isn't in the stream's language, when the streamer turned it on.
func (a *access) auto(ctx *luna.Ctx, ev *luna.Event) error {
	text := strings.TrimSpace(ev.Str("text"))
	u := ev.User()
	// Luna's own translations come back as chat too; translating them again would only spend quota.
	if !a.cfg.AutoTranslate || u == nil || strings.HasPrefix(text, "!") || strings.HasPrefix(text, "🌐") ||
		len([]rune(text)) < a.cfg.MinChars {
		return nil
	}
	out, src, err := a.bot.Translate(ctx, text, a.cfg.Language)
	if err != nil {
		a.bot.Log.Warn(ctx, "auto-translate: "+err.Error())
		return nil
	}
	if strings.HasPrefix(strings.ToUpper(a.cfg.Language), src) || strings.EqualFold(out, text) {
		return nil // already in the stream's language
	}
	return a.bot.Chat.Send(ctx, fmt.Sprintf("🌐 %s (%s): %s", u.DisplayName(), src, out))
}
