package main

// 再起動をまたいで会話を引き継ぐための状態。logs/session.json に保持する。
// 起動時に前回のチャットログを読み込み、各エージェントの CLI の会話ID と
// 渡し済みの位置（cursor）を戻して、同じログファイルへ追記を続ける。
// 作業ディレクトリが前回と異なる場合は復元しない（CLI の会話は作業ディレクトリに結び付くため）。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type sessionAgent struct {
	SessionID    string `json:"session_id"`
	Cursor       int    `json:"cursor"`
	Paused       bool   `json:"paused,omitempty"`        // 一時停止中（会話の復元とは別に、常に引き継ぐ）
	SessionStart int    `json:"session_start,omitempty"` // 今のセッションに最初に渡した発言のID
	LastInput    int    `json:"last_input,omitempty"`    // 直近1回の実行の入力トークン（セッション切り替えの判定に使う）
}

// sessionFile は会話の引き継ぎ用ファイルの内容
type sessionFile struct {
	LogFile string                  `json:"log_file"` // チャットログのファイル名（logDir 内）
	Workdir string                  `json:"workdir"`
	Agents  map[string]sessionAgent `json:"agents"`
}

func (r *Room) sessionPath() string { return filepath.Join(r.logDir, "session.json") }

// savedWorkdir は前回の作業ディレクトリを返す。記録がないか、ディレクトリがもうなければ空。
func savedWorkdir(logDir string) string {
	b, err := os.ReadFile(filepath.Join(logDir, "session.json"))
	if err != nil {
		return ""
	}
	var f sessionFile
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	wd, err := normalizeWorkdir(f.Workdir)
	if err != nil {
		return ""
	}
	return wd
}

// restoreSessionLocked は前回の会話を復元する。復元できなければ新しい会話のまま始める。
func (r *Room) restoreSessionLocked() {
	b, err := os.ReadFile(r.sessionPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.log.Info("session.restore", "skipped", "no_session_file")
		} else {
			r.log.Warn("session.restore", "error", err.Error())
		}
		return
	}
	var f sessionFile
	if err := json.Unmarshal(b, &f); err != nil {
		r.log.Warn("session.restore", "error", err.Error())
		return
	}
	for _, a := range r.agents { // 一時停止は作業ディレクトリや会話の復元に関係なく引き継ぐ
		a.paused = f.Agents[a.ID].Paused
	}
	if f.Workdir != r.workdir {
		// 別の作業ディレクトリで起動した。前回の会話はそのディレクトリの会話として残し、
		// このディレクトリに残っている会話があればそれを戻す（案 10）
		r.log.Info("session.restore", "skipped", "workdir_changed", "saved_workdir", f.Workdir)
		if logNameRe.MatchString(f.LogFile) {
			if _, err := os.Stat(filepath.Join(r.logDir, f.LogFile)); err == nil {
				r.archiveSessionLocked(f)
			}
		}
		r.restoreWorkdirLocked()
		return
	}
	r.applySessionLocked(f)
}

// applySessionLocked は f の会話（チャットログと各エージェントの会話ID・cursor）を戻す。戻せたら true を返す。
// 一時停止の状態は変えない
func (r *Room) applySessionLocked(f sessionFile) bool {
	if !logNameRe.MatchString(f.LogFile) {
		r.log.Warn("session.restore", "error", "invalid log_file", "log_file", f.LogFile)
		return false
	}
	path := filepath.Join(r.logDir, f.LogFile)
	msgs, err := readLogFile(path)
	if err != nil || len(msgs) == 0 {
		if err != nil && !errors.Is(err, os.ErrNotExist) { // 「新しい会話」の直後で発言がなければファイルはない
			r.log.Warn("session.restore", "error", err.Error(), "log_file", f.LogFile)
		}
		return false
	}
	for i := range msgs {
		msgs[i].Blocks = nil // 実行済みかを判別できないので、戻した発言のブロックは実行させない
	}
	r.messages = msgs
	r.nextID = msgs[len(msgs)-1].ID + 1
	r.logFile = path
	restored := 0
	for _, a := range r.agents {
		s, ok := f.Agents[a.ID]
		if !ok {
			continue
		}
		a.sessionID = s.SessionID
		a.cursor = max(0, min(s.Cursor, len(msgs)))
		a.lastInput, a.sessionStart = s.LastInput, s.SessionStart
		if a.sessionID != "" && a.sessionStart == 0 { // 古い形式には開始位置がない。会話全体で数えると全員が切り替わるので、復元した時点から数える
			a.sessionStart = r.nextID
		}
		if a.sessionID != "" {
			restored++
		}
	}
	r.log.Info("session.restore", "log_file", f.LogFile, "messages", len(msgs), "sessions", restored)
	return true
}

// currentSessionLocked は今の会話の状態を返す
func (r *Room) currentSessionLocked() sessionFile {
	f := sessionFile{LogFile: filepath.Base(r.logFile), Workdir: r.workdir, Agents: map[string]sessionAgent{}}
	for _, a := range r.agents {
		f.Agents[a.ID] = sessionAgent{SessionID: a.sessionID, Cursor: a.cursor, Paused: a.paused, LastInput: a.lastInput, SessionStart: a.sessionStart}
	}
	return f
}

func (r *Room) saveSessionLocked() {
	b, _ := json.MarshalIndent(r.currentSessionLocked(), "", "  ")
	if err := writeStateFile(r.sessionPath(), b); err != nil {
		r.log.Warn("session.save", "error", err.Error())
	}
}
