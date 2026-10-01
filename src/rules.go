package main

// 起動時に投稿する「この会話でのルール」を設定画面から編集する（2026-10-02 人間の依頼）。
// 編集するのは設定フォルダ（エージェントが読み書きできない場所。protect.go）の rules.md（日本語）と rules.en.md（英語）。
// ファイルがなければ既定のルールを返す。保存しても今の会話には投稿せず、次の投稿（起動時・新しい会話の開始時）から使う。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxRulesBytes はルールの長さの上限（毎回の投稿に載るため）
const maxRulesBytes = 64 << 10

var (
	ErrRulesEmpty           = errors.New("ルールが空です。既定のルールに戻すときは「既定に戻す」を使ってください")
	ErrRulesTooLarge        = errors.New("ルールが長すぎます（64KB まで）")
	ErrConfigDirUnavailable = errors.New("設定フォルダの場所を決められません")
)

// RulesInfo は1つの言語のルール
type RulesInfo struct {
	Lang   string `json:"lang"`
	File   string `json:"file"`   // 編集するファイル名（rules.md / rules.en.md）
	Text   string `json:"text"`   // ファイルの内容。ファイルがない・空なら既定のルール
	Custom bool   `json:"custom"` // ファイルに書いたルールがある（既定ではない）
	InUse  string `json:"in_use"` // この言語の投稿で実際に使うもの（ファイル名か "default"）
	Legacy bool   `json:"legacy"` // 使うファイルを旧い置き場所から読んでいる
}

// rulesFileFor は lang で編集するファイル名
func rulesFileFor(lang string) string { return pick(lang == langEn, rulesFileNameEn, rulesFileName) }

// Rules は日本語と英語のルールを返す
func (r *Room) Rules() map[string]RulesInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]RulesInfo{}
	for _, lang := range []string{langJa, langEn} {
		info := RulesInfo{Lang: lang, File: rulesFileFor(lang), Text: defaultRulesFor(lang)}
		if p, _ := resolveConfigFile(r.configDir, r.legacyConfigDir, info.File); p != "" {
			if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
				info.Text, info.Custom = strings.TrimSpace(string(b)), true
			}
		}
		_, info.InUse, _, info.Legacy = r.resolveRulesLocked(lang)
		out[lang] = info
	}
	return out
}

// SaveRules は lang のルールを設定フォルダに保存する
func (r *Room) SaveRules(lang, text string) error {
	if lang != langJa && lang != langEn {
		return ErrInvalidLang
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ErrRulesEmpty
	}
	if len(text) > maxRulesBytes {
		return ErrRulesTooLarge
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configDir == "" {
		return ErrConfigDirUnavailable
	}
	path := filepath.Join(r.configDir, rulesFileFor(lang))
	if err := os.MkdirAll(r.configDir, 0o700); err != nil {
		r.log.Error("rules.save", "lang", lang, "error_code", "RULES_SAVE_FAILED", "error", err.Error())
		return err
	}
	if err := writeStateFile(path, []byte(text+"\n")); err != nil {
		r.log.Error("rules.save", "lang", lang, "error_code", "RULES_SAVE_FAILED", "error", err.Error())
		return err
	}
	r.log.Info("rules.save", "lang", lang, "file", path, "bytes", len(text))
	return nil
}

// ResetRules は lang のルールのファイルを設定フォルダから消し、既定のルールに戻す。
// 旧い置き場所のファイルは消さない（Rules の legacy で分かる）
func (r *Room) ResetRules(lang string) error {
	if lang != langJa && lang != langEn {
		return ErrInvalidLang
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configDir == "" {
		return ErrConfigDirUnavailable
	}
	path := filepath.Join(r.configDir, rulesFileFor(lang))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Error("rules.reset", "lang", lang, "error_code", "RULES_SAVE_FAILED", "error", err.Error())
		return err
	}
	r.log.Info("rules.reset", "lang", lang, "file", path)
	return nil
}

// ---- HTTP ----------------------------------------------------------------------

func (room *Room) handleGetRules(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, room.Rules())
}

func (room *Room) handlePutRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRulesBytes*2)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	room.writeRulesResult(w, r, room.SaveRules(r.PathValue("lang"), body.Text))
}

func (room *Room) handleDeleteRules(w http.ResponseWriter, r *http.Request) {
	room.writeRulesResult(w, r, room.ResetRules(r.PathValue("lang")))
}

// writeRulesResult は保存・既定に戻した結果を返す。成功したら両方の言語のルールを返す
func (room *Room) writeRulesResult(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		writeData(w, http.StatusOK, room.Rules())
	case errors.Is(err, ErrInvalidLang):
		writeError(w, r, http.StatusBadRequest, "INVALID_LANG", err.Error())
	case errors.Is(err, ErrRulesEmpty):
		writeError(w, r, http.StatusBadRequest, "RULES_EMPTY", err.Error())
	case errors.Is(err, ErrRulesTooLarge):
		writeError(w, r, http.StatusBadRequest, "RULES_TOO_LARGE", err.Error())
	case errors.Is(err, ErrConfigDirUnavailable):
		writeError(w, r, http.StatusInternalServerError, "CONFIG_DIR_UNAVAILABLE", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "RULES_SAVE_FAILED", err.Error())
	}
}
