package main

// 過去のチャットログ（logs/chat-YYYYMMDD-HHMMSS.jsonl）の一覧と読み込み。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type LogSummary struct {
	Name    string `json:"name"`
	Started string `json:"started"` // 最初の発言時刻
	Updated string `json:"updated"` // 最後の発言時刻
	Count   int    `json:"count"`   // 発言数（システムメッセージを除く）
	Title   string `json:"title"`   // 人間の最初の発言
	Current bool   `json:"current"` // 現在の会話のログか
}

var (
	logNameRe      = regexp.MustCompile(`^chat-\d{8}-\d{6}\.jsonl$`)
	ErrLogNotFound = errors.New("ログが見つかりません")
)

func readLogFile(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs, sc.Err()
}

// ListLogs は logDir 内のチャットログを新しい順に返す。
func (r *Room) ListLogs() ([]LogSummary, error) {
	r.mu.Lock()
	current := filepath.Base(r.logFile)
	r.mu.Unlock()
	entries, err := os.ReadDir(r.logDir)
	if err != nil {
		return nil, err
	}
	var list []LogSummary
	for _, e := range entries {
		if e.IsDir() || !logNameRe.MatchString(e.Name()) {
			continue
		}
		s, ok := summarizeLog(filepath.Join(r.logDir, e.Name()), e)
		if !ok {
			continue
		}
		s.Name, s.Current = e.Name(), e.Name() == current
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
	return list, nil
}

// logSummaryCache は一覧用の要約を、ファイルのサイズと更新時刻が変わらないあいだ覚えておく（案4）。
// 一覧を開くたびに全ログを読み直さないため。メモリ上だけに持ち、再起動後の最初の一覧では全ログを読む
var logSummaryCache = struct {
	sync.Mutex
	m map[string]cachedSummary // キーはログのパス
}{m: map[string]cachedSummary{}}

type cachedSummary struct {
	size    int64
	modTime time.Time
	s       LogSummary // Name・Current は呼び出し側で付ける
	ok      bool       // 発言が1件以上あったか
}

// summarizeLog はログの要約（期間・件数・題名）を返す。発言がない・読めないログは ok=false
func summarizeLog(path string, e os.DirEntry) (LogSummary, bool) {
	info, err := e.Info()
	if err != nil {
		return LogSummary{}, false
	}
	logSummaryCache.Lock()
	c, hit := logSummaryCache.m[path]
	logSummaryCache.Unlock()
	if hit && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return c.s, c.ok
	}
	msgs, err := readLogFile(path)
	if err != nil {
		return LogSummary{}, false // 読めなかったものは覚えない（次の一覧で読み直す）
	}
	c = cachedSummary{size: info.Size(), modTime: info.ModTime(), ok: len(msgs) > 0}
	if c.ok {
		c.s = LogSummary{Started: msgs[0].Time, Updated: msgs[len(msgs)-1].Time}
		for _, m := range msgs {
			if m.Kind != "system" {
				c.s.Count++
			}
			if c.s.Title == "" && m.From == "human" {
				c.s.Title = m.Text
			}
		}
		if t := []rune(strings.ReplaceAll(c.s.Title, "\n", " ")); len(t) > 80 {
			c.s.Title = string(t[:80]) + "…"
		} else {
			c.s.Title = string(t)
		}
	}
	logSummaryCache.Lock()
	logSummaryCache.m[path] = c
	logSummaryCache.Unlock()
	return c.s, c.ok
}

// ReadLog は指定したチャットログの全発言を返す。name はファイル名のみ受け付ける。
func (r *Room) ReadLog(name string) ([]Message, error) {
	if !logNameRe.MatchString(name) {
		return nil, ErrLogNotFound
	}
	msgs, err := readLogFile(filepath.Join(r.logDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrLogNotFound
	}
	return msgs, err
}

var ErrBranchPointNotFound = errors.New("分岐する発言が元のログにありません")

// BranchFromLog は過去のログ name の発言 upto までを引き継いで、新しい会話を始める（案3）。
// 元のログは読むだけで変更しない。引き継ぐ発言は ID・返信先をそのまま保ち、新しいログファイルに書き写す。
// ログや発言が見つからなければ、今の会話をリセットせずにエラーを返す
func (r *Room) BranchFromLog(name string, upto int) error {
	msgs, err := r.ReadLog(name)
	if err != nil {
		return err
	}
	n := slices.IndexFunc(msgs, func(m Message) bool { return m.ID == upto })
	if n < 0 {
		return ErrBranchPointNotFound
	}
	msgs = msgs[:n+1]
	for i := range msgs {
		msgs[i].Blocks = nil // 復元と同じく、引き継いだ発言のブロックは実行させない
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetLocked()
	var b []byte
	for i := range msgs {
		if line, err := json.Marshal(msgs[i]); err == nil {
			b = append(append(b, line...), '\n')
		}
	}
	if err := os.WriteFile(r.logFile, b, 0o644); err != nil {
		r.log.Error("chatlog.write", "error", err.Error())
	}
	r.messages = msgs
	r.nextID = msgs[len(msgs)-1].ID + 1
	for i := range msgs {
		r.broadcastLocked(Event{Type: "message", Message: &msgs[i]})
	}
	r.saveSessionLocked()
	r.log.Info("conversation.branch", "from", name, "upto", upto, "messages", len(msgs), "log_file", filepath.Base(r.logFile))
	r.postProfileLocked()
	r.postLocked("system", fmt.Sprintf("%s の #%d までを引き継いで、新しい会話を始めました。元のログは変更していません。", name, upto), "system")
	return nil
}
