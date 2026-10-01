package main

// 作業ディレクトリごとの会話の切り替え（案 10）。作業ディレクトリを変える前に、今の会話の状態
// （session.json と同じ形）を logs/workdirs.json に作業ディレクトリごとに残し、変えた先に残っている会話があれば復元する。
// チャットログ自体は logs/chat-*.jsonl に残っているので、ここにはファイル名と各エージェントの会話ID・cursor だけを置く。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const workdirsMax = 30 // 残す作業ディレクトリの数（超えたら古いものから消す）

type workdirEntry struct {
	sessionFile
	SavedAt time.Time `json:"saved_at"`
}

func (r *Room) workdirsPath() string { return filepath.Join(r.logDir, "workdirs.json") }

func (r *Room) loadWorkdirsLocked() map[string]workdirEntry {
	m := map[string]workdirEntry{}
	b, err := os.ReadFile(r.workdirsPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.log.Warn("workdirs.load", "error", err.Error())
		}
		return m
	}
	if err := json.Unmarshal(b, &m); err != nil {
		r.log.Warn("workdirs.load", "error", err.Error())
		return map[string]workdirEntry{}
	}
	return m
}

// archiveWorkdirLocked は今の会話を、今の作業ディレクトリの会話として残す。発言がなければ残さない
func (r *Room) archiveWorkdirLocked() {
	if len(r.messages) == 0 || r.workdir == "" {
		return
	}
	r.archiveSessionLocked(r.currentSessionLocked())
}

// archiveSessionLocked は f を f.Workdir の会話として残す（同じディレクトリの古い記録は置き換える）
func (r *Room) archiveSessionLocked(f sessionFile) {
	if f.Workdir == "" {
		return
	}
	m := r.loadWorkdirsLocked()
	if k, ok := findWorkdirKey(m, f.Workdir); ok {
		delete(m, k)
	}
	m[f.Workdir] = workdirEntry{sessionFile: f, SavedAt: time.Now()}
	if len(m) > workdirsMax {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return m[keys[i]].SavedAt.Before(m[keys[j]].SavedAt) })
		for _, k := range keys[:len(m)-workdirsMax] {
			delete(m, k)
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeStateFile(r.workdirsPath(), b); err != nil {
		r.log.Warn("workdirs.save", "error", err.Error())
		return
	}
	r.log.Info("workdirs.archive", "workdir", f.Workdir, "log_file", f.LogFile)
}

// findWorkdirKey は m から workdir の記録のキーを探す。Windows のパスは大文字・小文字を区別しないので、区別せずに比べる
func findWorkdirKey(m map[string]workdirEntry, workdir string) (string, bool) {
	if _, ok := m[workdir]; ok {
		return workdir, true
	}
	for k := range m {
		if strings.EqualFold(k, workdir) {
			return k, true
		}
	}
	return "", false
}

// restoreWorkdirLocked は r.workdir に残っている会話を戻す。戻せたら true を返す。
// resetLocked の直後に呼ぶ（一時停止の状態は変えない）
func (r *Room) restoreWorkdirLocked() bool {
	m := r.loadWorkdirsLocked()
	k, ok := findWorkdirKey(m, r.workdir)
	if !ok {
		return false
	}
	f := m[k].sessionFile
	f.Workdir = r.workdir
	return r.applySessionLocked(f)
}
