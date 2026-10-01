package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDetectWrites(t *testing.T) {
	cases := []struct {
		name, line string
		want       []string
	}{
		{"claude Edit", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"C:/w/chat.go"}}]}}`, []string{"C:/w/chat.go"}},
		{"claude Write", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"a.go"}}]}}`, []string{"a.go"}},
		{"claude Read は書き込みではない", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}`, nil},
		{"codex apply_patch", `{"type":"item.completed","item":{"id":"i2","type":"file_change","changes":[{"path":"C:/w/a.go","kind":"update"},{"path":"C:/w/b.go","kind":"add"}],"status":"completed"}}`, []string{"C:/w/a.go", "C:/w/b.go"}},
		{"codex command は書き込みとみなさない", `{"type":"item.started","item":{"type":"command_execution","command":"ls"}}`, nil},
		{"agy write_to_file", `{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"write_to_file","tool_info":{"parameters":{"TargetFile":"C:/w/x.md"}}}}`, []string{"C:/w/x.md"}},
		{"agy replace_file_content", `{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"replace_file_content","tool_info":{"parameters":{"TargetFile":"C:/w/y.go"}}}}`, []string{"C:/w/y.go"}},
		{"agy view_file は書き込みではない", `{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"view_file","tool_info":{"parameters":{"AbsolutePath":"C:/w/y.go"}}}}`, nil},
		{"agy 終了の行は数えない", `{"event":"step_update","step_update":{"state":"DONE","step_type":"tool","tool_name":"write_to_file","tool_info":{"parameters":{"TargetFile":"C:/w/x.md"}}}}`, nil},
	}
	for _, c := range cases {
		if got := detectWrites(c.line); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// editingAdapter は、ファイルへの書き込みを出力してから、テストが release を閉じるまでターンを終えない
type editingAdapter struct {
	line    string
	wrote   chan struct{}
	release chan struct{}
}

func (editingAdapter) Available() bool       { return true }
func (editingAdapter) Models() []ModelOption { return nil }
func (editingAdapter) ModelSource() string   { return "固定定義" }
func (editingAdapter) SelfTool() bool        { return true }
func (e editingAdapter) Run(ctx context.Context, _, _, _, _ string) (TurnResult, error) {
	if pw := progressWriterFrom(ctx); pw != nil {
		pw.Write([]byte(e.line + "\n"))
	}
	close(e.wrote)
	<-e.release
	return TurnResult{Text: "chat.go を直しました"}, nil
}

func TestEditingOwnerLifecycle(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.ToSlash(filepath.Join(workdir, "chat.go"))
	ad := editingAdapter{
		line:    `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"` + target + `"}}]}}`,
		wrote:   make(chan struct{}),
		release: make(chan struct{}),
	}
	x := &Agent{ID: "x", Name: "X", Adapter: ad}
	y := &Agent{ID: "y", Name: "Y", Adapter: fakeAdapter{}}
	r := NewRoom([]*Agent{x, y}, workdir, t.TempDir(), 10, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.mu.Lock()
	r.postLocked("human", "chat.go を直して", "chat")
	r.mu.Unlock()

	done := make(chan turnOutcome)
	go func() { done <- r.runTurn(context.Background(), x) }()
	<-ad.wrote

	promptFor := func(a *Agent) string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.buildPrompt(a, r.messages, "tok", "")
	}
	r.mu.Lock()
	editing := slices.Clone(x.editing)
	st := r.statusLocked()
	r.mu.Unlock()
	if !slices.Equal(editing, []string{"chat.go"}) {
		t.Fatalf("書き込みが担当として記録されていない: %v", editing)
	}
	if !slices.Equal(st.Agents[0].Editing, []string{"chat.go"}) {
		t.Errorf("状態に作業中のファイルが出ていない: %+v", st.Agents[0])
	}
	if p := promptFor(y); !strings.Contains(p, "@x が `chat.go` を作業中。完了報告まで、このファイルの編集とレビューは待ってください。") {
		t.Errorf("ほかのエージェントの依頼文に警告がない:\n%s", p)
	}
	if p := promptFor(x); strings.Contains(p, "作業中のファイル") {
		t.Errorf("本人の依頼文に自分の担当の警告が入っている:\n%s", p)
	}

	close(ad.release)
	if got := <-done; got != outcomeSpoke {
		t.Fatalf("ターンの結果: %v", got)
	}
	r.mu.Lock()
	left := x.editing
	r.mu.Unlock()
	if left != nil {
		t.Errorf("ターンが終わっても担当が外れていない: %v", left)
	}
	if p := promptFor(y); strings.Contains(p, "作業中のファイル") {
		t.Errorf("担当が外れたのに警告が残っている:\n%s", p)
	}
}

func TestEditDisplayPath(t *testing.T) {
	wd := t.TempDir()
	if got := editDisplayPath(wd, filepath.Join(wd, "sub", "a.go")); got != "sub/a.go" {
		t.Errorf("作業ディレクトリの中: %q", got)
	}
	out := filepath.Join(filepath.Dir(wd), "other.go")
	if got := editDisplayPath(wd, out); got != filepath.ToSlash(out) {
		t.Errorf("作業ディレクトリの外: %q", got)
	}
	if runtime.GOOS == "windows" { // \ がパスの区切りになるのは Windows だけ
		if got := editDisplayPath(wd, `sub\b.go`); got != "sub/b.go" {
			t.Errorf("相対パス: %q", got)
		}
	}
}
