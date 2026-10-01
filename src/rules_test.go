package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ファイルがなければ既定のルールを返し、保存すると設定フォルダに書いて次の投稿から使う。既定に戻すとファイルを消す
func TestRulesSaveAndReset(t *testing.T) {
	r, _ := newTestRoom(t)
	cfg := filepath.Join(t.TempDir(), "config") // まだない設定フォルダも作る
	r.SetEnvProfile(&envInfo{OS: "test"}, cfg, "")

	got := r.Rules()
	if ja := got[langJa]; ja.Custom || ja.InUse != rulesDefault || ja.Text != defaultRules || ja.File != rulesFileName {
		t.Fatalf("既定の日本語のルール %+v", ja)
	}
	if en := got[langEn]; en.Custom || en.Text != defaultRulesEn || en.File != rulesFileNameEn {
		t.Fatalf("既定の英語のルール %+v", en)
	}

	if err := r.SaveRules(langJa, "  - 独自のルール\r\n- 2行目\n\n"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg, rulesFileName))
	if err != nil || string(b) != "- 独自のルール\n- 2行目\n" {
		t.Fatalf("保存した内容 %q %v", b, err)
	}
	got = r.Rules()
	if ja := got[langJa]; !ja.Custom || ja.InUse != rulesFileName || ja.Text != "- 独自のルール\n- 2行目" {
		t.Fatalf("保存後の日本語のルール %+v", ja)
	}
	// 英語の rules.en.md がなければ、英語の投稿も rules.md を使う
	if en := got[langEn]; en.Custom || en.InUse != rulesFileName || en.Text != defaultRulesEn {
		t.Fatalf("保存後の英語のルール %+v", en)
	}
	// 今の会話には投稿しない
	n := len(r.messages)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "独自のルール") {
		t.Fatalf("次の投稿で使っていない:\n%s", m.Text)
	}
	if n != 1 {
		t.Fatalf("保存で投稿した: %d 件", n)
	}

	if err := r.ResetRules(langJa); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, rulesFileName)); !os.IsNotExist(err) {
		t.Fatalf("ファイルが残っている: %v", err)
	}
	if ja := r.Rules()[langJa]; ja.Custom || ja.Text != defaultRules {
		t.Fatalf("既定に戻っていない %+v", ja)
	}
	if err := r.ResetRules(langEn); err != nil { // ファイルがなくても失敗しない
		t.Fatal(err)
	}
}

// 不正な言語・空・長すぎるルールは断り、ファイルを書かない
func TestRulesSaveRejects(t *testing.T) {
	r, _ := newTestRoom(t)
	cfg := t.TempDir()
	r.SetEnvProfile(&envInfo{OS: "test"}, cfg, "")
	for _, c := range []struct {
		lang, text string
		want       error
	}{
		{"fr", "x", ErrInvalidLang},
		{langJa, " \n ", ErrRulesEmpty},
		{langEn, strings.Repeat("a", maxRulesBytes+1), ErrRulesTooLarge},
	} {
		if err := r.SaveRules(c.lang, c.text); err != c.want {
			t.Errorf("%s: got %v, want %v", c.lang, err, c.want)
		}
	}
	if entries, _ := os.ReadDir(cfg); len(entries) != 0 {
		t.Fatalf("断ったのにファイルを書いた: %v", entries)
	}

	r2, _ := newTestRoom(t) // 設定フォルダが決まっていない
	if err := r2.SaveRules(langJa, "x"); err != ErrConfigDirUnavailable {
		t.Fatalf("got %v", err)
	}
}

// API: GET で両方の言語を返し、PUT で保存、DELETE で既定に戻す。エラーは error_code で返す
func TestRulesAPI(t *testing.T) {
	r, _ := newTestRoom(t)
	r.SetEnvProfile(&envInfo{OS: "test"}, t.TempDir(), "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/rules", r.handleGetRules)
	mux.HandleFunc("PUT /api/rules/{lang}", r.handlePutRules)
	mux.HandleFunc("DELETE /api/rules/{lang}", r.handleDeleteRules)
	do := func(method, target, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var res map[string]any
		json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res
	}

	if code, res := do("PUT", "/api/rules/en", `{"text": "- my rules"}`); code != 200 || !strings.Contains(toJSON(res["data"]), `"text":"- my rules"`) {
		t.Fatalf("PUT %d %v", code, res)
	}
	if code, res := do("GET", "/api/rules", ""); code != 200 || !strings.Contains(toJSON(res["data"]), `"in_use":"rules.en.md"`) {
		t.Fatalf("GET %d %v", code, res)
	}
	if code, res := do("PUT", "/api/rules/ja", `{"text": ""}`); code != 400 || res["error_code"] != "RULES_EMPTY" {
		t.Fatalf("空の PUT %d %v", code, res)
	}
	if code, res := do("PUT", "/api/rules/fr", `{"text": "x"}`); code != 400 || res["error_code"] != "INVALID_LANG" {
		t.Fatalf("不正な言語の PUT %d %v", code, res)
	}
	if code, res := do("DELETE", "/api/rules/en", ""); code != 200 || strings.Contains(toJSON(res["data"]), "my rules") {
		t.Fatalf("DELETE %d %v", code, res)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
