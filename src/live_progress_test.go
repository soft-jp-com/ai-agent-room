package main

import (
	"context"
	"os"
	"testing"
)

// 実機確認用: AI_AGENT_ROOM_LIVE=1 go test -run TestLiveProgress -v
// 各 CLI にファイルを読ませ、途中経過（ツール名）と返答・会話ID・モデル・使用量が取れることを確かめる
func TestLiveProgress(t *testing.T) {
	if os.Getenv("AI_AGENT_ROOM_LIVE") != "1" {
		t.Skip()
	}
	dir := t.TempDir()
	os.WriteFile(dir+"/note.txt", []byte("secret word: pineapple"), 0o644)
	for _, ad := range []Adapter{newClaudeAdapter(), newCodexAdapter(), newAgyAdapter()} {
		var got []string
		ctx := withProgress(context.Background(), func(s string) { got = append(got, s) })
		res, err := ad.Run(ctx, "Read the file note.txt in the current directory with your file reading tool and reply with the secret word only.", "", "", dir)
		t.Logf("%T err=%v text=%q session=%q model=%q usage=%+v progress=%q", ad, err, res.Text, res.SessionID, res.Model, res.Usage, got)
	}
}
