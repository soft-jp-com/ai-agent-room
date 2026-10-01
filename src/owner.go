package main

// 作業中のファイルの記録（案 1・案 3）: エージェントの CLI 出力でファイルへの書き込み
// （Edit / Write / apply_patch など）を見つけたら、そのファイルをエージェントの担当として記録する。
// 担当のあるエージェントは「作業中」とし、ほかのエージェントのプロンプトに、
// 完了報告までそのファイルの編集とレビューを待つよう書き添える。担当はそのエージェントのターンが終わると外す。
//
// ほかのエージェントに伝わるのは、そのエージェントのターンが始まる時点の担当だけ
// （すでに実行中のエージェントには、あとから見つかった担当は伝わらない）。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type editSinkKey struct{}

// withEditSink はファイルへの書き込みを見つけたときの通知先を ctx に付ける（Room が設定する）
func withEditSink(ctx context.Context, sink func(path string)) context.Context {
	return context.WithValue(ctx, editSinkKey{}, sink)
}

// claudeWriteTools は Claude CLI でファイルを書き換えるツール
var claudeWriteTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// detectWrites は CLI の出力1行から、書き込み先のファイルを取り出す（Claude / Codex / agy の形式を順に試す）
func detectWrites(line string) []string {
	switch {
	case strings.Contains(line, `"tool_use"`):
		return claudeWrites(line)
	case strings.Contains(line, `"file_change"`):
		return codexWrites(line)
	case strings.Contains(line, `"step_update"`):
		return agyWrites(line)
	}
	return nil
}

func claudeWrites(line string) []string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "assistant" {
		return nil
	}
	var out []string
	for _, c := range ev.Message.Content {
		if c.Type == "tool_use" && claudeWriteTools[c.Name] {
			if p := firstString(c.Input, "file_path", "notebook_path"); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// codexWrites は codex exec --json の file_change 項目（apply_patch による変更）から変更先を取り出す
func codexWrites(line string) []string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil || !strings.HasPrefix(ev.Type, "item.") || ev.Item.Type != "file_change" {
		return nil
	}
	var out []string
	for _, c := range ev.Item.Changes {
		if c.Path != "" {
			out = append(out, c.Path)
		}
	}
	return out
}

// agyWrites は agy の step_update 行のうち、ファイルを書き換えるツール
// （write_to_file / replace_file_content など、名前に write・replace・edit を含むもの）から変更先を取り出す
func agyWrites(line string) []string {
	var ev struct {
		Event      string `json:"event"`
		StepUpdate struct {
			State    string `json:"state"`
			StepType string `json:"step_type"`
			ToolName string `json:"tool_name"`
			ToolInfo struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"tool_info"`
		} `json:"step_update"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Event != "step_update" {
		return nil
	}
	su := ev.StepUpdate
	name := strings.ToLower(su.ToolName)
	if su.StepType != "tool" || su.State != "ACTIVE" ||
		!(strings.Contains(name, "write") || strings.Contains(name, "replace") || strings.Contains(name, "edit")) {
		return nil
	}
	if p := firstString(su.ToolInfo.Parameters, "TargetFile", "AbsolutePath"); p != "" {
		return []string{p}
	}
	return nil
}

// editDisplayPath は書き込み先を、作業ディレクトリの中なら相対パス（/ 区切り）にする
func editDisplayPath(workdir, p string) string {
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p))
	}
	if workdir != "" {
		if rel, err := filepath.Rel(workdir, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// addEditingLocked は a の担当に path を加える。新しく加えたら true
func (a *Agent) addEditingLocked(path string) bool {
	for _, p := range a.editing {
		if strings.EqualFold(p, path) {
			return false
		}
	}
	a.editing = append(a.editing, path)
	return true
}

// writeEditingLocked は、ほかのエージェントが作業中のファイルをプロンプトに書き添える（なければ何も書かない）
func (r *Room) writeEditingLocked(b *strings.Builder, a *Agent) {
	var lines []string
	for _, o := range r.agents {
		if o.ID == a.ID || len(o.editing) == 0 {
			continue
		}
		files := make([]string, len(o.editing))
		for i, p := range o.editing {
			files[i] = "`" + p + "`"
		}
		lines = append(lines, fmt.Sprintf("- @%s が %s を作業中。完了報告まで、このファイルの編集とレビューは待ってください。", o.ID, strings.Join(files, "、")))
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("作業中のファイル（AI Agent Room が CLI の出力から検出）:\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n\n")
}
