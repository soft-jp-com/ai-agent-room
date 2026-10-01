package main

// 会話の検索と書き出し（案 4）。
// logs/chat-*.jsonl を横断してキーワードを探し、一致した発言の前後を切り出して返す。
// 書き出しは1つのログを Markdown にする。どちらもログを読むだけで、書き込みはしない。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SearchHit は検索で一致した発言1件
type SearchHit struct {
	Log     string `json:"log"`               // ログファイル名
	Current bool   `json:"current,omitempty"` // 現在の会話のログか
	ID      int    `json:"id"`
	From    string `json:"from"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Time    string `json:"time"`
	Snippet string `json:"snippet"` // 一致箇所の前後を切り出した本文（1行にする）
}

// SearchResult は検索の結果
type SearchResult struct {
	Query     string      `json:"query"`
	Hits      []SearchHit `json:"hits"`
	Total     int         `json:"total"`               // 打ち切り前の一致件数
	Logs      int         `json:"logs"`                // 探したログの数
	Truncated bool        `json:"truncated,omitempty"` // limit で打ち切ったか
	// TotalPartial は、上限に達したので残りのログを探さずに打ち切ったこと（Total と Logs は探した範囲の数。案4）
	TotalPartial bool `json:"total_partial,omitempty"`
}

const (
	searchDefaultLimit = 100
	searchMaxLimit     = 500
	snippetContext     = 60 // 一致箇所の前後に付ける文字数
)

var ErrEmptyQuery = errors.New("検索する語が空です")

// SearchMessages は過去ログと現在の会話を横断して query を含む発言を探す。
// query は大文字小文字を区別しない部分一致。from が空でなければ、その発言者（human / system / エージェントID）に絞る。
// 結果は新しいログから順に、ログ内では記録順に並べる。
func (r *Room) SearchMessages(query, from string, limit int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	res := SearchResult{Query: query, Hits: []SearchHit{}}
	if query == "" {
		return res, ErrEmptyQuery
	}
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	limit = min(limit, searchMaxLimit)

	r.mu.Lock()
	current := filepath.Base(r.logFile)
	r.mu.Unlock()
	entries, err := os.ReadDir(r.logDir)
	if err != nil {
		return res, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && logNameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // 新しいログから探す

	needle := strings.ToLower(query)
	// JSON で書き換わらない語なら、行を解析する前に行のままで絞り込む（案4）。一致しない行は解析しない
	prefilter := !strings.ContainsFunc(query, func(c rune) bool {
		return c < 0x20 || strings.ContainsRune(`"\<>&`, c) || c == 0x2028 || c == 0x2029
	})
	bneedle := []byte(needle)
	for _, name := range names {
		if res.TotalPartial {
			break
		}
		err := scanLogLines(filepath.Join(r.logDir, name), func(line []byte) bool {
			if prefilter && !bytes.Contains(bytes.ToLower(line), bneedle) {
				return true
			}
			var m Message
			if json.Unmarshal(line, &m) != nil {
				return true
			}
			if from != "" && m.From != from {
				return true
			}
			if !strings.Contains(strings.ToLower(m.Text), needle) {
				return true
			}
			res.Total++
			if len(res.Hits) >= limit { // 上限を超える一致を見つけたら、残りは探さない
				res.Truncated, res.TotalPartial = true, true
				return false
			}
			res.Hits = append(res.Hits, SearchHit{
				Log: name, Current: name == current, ID: m.ID, From: m.From, Name: m.Name,
				Kind: m.Kind, Time: m.Time, Snippet: snippetAround(m.Text, query),
			})
			return true
		})
		if err != nil {
			r.log.Warn("search.read", "log", name, "error", err.Error())
			continue
		}
		res.Logs++
	}
	return res, nil
}

// scanLogLines はログを1行ずつ fn に渡す。fn が false を返したら読むのをやめる
func scanLogLines(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		if !fn(sc.Bytes()) {
			return nil
		}
	}
	return sc.Err()
}

// snippetAround は query の最初の一致箇所の前後を切り出し、改行と連続する空白を詰めて1行にして返す。
// 切り出しは詰めたあとの文字列の上で行う（元の位置は空白を詰めるとずれるため）。
func snippetAround(text, query string) string {
	flat := []rune(strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " "))
	idx := strings.Index(strings.ToLower(string(flat)), strings.ToLower(query))
	if idx < 0 { // 改行や連続する空白をまたぐ一致は詰めたあとに見つからない。その場合は先頭から見せる
		idx = 0
	}
	pos := len([]rune(string(flat)[:idx])) // バイト位置を文字数に直す
	start, end := pos-snippetContext, pos+len([]rune(query))+snippetContext
	head, tail := "…", "…"
	if start <= 0 {
		start, head = 0, ""
	}
	if end >= len(flat) {
		end, tail = len(flat), ""
	}
	return head + string(flat[start:end]) + tail
}

// ExportMarkdown は1つのチャットログを Markdown にして返す。name はファイル名のみ受け付ける。
func (r *Room) ExportMarkdown(name string) (string, error) {
	msgs, err := r.ReadLog(name)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", ErrLogNotFound
	}
	count := 0
	for _, m := range msgs {
		if m.Kind != "system" {
			count++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 会話ログ %s\n\n", name)
	fmt.Fprintf(&b, "- 期間: %s 〜 %s\n", msgs[0].Time, msgs[len(msgs)-1].Time)
	fmt.Fprintf(&b, "- 発言数: %d（システムメッセージを除く）/ 全 %d 件\n\n", count, len(msgs))
	for _, m := range msgs {
		who := m.Name
		if who == "" {
			who = m.From
		}
		title := fmt.Sprintf("#%d %s", m.ID, who)
		if m.Model != "" {
			title += fmt.Sprintf("（%s）", m.Model)
		}
		if m.Kind != "" && m.Kind != "chat" {
			title += fmt.Sprintf("［%s］", m.Kind)
		}
		fmt.Fprintf(&b, "## %s — %s\n\n", title, m.Time)
		var notes []string
		if m.ReplyTo > 0 {
			notes = append(notes, fmt.Sprintf("#%d への返信", m.ReplyTo))
		}
		if m.ReadUpTo > 0 {
			notes = append(notes, fmt.Sprintf("#%d まで読了", m.ReadUpTo))
		}
		if len(notes) > 0 {
			fmt.Fprintf(&b, "> %s\n\n", strings.Join(notes, "、"))
		}
		b.WriteString(strings.TrimRight(m.Text, "\n"))
		b.WriteString("\n\n")
	}
	return b.String(), nil
}
