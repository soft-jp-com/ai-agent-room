package main

// 発言中のコードブロックを、人間が画面のボタンで実行する（修正案 7.1）。
// 実行 API が受け取るのは発言IDとブロック番号だけで、本文は保存済みの発言（Message.Blocks）から取り直す。
// 1つのブロックを実行できるのは1回だけ。実行は同時に commandMaxRunning 件まで（案12）。結果は伏せ字にしてから投稿し、進行役だけを呼び出す。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	commandExpiry         = time.Hour        // 発言からこの時間を過ぎたブロックは実行しない
	commandDefaultTimeout = 10 * time.Minute // 実行の時間制限の既定
	commandMaxTimeout     = 2 * time.Hour
	commandTailLines      = 30   // 画面とチャットに出す出力の末尾の行数
	commandTailBytes      = 4000 // 同じく最大バイト数
	commandBufBytes       = 64 << 10
	commandMaxRunning     = 3 // 同時に実行できるコマンドの数
)

var (
	ErrCommandNotFound   = errors.New("発言またはブロックが見つかりません")
	ErrCommandNotRunable = errors.New("実行できる言語（powershell、pwsh、cmd、bat、bash、sh）のブロックではありません")
	ErrCommandExpired    = errors.New("発言から1時間を過ぎたため実行できません")
	ErrCommandUsed       = errors.New("このブロックはすでに実行しました。再実行するには新しい発言で出してもらってください")
	ErrCommandBusy       = fmt.Errorf("同時に実行できるコマンドは%d件までです。どれかが終わってから実行してください", commandMaxRunning)
	ErrCommandForbidden  = errors.New("進行役の発言のブロックだけが実行できる設定です")
	ErrCommandConfirm    = errors.New("進行役以外の発言のブロックです。確認してから実行してください")
	ErrCommandNotRunning = errors.New("実行中ではありません")
)

// ErrCommandUnsupportedOS は、Windows 以外で cmd・bat のブロックを実行しようとしたとき
var ErrCommandUnsupportedOS = errors.New("cmd・bat のブロックは Windows でしか実行できません")

// CommandRun はブロック1つの実行状態。画面には status イベントの commands で配信する
type CommandRun struct {
	MsgID    int    `json:"msg_id"`
	Block    int    `json:"block"` // Message.Blocks の添字（0 始まり）
	Shell    string `json:"shell"` // pwsh / cmd / bash
	Cwd      string `json:"cwd"`
	State    string `json:"state"` // running / done / failed / canceled / timeout / interrupted（「新しい会話」で止めた）
	ExitCode *int   `json:"exit_code,omitempty"`
	Started  int64  `json:"started"`
	Ended    int64  `json:"ended,omitempty"`
	Tail     string `json:"tail,omitempty"`  // 伏せ字済みの出力の末尾（private なら空）
	Private  bool   `json:"private"`         // 結果をチャットに出さない
	Error    string `json:"error,omitempty"` // シェルを起動できなかったなどの理由

	gen      int
	startID  int // 実行開始の system の発言のID（案13）。実行結果はこの発言への返信にする
	cancel   context.CancelFunc
	canceled bool // 人間の［中止］または「新しい会話」で止めた
}

// CommandRequest は実行 API の本文
type CommandRequest struct {
	Cwd        string `json:"cwd"`         // 作業フォルダ（空なら会話の作業ディレクトリ）
	Confirm    bool   `json:"confirm"`     // 進行役以外の発言のブロックを、確認のうえ実行する
	Private    bool   `json:"private"`     // 結果（出力）をチャットに出さない
	NoLog      bool   `json:"no_log"`      // 出力の全文ログを残さない
	TimeoutMin int    `json:"timeout_min"` // 時間制限（分）。0 なら既定の10分
}

func commandKey(msgID, block int) string { return fmt.Sprintf("%d:%d", msgID, block) }

// privateCommandDir は「結果をチャットに出さない」コマンドの全文ログの置き場所。
// 作業ディレクトリの外（%LOCALAPPDATA%\ai-agent-room\commands）に置く。テストでは差し替える
var privateCommandDir = func() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, appDirName, "commands")
}

// RunBlock は発言 msgID のブロック block を実行し始める。終了は待たない
func (r *Room) RunBlock(msgID, block int, req CommandRequest) (CommandRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var msg *Message
	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].ID == msgID {
			msg = &r.messages[i]
			break
		}
	}
	if msg == nil || block < 0 || block >= len(msg.Blocks) {
		return CommandRun{}, ErrCommandNotFound
	}
	shell, ok := shellOf[msg.Blocks[block].Lang]
	if !ok {
		return CommandRun{}, ErrCommandNotRunable
	}
	if shell == "cmd" && runtime.GOOS != "windows" { // cmd・bat は Windows にしかない
		return CommandRun{}, ErrCommandUnsupportedOS
	}
	if time.Since(time.UnixMilli(msg.TS)) > commandExpiry {
		return CommandRun{}, ErrCommandExpired
	}
	key := commandKey(msgID, block)
	if _, used := r.commands[key]; used {
		return CommandRun{}, ErrCommandUsed
	}
	// 同じキーが実行中なのは「新しい会話」で止めた前の会話のコマンドが、まだ終わっていない場合（発言IDは会話ごとに振り直す）
	if _, busy := r.cmdRunning[key]; busy || len(r.cmdRunning) >= commandMaxRunning {
		return CommandRun{}, ErrCommandBusy
	}
	if msg.From != "human" && msg.From != r.leader {
		if r.commandLeaderOnly {
			return CommandRun{}, ErrCommandForbidden
		}
		if !req.Confirm {
			return CommandRun{}, ErrCommandConfirm
		}
	}
	cwd := r.workdir
	if strings.TrimSpace(req.Cwd) != "" {
		var err error
		if cwd, err = normalizeWorkdir(req.Cwd); err != nil {
			return CommandRun{}, err
		}
	}
	timeout := commandDefaultTimeout
	if req.TimeoutMin > 0 {
		timeout = min(time.Duration(req.TimeoutMin)*time.Minute, commandMaxTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	run := &CommandRun{MsgID: msgID, Block: block, Shell: shell, Cwd: cwd, State: "running",
		Started: time.Now().UnixMilli(), Private: req.Private, gen: r.gen, cancel: cancel}
	r.commands[key] = run
	r.cmdRunning[key] = run
	logPath := r.commandLogPathLocked(run, req.NoLog)
	r.log.Info("command.start", "msg_id", msgID, "block", block, "shell", shell, "cwd", cwd,
		"from", msg.From, "private", req.Private, "log", logPath != "", "timeout_sec", int(timeout/time.Second))
	// 実行開始をチャットログにも残す（案13）。Kind が system なので、エージェントのターンのきっかけにはしない
	start := r.appendMessageLocked(Message{From: "system", Kind: "system", ReplyTo: msgID,
		Text: fmt.Sprintf("▶ #%d のブロック %d を %s で実行し始めました（作業フォルダ: %s、時間制限: %d分）", msgID, block+1, shell, cwd, int(timeout/time.Minute))})
	run.startID = start.ID
	r.pushCommandLocked(run)
	go r.execCommand(ctx, run, msg.Blocks[block].Code, logPath)
	return *run, nil
}

// CancelBlock は実行中のブロックを中止する
func (r *Room) CancelBlock(msgID, block int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.commands[commandKey(msgID, block)]
	if run == nil || run != r.cmdRunning[commandKey(msgID, block)] {
		return ErrCommandNotRunning
	}
	run.canceled = true // 状態は execCommand がプロセスの終了後に変える
	run.cancel()
	r.log.Info("command.cancel", "msg_id", msgID, "block", block)
	return nil
}

// cancelCommandsLocked は「新しい会話」で実行中のコマンドを止め、実行済みの記録を捨てる
func (r *Room) cancelCommandsLocked() {
	// cmdRunning はプロセスが終わってから execCommand が外す。それまでは同時に実行できる数に数える
	for _, run := range r.cmdRunning {
		run.canceled = true
		run.cancel()
	}
	clear(r.commands)
}

func (r *Room) commandsLocked() []CommandRun {
	if len(r.commands) == 0 {
		return nil
	}
	out := make([]CommandRun, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, *c)
	}
	return out
}

// commandLogPathLocked は出力の全文ログのパスを返す（残さない場合は空）
func (r *Room) commandLogPathLocked(run *CommandRun, noLog bool) string {
	if noLog {
		return ""
	}
	dir := filepath.Join(r.logDir, "commands")
	if run.Private {
		if dir = privateCommandDir(); dir == "" {
			return ""
		}
	}
	name := fmt.Sprintf("cmd-%s-%d-%d.log", time.UnixMilli(run.Started).Format("20060102-150405"), run.MsgID, run.Block+1)
	return filepath.Join(dir, name)
}

// execCommand は本文を一時ファイルに書いて実行し、結果を投稿する
func (r *Room) execCommand(ctx context.Context, run *CommandRun, code, logPath string) {
	out := &tailBuffer{max: commandBufBytes}
	var w io.Writer = out
	var logf *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err == nil {
			logf, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		}
		if logf != nil {
			w = io.MultiWriter(out, logf)
		} else {
			r.log.Warn("command.log", "error", "ログファイルを作成できません")
		}
	}
	exitCode, err := runScript(ctx, run.Shell, code, run.Cwd, w)
	if logf != nil {
		logf.Close()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if key := commandKey(run.MsgID, run.Block); r.cmdRunning[key] == run {
		delete(r.cmdRunning, key)
	}
	run.cancel()
	run.Ended = time.Now().UnixMilli()
	switch {
	case run.canceled && run.gen != r.gen:
		run.State = "interrupted"
	case run.canceled:
		run.State = "canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		run.State = "timeout"
	case err != nil:
		run.State, run.Error = "failed", err.Error()
		fmt.Fprintf(out, "\n[ai-agent-room] 実行できません: %v\n", err)
	case exitCode == 0:
		run.State = "done"
	default:
		run.State = "failed"
	}
	if err == nil && (run.State == "done" || run.State == "failed") {
		run.ExitCode = &exitCode
	}
	if !run.Private {
		run.Tail = commandTail(out.String())
	}
	r.log.Info("command.end", "msg_id", run.MsgID, "block", run.Block, "state", run.State,
		"exit_code", exitCode, "latency_ms", run.Ended-run.Started)
	r.pushCommandLocked(run)
	if run.gen != r.gen { // 「新しい会話」のあとは投稿しない
		return
	}
	m := r.appendMessageLocked(Message{From: "system", Kind: "command_result", Text: formatCommandResult(run), ReplyTo: run.startID})
	r.notifyCommandResultLocked(m)
}

// pushCommandLocked は実行状態の変化を配信する（command イベントと、つなぎ直し用の status の commands）
func (r *Room) pushCommandLocked(run *CommandRun) {
	c := *run
	r.broadcastLocked(Event{Type: "command", Command: &c})
	r.pushStatusLocked()
}

// formatCommandResult は実行結果の発言の本文を作る
func formatCommandResult(run *CommandRun) string {
	var b strings.Builder
	status := map[string]string{"done": "成功", "failed": "失敗", "canceled": "中止", "timeout": "時間切れ"}[run.State]
	fmt.Fprintf(&b, "コマンドの実行結果: #%d のブロック%d（%s、作業フォルダ %s）→ %s", run.MsgID, run.Block+1, run.Shell, run.Cwd, status)
	if run.ExitCode != nil {
		fmt.Fprintf(&b, "、終了コード %d", *run.ExitCode)
	}
	fmt.Fprintf(&b, "（%d秒）", (run.Ended-run.Started)/1000)
	switch {
	case run.Private:
		b.WriteString("\n出力は、人間の指定によりチャットに出していません。")
	case run.Tail == "":
		b.WriteString("\n出力はありません。")
	default:
		// 出力中の ``` でコードブロックが崩れないようにする
		fmt.Fprintf(&b, "\n```text\n%s\n```", strings.ReplaceAll(run.Tail, "```", "`\u200b``"))
	}
	return b.String()
}

// commandNotifyTargetLocked は実行結果の発言で呼び出すエージェントを返す。
// 進行役がいれば進行役、いなければブロックを書いたエージェント（人間の発言なら呼び出さない）
func (r *Room) commandNotifyTargetLocked(m Message) string {
	if m.Kind != "command_result" {
		return ""
	}
	if r.leader != "" {
		return r.leader
	}
	// 返信先は実行開始の発言（案13）。その返信先がブロックを含む元の発言。古いログの結果は元の発言に直接返信している
	id := m.ReplyTo
	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].ID != id {
			continue
		}
		f := r.messages[i].From
		if f == "system" && r.messages[i].ReplyTo != 0 && id == m.ReplyTo {
			id = r.messages[i].ReplyTo
			continue
		}
		if f != "human" && f != "system" {
			return f
		}
		break
	}
	return ""
}

// notifyCommandResultLocked はチャット中なら呼び出す相手を発言待ちに入れる。
// フリートーク中は hasNewChatLocked が相手だけを起こす。ディスカッション中は次の発言者が新着として受け取る
func (r *Room) notifyCommandResultLocked(m Message) {
	target := r.commandNotifyTargetLocked(m)
	if target == "" || r.free != nil || r.disc != nil {
		return
	}
	r.enqueueLocked([]string{target})
}

// ---- 実行 -----------------------------------------------------------------------

// runScript は code を一時ファイルに書き、shell で実行する。stdin は閉じ、出力は UTF-8 に寄せる
func runScript(ctx context.Context, shell, code, cwd string, out io.Writer) (int, error) {
	var ext, body string
	switch shell {
	case "pwsh":
		// BOM 付き UTF-8 にすると Windows PowerShell 5.1 でも日本語を正しく読む
		ext = ".ps1"
		body = utf8BOM + "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\r\n$OutputEncoding = [System.Text.Encoding]::UTF8\r\n" +
			// PowerShell 7.2 以降は表などに色（ANSI エスケープ）を付けるので止める。5.1 には $PSStyle がない
			"if ($PSStyle) { $PSStyle.OutputRendering = 'PlainText' }\r\n" +
			crlf(code) + "\r\nif ($LASTEXITCODE) { exit $LASTEXITCODE }\r\n"
	case "cmd":
		ext = ".cmd"
		body = "@echo off\r\nchcp 65001 >nul\r\n" + crlf(code) + "\r\n"
	case "bash":
		ext = ".sh"
		body = strings.ReplaceAll(code, "\r\n", "\n") + "\n"
	default:
		return -1, fmt.Errorf("未対応のシェルです: %s", shell)
	}
	f, err := os.CreateTemp("", "ai-agent-room-cmd-*"+ext)
	if err != nil {
		return -1, err
	}
	path := f.Name()
	defer os.Remove(path)
	_, err = f.WriteString(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return -1, err
	}
	exe, args, err := shellCommand(shell, path)
	if err != nil {
		return -1, err
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "AWS_PAGER=", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1", "NO_COLOR=1")
	cmd.Stdout, cmd.Stderr = out, out // stdin は nil（NUL デバイス）なので対話待ちで止まらない
	hideWindow(cmd)
	newProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		killTree(cmd)
		<-done
		return -1, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

var utf8BOM = string([]byte{0xEF, 0xBB, 0xBF})

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// shellCommand はシェルの実行ファイルと引数を返す
func shellCommand(shell, path string) (string, []string, error) {
	switch shell {
	case "pwsh":
		exe, err := exec.LookPath("pwsh")
		if err != nil {
			if exe, err = exec.LookPath("powershell"); err != nil {
				return "", nil, errors.New("pwsh と powershell が見つかりません")
			}
		}
		return exe, []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path}, nil
	case "cmd":
		return "cmd", []string{"/d", "/c", path}, nil
	case "bash":
		if runtime.GOOS == "windows" {
			// System32 の bash.exe（WSL）ではなく Git Bash を使う
			for _, p := range []string{os.Getenv("ProgramFiles") + `\Git\bin\bash.exe`, os.Getenv("ProgramW6432") + `\Git\bin\bash.exe`} {
				if _, err := os.Stat(p); err == nil {
					return p, []string{path}, nil
				}
			}
			return "", nil, errors.New("Git Bash が見つかりません")
		}
		return "bash", []string{path}, nil
	}
	return "", nil, fmt.Errorf("未対応のシェルです: %s", shell)
}

// ---- 出力 -----------------------------------------------------------------------

// tailBuffer は出力の末尾 max バイトだけを保持する
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ansiEscapeRe は ANSI のエスケープシーケンス（CSI の色・カーソル移動と、OSC のタイトル設定など）
var ansiEscapeRe = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

// commandTail は出力の末尾を、伏せ字にして行数と長さを制限して返す
func commandTail(s string) string {
	s = ansiEscapeRe.ReplaceAllString(s, "") // 色などの制御シーケンスはチャットでは読めない
	s = redactSecrets(strings.ToValidUTF8(strings.ReplaceAll(s, "\r\n", "\n"), "?"))
	s = strings.Trim(s, "\n ")
	lines := strings.Split(s, "\n")
	if len(lines) > commandTailLines {
		lines = append([]string{fmt.Sprintf("…（先頭の %d 行を省略）", len(lines)-commandTailLines)}, lines[len(lines)-commandTailLines:]...)
	}
	s = strings.Join(lines, "\n")
	if len(s) > commandTailBytes {
		s = "…" + strings.ToValidUTF8(s[len(s)-commandTailBytes:], "")
	}
	return s
}

var secretRes = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[伏せ字:AWSキー]"},
	{regexp.MustCompile(`\beyJ[\w-]{8,}\.[\w-]{8,}\.[\w-]{8,}`), "[伏せ字:JWT]"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[伏せ字:秘密鍵]"},
	// キー名は残し、値だけを伏せる（aws_secret_access_key = xxx、"password": "xxx"、SECRET=xxx など）
	{regexp.MustCompile(`(?i)((?:aws_secret_access_key|aws_session_token|secret[\w-]*|password|passwd|pwd|token|api[_-]?key)["']?\s*[:=]\s*["']?)[^\s"',;]+`), "${1}[伏せ字]"},
}

// 出力の末尾だけを残すと秘密鍵の BEGIN の行が切れていることがあるので、END の行と、Base64 だけの長い行も伏せる
var (
	pemEndRe      = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
	base64LineRe  = regexp.MustCompile(`(?m)^[ \t]*[A-Za-z0-9+/]{40,}={0,2}[ \t]*$`)
	mixedCaseLine = regexp.MustCompile(`[a-z][\s\S]*[A-Z]|[A-Z][\s\S]*[a-z]`)
)

// redactSecrets は出力中の代表的な秘密のパターンを伏せ字にする
func redactSecrets(s string) string {
	for _, p := range secretRes {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	s = pemEndRe.ReplaceAllString(s, "[伏せ字:秘密鍵]")
	// 大文字と小文字が混ざる行だけを対象にし、16進のハッシュ（小文字だけ）などは残す
	return base64LineRe.ReplaceAllStringFunc(s, func(line string) string {
		if mixedCaseLine.MatchString(line) {
			return "[伏せ字:Base64]"
		}
		return line
	})
}
