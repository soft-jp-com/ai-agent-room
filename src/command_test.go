package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractCodeBlocks(t *testing.T) {
	text := "説明\n```powershell\nGet-Date\n```\n途中\n```\nno lang\n```\n```JSON {\"a\":1}\n{}\n```\n```bash\n閉じていない"
	got := extractCodeBlocks(text)
	want := []CodeBlock{{"powershell", "Get-Date", false}, {"", "no lang", false}, {"json", "{}", false}}
	if len(got) != len(want) {
		t.Fatalf("ブロック数 %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
	// render() と同じく、1行のフェンスは言語指定だけで本文は空になる
	if b := extractCodeBlocks("```ps echo```"); len(b) != 1 || b[0].Lang != "ps" || b[0].Code != "" {
		t.Errorf("1行のフェンス: %#v", b)
	}
}

func TestMessageBlocksOnlyForChat(t *testing.T) {
	r, _ := newTestRoom(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.postLocked("x", "```cmd\necho 1\n```", "chat"); len(m.Blocks) != 1 {
		t.Fatalf("chat の発言に Blocks がない: %#v", m.Blocks)
	}
	if m := r.postLocked("system", "```cmd\necho 1\n```", "system"); m.Blocks != nil {
		t.Fatalf("system の発言に Blocks が付いた")
	}
}

// waitCommand は実行が終わるまで待ち、最終状態を返す
func waitCommand(t *testing.T, r *Room, msgID, block int) CommandRun {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		c := r.commands[commandKey(msgID, block)]
		var st CommandRun
		if c != nil {
			st = *c
		}
		r.mu.Unlock()
		if c != nil && st.State != "running" {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("コマンドが終わらない")
	return CommandRun{}
}

func postChat(r *Room, from, text string) Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.postLocked(from, text, "chat")
}

func lastMessage(r *Room) Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messages[len(r.messages)-1]
}

func TestRunBlock(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で実行するため Windows のみ")
	}
	r, _ := newTestRoom(t)
	m := postChat(r, "human", "```json\n{}\n```\n```cmd\necho hello\nexit /b 3\n```")

	if _, err := r.RunBlock(m.ID, 0, CommandRequest{}); !errors.Is(err, ErrCommandNotRunable) {
		t.Fatalf("json のブロックを実行した: %v", err)
	}
	if _, err := r.RunBlock(m.ID, 5, CommandRequest{}); !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("存在しないブロック: %v", err)
	}
	if _, err := r.RunBlock(m.ID+100, 0, CommandRequest{}); !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("存在しない発言: %v", err)
	}
	if _, err := r.RunBlock(m.ID, 1, CommandRequest{}); err != nil {
		t.Fatal(err)
	}
	st := waitCommand(t, r, m.ID, 1)
	if st.State != "failed" || st.ExitCode == nil || *st.ExitCode != 3 || !strings.Contains(st.Tail, "hello") {
		t.Fatalf("状態 %#v", st)
	}
	res := lastMessage(r)
	if res.Kind != "command_result" || res.ReplyTo != m.ID+1 || !strings.Contains(res.Text, "終了コード 3") || !strings.Contains(res.Text, "hello") {
		t.Fatalf("結果の発言 %#v", res)
	}
	if len(res.Blocks) != 0 {
		t.Fatal("結果の発言のブロックが実行できてしまう")
	}
	// 1回かぎり
	if _, err := r.RunBlock(m.ID, 1, CommandRequest{}); !errors.Is(err, ErrCommandUsed) {
		t.Fatalf("2回目の実行を受け付けた: %v", err)
	}
}

func TestRunBlockRules(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で実行するため Windows のみ")
	}
	r, _ := newTestRoom(t)
	m := postChat(r, "x", "```cmd\necho 1\n```")

	// 進行役以外の発言は確認が要る
	if _, err := r.RunBlock(m.ID, 0, CommandRequest{}); !errors.Is(err, ErrCommandConfirm) {
		t.Fatalf("確認なしで実行した: %v", err)
	}
	// 進行役の発言だけの設定では、確認しても実行しない
	r.SetCommandLeaderOnly(true)
	if _, err := r.RunBlock(m.ID, 0, CommandRequest{Confirm: true}); !errors.Is(err, ErrCommandForbidden) {
		t.Fatalf("進行役以外の発言を実行した: %v", err)
	}
	r.SetCommandLeaderOnly(false)
	// 期限切れ
	r.mu.Lock()
	r.messages[len(r.messages)-1].TS = time.Now().Add(-commandExpiry - time.Minute).UnixMilli()
	r.mu.Unlock()
	if _, err := r.RunBlock(m.ID, 0, CommandRequest{Confirm: true}); !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("期限切れを実行した: %v", err)
	}
	// 存在しない作業フォルダ
	m2 := postChat(r, "human", "```cmd\necho 1\n```")
	if _, err := r.RunBlock(m2.ID, 0, CommandRequest{Cwd: `Z:\no\such\dir`}); !errors.Is(err, ErrInvalidWorkdir) {
		t.Fatalf("存在しない作業フォルダ: %v", err)
	}
	// 同時に commandMaxRunning 件まで
	r.mu.Lock()
	for i := range commandMaxRunning {
		r.cmdRunning[commandKey(-1, i)] = &CommandRun{}
	}
	r.mu.Unlock()
	if _, err := r.RunBlock(m2.ID, 0, CommandRequest{}); !errors.Is(err, ErrCommandBusy) {
		t.Fatalf("上限を超えてコマンドを受け付けた: %v", err)
	}
	// 同じキーが前の会話から実行中なら、上限に達していなくても受け付けない（発言IDは会話ごとに振り直す）
	r.mu.Lock()
	clear(r.cmdRunning)
	r.cmdRunning[commandKey(m2.ID, 0)] = &CommandRun{}
	r.mu.Unlock()
	if _, err := r.RunBlock(m2.ID, 0, CommandRequest{}); !errors.Is(err, ErrCommandBusy) {
		t.Fatalf("前の会話の同じキーが実行中なのに受け付けた: %v", err)
	}
}

// 案12: 複数のブロックを同時に実行でき、中止は指定した1件だけに効く。「新しい会話」では実行中のすべてを止める
func TestRunBlockParallel(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で実行するため Windows のみ")
	}
	r, _ := newTestRoom(t)
	long := "```cmd\nping -n 30 127.0.0.1 >nul\n```"
	m := postChat(r, "human", long+"\n"+long+"\n"+long)
	for i := range 2 {
		if _, err := r.RunBlock(m.ID, i, CommandRequest{}); err != nil {
			t.Fatalf("ブロック%d: %v", i+1, err)
		}
	}
	r.mu.Lock()
	n := len(r.cmdRunning)
	r.mu.Unlock()
	if n != 2 {
		t.Fatalf("同時に実行中の数 %d, want 2", n)
	}
	time.Sleep(300 * time.Millisecond)
	if err := r.CancelBlock(m.ID, 0); err != nil {
		t.Fatal(err)
	}
	if st := waitCommand(t, r, m.ID, 0); st.State != "canceled" {
		t.Fatalf("中止したブロックの状態 %#v", st)
	}
	r.mu.Lock()
	other := r.commands[commandKey(m.ID, 1)].State
	_, stillRunning := r.cmdRunning[commandKey(m.ID, 1)]
	r.mu.Unlock()
	if other != "running" || !stillRunning {
		t.Fatalf("中止していないブロックまで止まった: %q", other)
	}
	// 中止した分の枠が空くので、3件目も実行できる
	if _, err := r.RunBlock(m.ID, 2, CommandRequest{}); err != nil {
		t.Fatalf("3件目: %v", err)
	}
	// 「新しい会話」では実行中のすべてを止める
	r.mu.Lock()
	running := make([]*CommandRun, 0, len(r.cmdRunning))
	for _, c := range r.cmdRunning {
		running = append(running, c)
	}
	r.gen++
	r.cancelCommandsLocked()
	r.mu.Unlock()
	if len(running) != 2 {
		t.Fatalf("止める前の実行中の数 %d, want 2", len(running))
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		left := len(r.cmdRunning)
		states := []string{running[0].State, running[1].State}
		r.mu.Unlock()
		if left == 0 && states[0] == "interrupted" && states[1] == "interrupted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("新しい会話で止まらない: 残り %d, 状態 %v", left, states)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunBlockCancelAndPrivate(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で実行するため Windows のみ")
	}
	r, _ := newTestRoom(t)
	priv := t.TempDir()
	old := privateCommandDir
	privateCommandDir = func() string { return priv }
	t.Cleanup(func() { privateCommandDir = old })

	m := postChat(r, "human", "```cmd\necho secret-output\nping -n 30 127.0.0.1 >nul\n```")
	if _, err := r.RunBlock(m.ID, 0, CommandRequest{Private: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := r.CancelBlock(m.ID, 0); err != nil {
		t.Fatal(err)
	}
	st := waitCommand(t, r, m.ID, 0)
	if st.State != "canceled" || st.Tail != "" {
		t.Fatalf("状態 %#v", st)
	}
	res := lastMessage(r)
	if strings.Contains(res.Text, "secret-output") || !strings.Contains(res.Text, "中止") {
		t.Fatalf("private の出力がチャットに出た: %q", res.Text)
	}
	if err := r.CancelBlock(m.ID, 0); !errors.Is(err, ErrCommandNotRunning) {
		t.Fatalf("終わったコマンドを中止できた: %v", err)
	}
}

// 実行開始を system の発言として記録し、エージェントのターンを起こさない。結果はその発言への返信にする（案13）
func TestRunBlockStartMessage(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で実行するため Windows のみ")
	}
	r, a := newTestRoom(t)
	m := postChat(r, "x", "```cmd\necho 1\n```")
	r.mu.Lock()
	a.cursor = len(r.messages) // x は自分の発言まで受け取り済み
	r.mu.Unlock()
	if _, err := r.RunBlock(m.ID, 0, CommandRequest{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	start := r.messages[len(r.messages)-1]
	queued := len(r.queue)
	woke := r.hasNewChatLocked(a)
	r.mu.Unlock()
	if start.From != "system" || start.Kind != "system" || start.ReplyTo != m.ID ||
		!strings.Contains(start.Text, fmt.Sprintf("#%d のブロック 1", m.ID)) || !strings.Contains(start.Text, "cmd") {
		t.Fatalf("開始の発言 %#v", start)
	}
	if queued != 0 || woke {
		t.Fatalf("開始の発言でエージェントを起こした: queue=%d freetalk=%v", queued, woke)
	}
	if runtime.GOOS != "windows" {
		return // 実行結果は cmd が要る
	}
	waitCommand(t, r, m.ID, 0)
	r.mu.Lock()
	var res Message // 結果の通知で x が起きて返答するので、最後の発言とは限らない
	for _, msg := range r.messages {
		if msg.Kind == "command_result" {
			res = msg
		}
	}
	target := r.commandNotifyTargetLocked(res)
	r.mu.Unlock()
	if res.Kind != "command_result" || res.ReplyTo != start.ID || target != "x" {
		t.Fatalf("結果の返信先 %d（開始 %d）/ 呼び出す相手 %q", res.ReplyTo, start.ID, target)
	}
}

func TestCommandNotifyTarget(t *testing.T) {
	r, _ := newTestRoom(t)
	r.mu.Lock()
	r.leader = "x"
	r.mu.Unlock()
	m := postChat(r, "human", "```cmd\necho 1\n```")
	res := Message{Kind: "command_result", ReplyTo: m.ID}
	r.mu.Lock()
	target := r.commandNotifyTargetLocked(res)
	r.leader = ""
	noLeader := r.commandNotifyTargetLocked(res)
	r.mu.Unlock()
	if target != "x" || noLeader != "" {
		t.Fatalf("呼び出す相手 %q / 進行役なし・人間の発言 %q", target, noLeader)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "key AKIAABCDEFGHIJKLMNOP\naws_secret_access_key = abc/DEF+123\n\"password\": \"hunter2\"\njwt eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4\nnormal line"
	out := redactSecrets(in)
	for _, s := range []string{"AKIAABCDEFGHIJKLMNOP", "abc/DEF+123", "hunter2", "SflKxwRJSMeKKF2QT4"} {
		if strings.Contains(out, s) {
			t.Errorf("伏せ字になっていない: %q\n%s", s, out)
		}
	}
	if !strings.Contains(out, "aws_secret_access_key = [伏せ字]") || !strings.Contains(out, "normal line") {
		t.Errorf("キー名や通常の行が残っていない:\n%s", out)
	}
}

func TestCommandTail(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString(strings.Repeat("x", 10) + string(rune('0'+i%10)) + "\r\n")
	}
	got := commandTail(b.String())
	if n := strings.Count(got, "\n"); n != commandTailLines {
		t.Fatalf("行数 %d", n)
	}
	if !strings.HasPrefix(got, "…（先頭の 70 行を省略）") {
		t.Fatalf("省略の表示がない: %q", got[:40])
	}
}

func TestRunScriptShells(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows のシェルを確かめる")
	}
	cases := []struct{ shell, code string }{
		{"pwsh", "Write-Output '日本語の出力'\ncmd /c exit 4"},
		{"cmd", "echo 日本語の出力\nexit /b 4"},
		{"bash", "echo 日本語の出力\nexit 4"},
	}
	for _, c := range cases {
		t.Run(c.shell, func(t *testing.T) {
			if _, _, err := shellCommand(c.shell, "x"); err != nil {
				t.Skip(err)
			}
			var out tailBuffer
			out.max = commandBufBytes
			code, err := runScript(context.Background(), c.shell, c.code, t.TempDir(), &out)
			if err != nil || code != 4 || !strings.Contains(out.String(), "日本語の出力") {
				t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
			}
		})
	}
}

// 末尾だけを残して秘密鍵の BEGIN の行が切れていても、鍵の本体と END の行を伏せる
func TestRedactTruncatedPrivateKey(t *testing.T) {
	in := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nb3J0aGFuZGxlc3NvbWVyYW5kb21iYXNlNjRkYXRhWFla\n-----END PRIVATE KEY-----\nsha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	out := redactSecrets(in)
	for _, s := range []string{"MIIEvQIBADAN", "b3J0aGFuZGxl", "-----END PRIVATE KEY-----"} {
		if strings.Contains(out, s) {
			t.Errorf("伏せ字になっていない: %q\n%s", s, out)
		}
	}
	if !strings.Contains(out, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Errorf("16進のハッシュまで伏せた:\n%s", out)
	}
}

// 色などの ANSI エスケープは取り除く。先頭の空行も詰める
func TestCommandTailStripsANSI(t *testing.T) {
	in := "\n\x1b[32;1mPath\x1b[0m\n\x1b[32;1m----\x1b[0m\n\x1b]0;title\x07work\n"
	got := commandTail(in)
	if strings.Contains(got, "\x1b") || got != "Path\n----\nwork" {
		t.Fatalf("got %q", got)
	}
}

// pwsh の表形式の出力に色が付かない
func TestRunScriptPwshPlainText(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows のシェルを確かめる")
	}
	if _, _, err := shellCommand("pwsh", "x"); err != nil {
		t.Skip(err)
	}
	var out tailBuffer
	out.max = commandBufBytes
	if _, err := runScript(context.Background(), "pwsh", "Get-Location", t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("色の制御シーケンスが出た: %q", out.String())
	}
}
