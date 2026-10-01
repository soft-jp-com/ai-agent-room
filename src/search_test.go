package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSearchRoom(t *testing.T) *Room {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
	return NewRoom([]*Agent{a}, t.TempDir(), t.TempDir(), 10, time.Second, discard)
}

func TestSearchMessages(t *testing.T) {
	r := newSearchRoom(t)
	r.mu.Lock()
	r.postLocked("human", "タイムアウトの改善をお願いします", "chat")
	r.postLocked("x", "adapters.go の TIMEOUT を可変にします", "chat")
	r.postLocked("x", "無関係な発言", "chat")
	r.mu.Unlock()

	// 大文字小文字を区別せず、現在の会話のログも探す
	res, err := r.SearchMessages("timeout", "", 0)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if res.Total != 1 || len(res.Hits) != 1 {
		t.Fatalf("一致件数 = %d/%d, want 1/1: %+v", res.Total, len(res.Hits), res.Hits)
	}
	if h := res.Hits[0]; h.ID != 2 || h.From != "x" || !h.Current {
		t.Fatalf("一致した発言が想定と違う: %+v", h)
	}

	// 日本語の部分一致と、発言者での絞り込み
	if res, _ = r.SearchMessages("タイムアウト", "", 0); res.Total != 1 {
		t.Fatalf("日本語の一致件数 = %d, want 1", res.Total)
	}
	if res, _ = r.SearchMessages("発言", "human", 0); res.Total != 0 {
		t.Fatalf("発言者で絞れていない: %d", res.Total)
	}

	// limit で打ち切る
	if res, _ = r.SearchMessages("ま", "", 1); !res.Truncated || len(res.Hits) != 1 {
		t.Fatalf("打ち切りが効いていない: truncated=%v hits=%d", res.Truncated, len(res.Hits))
	}

	if _, err := r.SearchMessages("  ", "", 0); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("空の検索語でエラーにならない: %v", err)
	}
}

func TestSnippetAround(t *testing.T) {
	// 改行と連続する空白を詰めて1行にする
	if got := snippetAround("前\n\n中  後", "中"); got != "前 中 後" {
		t.Fatalf("snippet = %q, want %q", got, "前 中 後")
	}
	// 長い本文は一致箇所の前後だけを切り出し、省略記号を付ける
	long := strings.Repeat("あ", 200) + "鍵" + strings.Repeat("い", 200)
	got := snippetAround(long, "鍵")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("省略記号が付いていない: %q", got)
	}
	if n := len([]rune(got)); n != snippetContext*2+1+2 {
		t.Fatalf("切り出しの長さ = %d, want %d", n, snippetContext*2+3)
	}
	if !strings.Contains(got, "鍵") {
		t.Fatalf("一致箇所が含まれていない: %q", got)
	}
}

func TestExportMarkdown(t *testing.T) {
	r := newSearchRoom(t)
	r.mu.Lock()
	r.postLocked("human", "おはよう", "chat")
	r.appendMessageLocked(Message{From: "x", Kind: "chat", Text: "はい", Model: "opus", ReadUpTo: 1})
	r.postLocked("system", "システムの通知", "system")
	name := filepath.Base(r.logFile)
	r.mu.Unlock()

	md, err := r.ExportMarkdown(name)
	if err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	for _, want := range []string{"# 会話ログ " + name, "## #1 人間", "## #2 X（opus）", "#1 まで読了", "［system］", "おはよう", "はい"} {
		if !strings.Contains(md, want) {
			t.Fatalf("書き出しに %q が含まれていない:\n%s", want, md)
		}
	}
	if !strings.Contains(md, "- 発言数: 2（システムメッセージを除く）/ 全 3 件") {
		t.Fatalf("件数の行が想定と違う:\n%s", md)
	}

	// ファイル名以外は受け付けない（ログディレクトリの外を読ませない）
	for _, bad := range []string{"..\\..\\CLAUDE.md", "../secret", "chat-20260101-000000.jsonl"} {
		if _, err := r.ExportMarkdown(bad); !errors.Is(err, ErrLogNotFound) {
			t.Fatalf("%q が拒否されない: %v", bad, err)
		}
	}
	// 空のログは書き出せない
	empty := "chat-20260102-030405.jsonl"
	if err := os.WriteFile(filepath.Join(r.logDir, empty), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExportMarkdown(empty); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("空のログが拒否されない: %v", err)
	}
}

// 上限を超える一致を見つけたら、残りのログは探さずに打ち切ったことを示す（案4）
func TestSearchStopsAtLimit(t *testing.T) {
	r := newSearchRoom(t)
	for i, name := range []string{"chat-20260101-000000.jsonl", "chat-20260102-000000.jsonl", "chat-20260103-000000.jsonl"} {
		line := `{"id":1,"from":"human","kind":"chat","text":"共通の語 ` + string(rune('A'+i)) + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(r.logDir, name), []byte(line+line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := r.SearchMessages("共通の語", "", 3)
	if len(res.Hits) != 3 || !res.Truncated || !res.TotalPartial || res.Total != 4 || res.Logs != 2 {
		t.Fatalf("hits=%d truncated=%v partial=%v total=%d logs=%d", len(res.Hits), res.Truncated, res.TotalPartial, res.Total, res.Logs)
	}
	// 上限に届かなければ、すべてのログを探して打ち切りを示さない
	res, _ = r.SearchMessages("共通の語", "", 10)
	if len(res.Hits) != 6 || res.Truncated || res.TotalPartial || res.Total != 6 {
		t.Fatalf("hits=%d truncated=%v partial=%v total=%d", len(res.Hits), res.Truncated, res.TotalPartial, res.Total)
	}
}

// JSON で書き換わる文字（< > & " \ 改行）を含む語も見つける。行のままでの絞り込みで落とさない（案4）
func TestSearchEscapedChars(t *testing.T) {
	r := newSearchRoom(t)
	r.mu.Lock()
	r.postLocked("human", `if a < b && c > "d" { path := "C:\tmp" }`, "chat")
	r.mu.Unlock()
	for _, q := range []string{"a < b", "&&", `"d"`, `C:\tmp`, "PATH :="} {
		if res, _ := r.SearchMessages(q, "", 0); res.Total != 1 {
			t.Errorf("%q が見つからない: %d", q, res.Total)
		}
	}
}

// 一覧の要約は、ファイルが変わらないあいだは覚えたものを使い、変わったら読み直す（案4）
func TestListLogsCache(t *testing.T) {
	r := newSearchRoom(t)
	path := filepath.Join(r.logDir, "chat-20260101-000000.jsonl")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"id":1,"from":"human","kind":"chat","text":"`+text+`"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	title := func() string {
		t.Helper()
		list, err := r.ListLogs()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range list {
			if l.Name == filepath.Base(path) {
				return l.Title
			}
		}
		return ""
	}
	write("最初の題名")
	if got := title(); got != "最初の題名" {
		t.Fatalf("題名 = %q", got)
	}
	logSummaryCache.Lock()
	c := logSummaryCache.m[path]
	c.s.Title = "覚えた題名" // 覚えたものが使われることを確かめるため書き換える
	logSummaryCache.m[path] = c
	logSummaryCache.Unlock()
	if got := title(); got != "覚えた題名" {
		t.Fatalf("キャッシュが使われていない: %q", got)
	}
	write("変更後の長い題名です") // サイズが変わる
	if got := title(); got != "変更後の長い題名です" {
		t.Fatalf("変更後に読み直していない: %q", got)
	}
}
