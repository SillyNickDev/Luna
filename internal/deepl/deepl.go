// Package deepl translates text with DeepL. The API key lives in the daemon, like Twitch's
// tokens; plugins ask for translations and never see it.
package deepl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoints; free keys end in ":fx" and use the free one. Tests point them at a fake.
var (
	FreeURL = "https://api-free.deepl.com/v2/translate"
	ProURL  = "https://api.deepl.com/v2/translate"
)

var hc = &http.Client{Timeout: 15 * time.Second}

// Translate turns text into the target language ("EN", "DE", "PT-BR", ...) and says what
// language DeepL detected it as.
func Translate(ctx context.Context, key, text, target string) (out, source string, err error) {
	u := ProURL
	if strings.HasSuffix(key, ":fx") {
		u = FreeURL
	}
	body, _ := json.Marshal(map[string]any{"text": []string{text}, "target_lang": strings.ToUpper(target)})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case 200:
	case 403:
		return "", "", fmt.Errorf("deepl: the API key was refused; run luna auth login deepl")
	case 456:
		return "", "", fmt.Errorf("deepl: this month's character quota is used up")
	default:
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		return "", "", fmt.Errorf("deepl: %d %s", resp.StatusCode, strings.TrimSpace(e.Message+" "+http.StatusText(resp.StatusCode)))
	}
	var r struct {
		Translations []struct {
			Source string `json:"detected_source_language"`
			Text   string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Translations) == 0 {
		return "", "", fmt.Errorf("deepl: unexpected answer")
	}
	return r.Translations[0].Text, r.Translations[0].Source, nil
}
