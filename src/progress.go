package main

// エージェントの途中経過（実行中のツール）の取り出し。
// 各 CLI の JSON ストリーム出力を1行ずつ読み、ツールの実行開始を見つけたら短い説明にして通知する。
// 返答の本文・使用量は従来どおりプロセス終了後に出力全体から取り出す。

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type progressKey struct{}
type lineParserKey struct{}
type liveSinkKey struct{}

// withProgress は途中経過の通知先を ctx に付ける（Room が設定する）
func withProgress(ctx context.Context, report func(string)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

// withLineParser は出力1行から途中経過を取り出す関数を ctx に付ける（各アダプタが設定する）
func withLineParser(ctx context.Context, parse func(string) string) context.Context {
	return context.WithValue(ctx, lineParserKey{}, parse)
}

// withLiveSink は CLI の出力を1行ずつそのまま受け取る先を ctx に付ける（Room が設定する。出力の別窓表示に使う）
func withLiveSink(ctx context.Context, sink func(stream, line string)) context.Context {
	return context.WithValue(ctx, liveSinkKey{}, sink)
}

// progressWriterFrom は stdout を受け取る Writer を返す。途中経過の通知と出力の転送のどちらも不要なら nil
func progressWriterFrom(ctx context.Context) *progressWriter {
	report, _ := ctx.Value(progressKey{}).(func(string))
	parse, _ := ctx.Value(lineParserKey{}).(func(string) string)
	sink, _ := ctx.Value(liveSinkKey{}).(func(string, string))
	edit, _ := ctx.Value(editSinkKey{}).(func(string))
	partial := partialFrom(ctx) // 返答本文の途中表示（案5）
	if (report == nil || parse == nil) && sink == nil && edit == nil && partial == nil {
		return nil
	}
	return &progressWriter{onLine: func(line string) {
		// Claude の書きかけの本文（1語ごとの stream_event）は別窓に流さない。直近1000行の保持分が埋まるため（案5）
		if sink != nil && !strings.HasPrefix(line, `{"type":"stream_event"`) {
			sink("stdout", line)
		}
		if report != nil && parse != nil {
			if s := parse(line); s != "" {
				report(s)
			}
		}
		if edit != nil {
			for _, p := range detectWrites(line) {
				edit(p)
			}
		}
		if partial != nil {
			partial(line)
		}
	}}
}

// stderrWriterFrom は stderr を出力の転送先へ渡す Writer を返す。転送先がなければ nil
func stderrWriterFrom(ctx context.Context) *progressWriter {
	sink, _ := ctx.Value(liveSinkKey{}).(func(string, string))
	if sink == nil {
		return nil
	}
	return &progressWriter{onLine: func(line string) { sink("stderr", line) }}
}

// progressMaxLine を超える1行（大きなツール結果など）は解析せずに捨てる
const progressMaxLine = 4 * 1024 * 1024

// progressWriter は受け取った出力を行に分けて onLine に渡す
type progressWriter struct {
	onLine func(string)
	buf    []byte
	skip   bool // 長すぎる行の残りを読み飛ばし中
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if w.skip {
			w.skip = false
			continue
		}
		w.onLine(strings.TrimSpace(string(line)))
	}
	if len(w.buf) > progressMaxLine {
		w.buf, w.skip = nil, true
		w.onLine("（長すぎる行を省略しました）")
	}
	return len(p), nil
}

// progressLabel は「ツール名: 詳細」を1行・最大60文字にまとめる
func progressLabel(tool, detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	s := tool
	if detail != "" {
		s += ": " + detail
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// firstString は input のうち最初に見つかった文字列の値を返す
func firstString(input map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// claudeProgress は claude -p --output-format stream-json の assistant 行からツール呼び出しを取り出す
func claudeProgress(line string) string {
	if !strings.Contains(line, `"tool_use"`) {
		return ""
	}
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
		return ""
	}
	label := ""
	for _, c := range ev.Message.Content {
		if c.Type == "tool_use" {
			label = progressLabel(c.Name, firstString(c.Input, "command", "file_path", "pattern", "url", "query", "description"))
		}
	}
	return label
}

// codexProgress は codex exec --json の item.started 行から実行中の作業を取り出す
func codexProgress(line string) string {
	if !strings.Contains(line, `"item.started"`) {
		return ""
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Query   string `json:"query"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "item.started" {
		return ""
	}
	switch it := ev.Item; it.Type {
	case "command_execution":
		return progressLabel("command", it.Command)
	case "mcp_tool_call":
		return progressLabel(it.Server+"."+it.Tool, "")
	case "web_search":
		return progressLabel("web_search", it.Query)
	case "file_change":
		return "file_change"
	}
	return ""
}

// agyProgress は agy --output-format stream-json の step_update 行から実行中のツールを取り出す
func agyProgress(line string) string {
	if !strings.Contains(line, `"step_update"`) {
		return ""
	}
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
		return ""
	}
	su := ev.StepUpdate
	if su.StepType != "tool" || su.State != "ACTIVE" || su.ToolName == "" {
		return ""
	}
	return progressLabel(su.ToolName, firstString(su.ToolInfo.Parameters, "CommandLine", "AbsolutePath", "TargetFile", "Query", "Url"))
}
