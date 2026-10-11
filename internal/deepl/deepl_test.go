package deepl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "DeepL-Auth-Key k:fx" {
			w.WriteHeader(403)
			return
		}
		var in struct {
			Text   []string `json:"text"`
			Target string   `json:"target_lang"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		json.NewEncoder(w).Encode(map[string]any{"translations": []any{map[string]string{
			"detected_source_language": "DE", "text": strings.ToUpper(in.Text[0]) + " in " + in.Target}}})
	}))
	defer srv.Close()
	FreeURL, ProURL = srv.URL+"/free", srv.URL+"/pro"
	out, src, err := Translate(context.Background(), "k:fx", "hallo", "en")
	if err != nil || out != "HALLO in EN" || src != "DE" || path != "/free" {
		t.Fatalf("%q %q %v (path %s)", out, src, err, path)
	}
	if _, _, err := Translate(context.Background(), "wrong", "hallo", "en"); err == nil || !strings.Contains(err.Error(), "luna auth login deepl") || path != "/pro" {
		t.Fatalf("bad key: %v (path %s)", err, path)
	}
}
