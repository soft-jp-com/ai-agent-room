package main

import (
	"context"
	"strings"
	"testing"
)

func TestProgressParsers(t *testing.T) {
	cases := []struct {
		name  string
		parse func(string) string
		line  string
		want  string
	}{
		{"claude", claudeProgress, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...","description":"Run tests"}}]}}`, "Bash: go test ./..."},
		{"claude text", claudeProgress, `{"type":"assistant","message":{"content":[{"type":"text","text":"tool_use"}]}}`, ""},
		{"codex", codexProgress, `{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"powershell -c ls","status":"in_progress"}}`, "command: powershell -c ls"},
		{"codex done", codexProgress, `{"type":"item.completed","item":{"type":"command_execution","command":"ls"}}`, ""},
		{"agy", agyProgress, `{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"echo hi"}}}}`, "run_command: echo hi"},
		{"agy done", agyProgress, `{"event":"step_update","step_update":{"state":"DONE","step_type":"tool","tool_name":"run_command"}}`, ""},
	}
	for _, c := range cases {
		if got := c.parse(c.line); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := progressLabel("Bash", strings.Repeat("x", 100)); len([]rune(got)) != 60 {
		t.Errorf("長い説明が60文字に切り詰められていない: %d", len([]rune(got)))
	}
}

// 行が分割して届いても1行ずつ通知する
func TestProgressWriter(t *testing.T) {
	var got []string
	ctx := withLineParser(withProgress(context.Background(), func(s string) { got = append(got, s) }), agyProgress)
	w := progressWriterFrom(ctx)
	line := `{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"view_file","tool_info":{"parameters":{"AbsolutePath":"C:/a.go"}}}}` + "\n"
	w.Write([]byte(line[:30]))
	w.Write([]byte(line[30:] + "not json\n"))
	if len(got) != 1 || got[0] != `view_file: C:/a.go` {
		t.Fatalf("got %q", got)
	}
	if progressWriterFrom(context.Background()) != nil {
		t.Fatal("通知先がないのに Writer を返した")
	}
}

func TestParseAgyResult(t *testing.T) {
	j, ok := parseAgyResult(`{"event":"init","conversation_id":"c1"}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"done\n"}}`)
	if !ok || j.ConversationID != "c1" || j.Status != "SUCCESS" || j.Response != "done\n" {
		t.Fatalf("%+v %v", j, ok)
	}
	if _, ok := parseAgyResult("no result"); ok {
		t.Fatal("結果行がないのに解析できた")
	}
	if u := j.turnUsage; u.Known {
		t.Fatalf("usage がないのに Known: %+v", u)
	}
}

// 実際の agy の出力（2026-09-25 に取得）の形で使用量を読む。
// result 行の usage は会話の始めからの累計なので使わず、このターンの agent_response（DONE）を足す
func TestParseAgyUsage(t *testing.T) {
	j, ok := parseAgyResult(`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"ACTIVE","step_type":"agent_response","text_delta":"a"}}
{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":3509,"output_tokens":493,"thinking_tokens":220,"cache_read_tokens":107326,"total_tokens":4002}}}
{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":3509,"output_tokens":493,"thinking_tokens":220,"cache_read_tokens":107326,"total_tokens":4002}}}
{"event":"step_update","step_update":{"conversation_id":"c1","step_index":3,"state":"DONE","step_type":"tool","tool_name":"view_file"}}
{"event":"step_update","step_update":{"conversation_id":"c1","step_index":4,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":1200,"output_tokens":50,"thinking_tokens":49,"cache_read_tokens":110000,"total_tokens":1250}}}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"hello\n","num_turns":51,"usage":{"input_tokens":765921,"output_tokens":39000,"thinking_tokens":26900,"cache_read_tokens":5000000,"total_tokens":804921}}}`)
	if !ok {
		t.Fatal("解析できない")
	}
	u := j.turnUsage
	want := Usage{Known: true, Input: 3509 + 107326 + 1200 + 110000, CachedInput: 107326 + 110000, Output: 543, Context: 111200}
	if u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
}
