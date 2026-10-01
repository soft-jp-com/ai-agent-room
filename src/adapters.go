package main

// 各CLIエージェントを非対話モードで1ターン実行するアダプタ。
// どのCLIも会話IDで前回の続きから再開できるので、毎ターン全履歴を送る必要はない。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// エラーコード（APIレスポンス・ログで共通）
const (
	ErrAgentUnavailable = "AGENT_UNAVAILABLE"
	ErrAgentFailed      = "AGENT_FAILED"
	ErrAgentQuota       = "AGENT_QUOTA" // 利用枠の上限（CLI のエラーとして出た部分だけで判定する）
	ErrAgentBadOutput   = "AGENT_BAD_OUTPUT"
	ErrAgentCanceled    = "AGENT_CANCELED"
	ErrAgentTimeout     = "AGENT_TIMEOUT"
)

type AgentError struct {
	Code string
	Msg  string
}

func (e *AgentError) Error() string { return e.Msg }

type TurnResult struct {
	Text      string
	SessionID string
	Model     string // 発言したモデル（取得できなかった場合は空）
	Usage     Usage
}

// Usage は1ジョブ（CLI の1回の起動）のトークン使用量
type Usage struct {
	Known       bool // CLI の出力から取得できたか
	Input       int  // 入力トークン（キャッシュ分を含む）
	CachedInput int  // 入力のうちキャッシュから読んだ分
	Output      int  // 出力トークン
	Context     int  // 最後の API 呼び出しの入力トークン（その時点の文脈の大きさ。キャッシュ分を含む。取得できなければ 0）
}

// ModelOption は画面で選べるモデルの候補
type ModelOption struct {
	ID    string `json:"id"`    // CLI の --model に渡す値
	Label string `json:"label"` // 表示名
}

type Adapter interface {
	Available() bool
	// Models は選べるモデルの候補を返す（空なら既定のモデルのみ）
	Models() []ModelOption
	// ModelSource は候補の取得元（固定定義 / キャッシュ / CLI照会）。アカウントで利用できると確認済みとは限らない
	ModelSource() string
	// SelfTool は自己管理ツール（AI Agent Room の MCP サーバ）を起動時に接続できるか
	SelfTool() bool
	// Run は1ターン実行する。model が空なら CLI の既定のモデルを使う
	Run(ctx context.Context, prompt, sessionID, model, cwd string) (TurnResult, error)
}

// modelIDRe は CLI に渡してよいモデル名。引数の注入を防ぐため '-' 始まりは許可しない
var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,99}$`)

// 1ターン（CLI の1回の起動）の上限時間。会話全体とエージェントごとに設定で変えられる（settings.go）
const (
	defaultTurnTimeout = 10 * time.Minute
	minTurnTimeout     = 1 * time.Minute
	maxTurnTimeout     = 60 * time.Minute
)

type turnTimeoutKey struct{}

// withTurnTimeout は1ターンの上限時間を ctx に付ける（Room が設定する）
func withTurnTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, turnTimeoutKey{}, d)
}

// turnTimeoutFrom は ctx に付いた上限時間を返す。付いていなければ既定値
func turnTimeoutFrom(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(turnTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTurnTimeout
}

// clampTurnTimeout は設定値（秒）を上限時間に直す。0 以下なら既定値、範囲外は丸める
func clampTurnTimeout(sec int) time.Duration {
	if sec <= 0 {
		return defaultTurnTimeout
	}
	return min(max(time.Duration(sec)*time.Second, minTurnTimeout), maxTurnTimeout)
}

// extraArgs は環境変数（例: CLAUDE_ARGS="--model sonnet"）から追加引数を読む
func extraArgs(env string) []string { return strings.Fields(os.Getenv(env)) }

type procResult struct {
	stdout, stderr string
	exitCode       int
}

// runProcess はコマンドを実行する。ctx がキャンセルされたらプロセスツリーごと終了させる。
func runProcess(ctx context.Context, name string, args []string, stdin, cwd string) (procResult, error) {
	timeout := turnTimeoutFrom(ctx)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE") // Claude Code 内から起動された場合の入れ子判定を避ける
	hideWindow(cmd)
	newProcessGroup(cmd) // 中止・時間切れのとき、孫プロセスまでまとめて終了させるため（Windows 以外）
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if pw := progressWriterFrom(ctx); pw != nil { // 途中経過の通知と、出力の別窓表示への転送
		cmd.Stdout = io.MultiWriter(&out, pw)
	}
	if ew := stderrWriterFrom(ctx); ew != nil {
		cmd.Stderr = io.MultiWriter(&errb, ew)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		return procResult{}, &AgentError{ErrAgentUnavailable, fmt.Sprintf("%s を起動できません: %v", name, err)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			return procResult{}, &AgentError{ErrAgentFailed, err.Error()}
		}
		return procResult{out.String(), errb.String(), code}, nil
	case <-ctx.Done():
		killTree(cmd)
		<-done
		// 終了までに受信した出力も返す（使用量の集計に使う。本文は採用しない）
		partial := procResult{out.String(), errb.String(), -1}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return partial, &AgentError{ErrAgentTimeout, fmt.Sprintf("%v 以内に応答がありませんでした", timeout)}
		}
		return partial, &AgentError{ErrAgentCanceled, "中断されました"}
	}
}

func filterEnv(env []string, drop string) []string {
	res := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), drop+"=") {
			res = append(res, kv)
		}
	}
	return res
}

// cliFailure は CLI がエラーとして返した文面から失敗を作る。利用枠の上限なら ErrAgentQuota にする。
// 返答本文やツールの結果（stdout の写し）は判定に使わない（会話の中身に「usage limit」などが含まれ得るため）
func cliFailure(msg string) *AgentError {
	if isQuotaError(msg) {
		return &AgentError{ErrAgentQuota, msg}
	}
	return &AgentError{ErrAgentFailed, msg}
}

func procFailure(name string, r procResult) error {
	tail := strings.TrimSpace(r.stderr)
	if tail == "" {
		tail = strings.TrimSpace(r.stdout)
	}
	lines := strings.Split(tail, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	msg := fmt.Sprintf("%s が失敗しました (exit %d)\n%s", name, r.exitCode, strings.Join(lines, "\n"))
	if isQuotaError(r.stderr) { // stdout の写しでは判定しない
		return &AgentError{ErrAgentQuota, msg}
	}
	return &AgentError{ErrAgentFailed, msg}
}

// ---- Claude Code ------------------------------------------------------------

type claudeAdapter struct{ bin string }

func newClaudeAdapter() *claudeAdapter {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("claude")
	}
	return &claudeAdapter{bin}
}

func (a *claudeAdapter) Available() bool { return a.bin != "" }

// Claude Code はモデルの一覧を出力するコマンドがないため、最新モデルを指す別名を候補にする
func (a *claudeAdapter) Models() []ModelOption {
	return []ModelOption{
		{"opus", "Opus（最新）"},
		{"sonnet", "Sonnet（最新）"},
		{"haiku", "Haiku（最新）"},
		{"fable", "Fable（最新）"},
	}
}

func (a *claudeAdapter) ModelSource() string { return "固定定義" }
func (a *claudeAdapter) SelfTool() bool      { return true }

// runArgs は非対話の1ターンの引数を返す。permission は権限の段階（案8）
func (a *claudeAdapter) runArgs(sessionID, model, permission string) []string {
	// stream-json は途中経過（ツール呼び出し）を1行ずつ出し、最後の type=result 行が json 形式と同じ内容になる
	// --include-partial-messages は書きかけの本文（text_delta）も出す。途中表示（案5）に使い、確定した本文は result 行から取る
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if selfToolURL != "" {
		// 可変長のオプションが後続の引数を取り込まないよう「=」で渡す
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"ai_agent_room": map[string]any{"type": "http", "url": selfToolURL}}})
		args = append(args, "--mcp-config="+string(cfg), "--allowedTools="+claudeAllowedTools())
	}
	if s := claudeDenySettings(); s != "" {
		// 認証トークン・人間が編集する設定を読み書きさせない（settings.json に手で deny を足さなくてよいように）
		args = append(args, "--settings="+s)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	// *_ARGS に権限の段階（案8）を反映する
	return append(args, argsWithPermission(a, "CLAUDE_ARGS", permission)...)
}

func (a *claudeAdapter) Run(ctx context.Context, prompt, sessionID, model, cwd string) (TurnResult, error) {
	args := a.runArgs(sessionID, model, permissionFrom(ctx))
	ctx = withPartialParser(withLineParser(ctx, claudeProgress), newClaudePartial())
	r, err := runProcess(ctx, a.bin, args, prompt, cwd) // プロンプトは stdin で渡す
	j, parsed := parseClaudeOutput(r.stdout)
	model, usage := j.modelAndUsage()
	usage.Context = claudeContextTokens(r.stdout)
	// 失敗・キャンセルでも、出力から取得できた使用量は集計に回す
	if err != nil {
		return TurnResult{Usage: usage}, err
	}
	if !parsed {
		return TurnResult{}, procFailure("claude", r)
	}
	if j.IsError {
		return TurnResult{Usage: usage}, cliFailure("claude: " + firstNonEmpty(j.Result, j.Subtype))
	}
	return TurnResult{j.Result, j.SessionID, model, usage}, nil
}

type claudeOutput struct {
	Type      string `json:"type"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Subtype   string `json:"subtype"`
	// モデル別の使用量。サブエージェントが別モデルを使うこともあるので出力トークンが最大のものを採る
	ModelUsage map[string]struct {
		InputTokens              int `json:"inputTokens"`
		OutputTokens             int `json:"outputTokens"`
		CacheReadInputTokens     int `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

// parseClaudeOutput は stream-json 出力の最後の type=result 行を読む
func parseClaudeOutput(stdout string) (claudeOutput, bool) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var j claudeOutput
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &j) == nil && j.Type == "result" {
			return j, true
		}
	}
	return claudeOutput{}, false
}

// claudeContextTokens は stream-json 出力の最後の assistant 行（サブエージェント分を除く）から、
// その API 呼び出しの入力トークンを返す。modelUsage は起動中の全呼び出しの合計なので文脈の大きさには使えない
func claudeContextTokens(stdout string) int {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var j struct {
			Type            string  `json:"type"`
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Message         struct {
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &j) != nil || j.Type != "assistant" || j.ParentToolUseID != nil || j.Message.Usage == nil {
			continue
		}
		u := j.Message.Usage
		return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	}
	return 0
}

func (j claudeOutput) modelAndUsage() (string, Usage) {
	model, most := "", -1
	usage := Usage{Known: j.ModelUsage != nil}
	for name, u := range j.ModelUsage {
		if u.OutputTokens > most {
			model, most = name, u.OutputTokens
		}
		// inputTokens はキャッシュ分を含まないので足し合わせる
		usage.Input += u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		usage.CachedInput += u.CacheReadInputTokens
		usage.Output += u.OutputTokens
	}
	return model, usage
}

// ---- Codex CLI ----------------------------------------------------------------

type codexAdapter struct {
	bin string
	pre []string // node で JS エントリを起動する場合のスクリプトパス
}

// npm のシム(codex.cmd)は cmd.exe 経由でないと起動できず引数のクォートが壊れるため、
// Windows では中身の JS エントリを node で直接起動する
func newCodexAdapter() *codexAdapter {
	if bin := os.Getenv("CODEX_BIN"); bin != "" {
		return &codexAdapter{bin: bin}
	}
	shim, err := exec.LookPath("codex")
	if err != nil {
		return &codexAdapter{}
	}
	if runtime.GOOS != "windows" || !strings.EqualFold(filepath.Ext(shim), ".cmd") {
		return &codexAdapter{bin: shim}
	}
	js := filepath.Join(filepath.Dir(shim), "node_modules", "@openai", "codex", "bin", "codex.js")
	node, err := exec.LookPath("node")
	if _, statErr := os.Stat(js); statErr != nil || err != nil {
		return &codexAdapter{}
	}
	return &codexAdapter{bin: node, pre: []string{js}}
}

func (a *codexAdapter) Available() bool { return a.bin != "" }

// Models は Codex が保存しているモデル一覧（~/.codex/models_cache.json）のうち、一覧表示対象のものを返す
func (a *codexAdapter) Models() []ModelOption {
	b, err := os.ReadFile(filepath.Join(codexHome(), "models_cache.json"))
	if err != nil {
		return nil
	}
	var cache struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &cache) != nil {
		return nil
	}
	var list []ModelOption
	for _, m := range cache.Models {
		if m.Visibility == "list" && modelIDRe.MatchString(m.Slug) {
			list = append(list, ModelOption{m.Slug, firstNonEmpty(m.DisplayName, m.Slug)})
		}
	}
	return list
}

func (a *codexAdapter) ModelSource() string { return "キャッシュ" }
func (a *codexAdapter) SelfTool() bool      { return true }

func (a *codexAdapter) Run(ctx context.Context, prompt, sessionID, model, cwd string) (TurnResult, error) {
	args := append([]string{}, a.pre...)
	args = append(args, "exec")
	if sessionID != "" {
		args = append(args, "resume", sessionID)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if selfToolURL != "" {
		// 非対話実行では承認を求められないため、自己管理ツールだけ承認不要にする
		args = append(args, "-c", fmt.Sprintf("mcp_servers.ai_agent_room.url=%q", selfToolURL))
		for _, name := range selfToolNames {
			args = append(args, "-c", "mcp_servers.ai_agent_room.tools."+name+`.approval_mode="approve"`)
		}
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	// *_ARGS に権限の段階（案8）を反映する
	args = append(args, argsWithPermission(a, "CODEX_ARGS", permissionFrom(ctx))...)
	args = append(args, "-") // プロンプトは stdin から
	r, err := runProcess(withPartialParser(withLineParser(ctx, codexProgress), codexPartial), a.bin, args, prompt, cwd)
	ev := parseCodexEvents(r.stdout)
	usage := ev.usage
	// 失敗・キャンセルでも、出力から取得できた使用量は集計に回す
	if err != nil {
		return TurnResult{Usage: usage}, err
	}
	threadID, texts, lastErr := firstNonEmpty(ev.threadID, sessionID), ev.texts, ev.lastErr
	if ev.scanErr != nil { // 読み取り途中の失敗を正常完了として扱わない
		return TurnResult{Usage: usage}, &AgentError{ErrAgentBadOutput, "codex: 出力を読み取れません: " + ev.scanErr.Error()}
	}
	if len(texts) == 0 {
		if lastErr != "" {
			return TurnResult{Usage: usage}, cliFailure("codex: " + lastErr)
		}
		return TurnResult{Usage: usage}, procFailure("codex", r)
	}
	return TurnResult{strings.Join(texts, "\n\n"), threadID, codexModel(threadID), usage}, nil
}

// codexEvents は codex exec --json の出力（1行1イベント）の解析結果
type codexEvents struct {
	threadID string
	texts    []string
	usage    Usage
	lastErr  string
	scanErr  error
}

func parseCodexEvents(stdout string) codexEvents {
	var e codexEvents
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			// turn.completed の使用量（input_tokens はキャッシュ分を含む）
			Usage *struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "thread.started":
			e.threadID = ev.ThreadID
		case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
			e.texts = append(e.texts, ev.Item.Text)
		case ev.Type == "turn.completed" && ev.Usage != nil:
			e.usage.Known = true
			e.usage.Input += ev.Usage.InputTokens
			e.usage.CachedInput += ev.Usage.CachedInputTokens
			e.usage.Output += ev.Usage.OutputTokens
		case ev.Type == "turn.failed" || ev.Type == "error":
			e.lastErr = firstNonEmpty(ev.Error.Message, ev.Message, line)
		}
	}
	e.scanErr = sc.Err()
	return e
}

// codexModel は Codex のセッション記録（~/.codex/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl）の
// 最後の turn_context からモデル名を読む。--json の出力にはモデル名が含まれないため。
func codexModel(threadID string) string {
	if threadID == "" {
		return ""
	}
	files, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if len(files) == 0 {
		return ""
	}
	f, err := os.Open(files[len(files)-1])
	if err != nil {
		return ""
	}
	defer f.Close()
	model := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"turn_context"`)) {
			continue
		}
		var ev struct {
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Payload.Model != "" {
			model = ev.Payload.Model
		}
	}
	return model
}

func codexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// ---- Antigravity CLI (agy) ----------------------------------------------------

// agy は stdin からのプロンプトに対応していないため引数で渡す。
// .exe を shell なしで直接起動するので、改行や引用符も正しく渡る。
type agyAdapter struct {
	bin string

	mu     sync.Mutex
	models []ModelOption // `agy models` の結果（起動時にバックグラウンドで取得）
}

const agyMaxPrompt = 30000 // Windows のコマンドライン長上限(32767)対策

func newAgyAdapter() *agyAdapter {
	bin := os.Getenv("AGY_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("agy")
	}
	a := &agyAdapter{bin: bin}
	if bin != "" {
		go a.loadModels() // 取得に数秒かかるため起動を待たせない
	}
	return a
}

func (a *agyAdapter) Available() bool { return a.bin != "" }

func (a *agyAdapter) Models() []ModelOption {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.models
}

func (a *agyAdapter) ModelSource() string { return "CLI照会" }

// agy は起動時に MCP サーバを指定できない（設定への恒久登録が必要で、人間の了承待ち）
func (a *agyAdapter) SelfTool() bool { return false }

// loadModels は `agy models` の「<ID>\t<表示名>」形式の行を読む
func (a *agyAdapter) loadModels() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := runProcess(ctx, a.bin, []string{"models"}, "", "")
	if err != nil || r.exitCode != 0 {
		return
	}
	var list []ModelOption
	for _, line := range strings.Split(r.stdout, "\n") {
		id, label, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && modelIDRe.MatchString(id) {
			list = append(list, ModelOption{id, strings.TrimSpace(label)})
		}
	}
	a.mu.Lock()
	a.models = list
	a.mu.Unlock()
}

func (a *agyAdapter) Run(ctx context.Context, prompt, sessionID, model, cwd string) (TurnResult, error) {
	if r := []rune(prompt); len(r) > agyMaxPrompt {
		prompt = "…(前半省略)…\n" + string(r[len(r)-agyMaxPrompt:])
	}
	// agy の JSON 出力にはモデル名が含まれないため、ターンごとのログファイルから読む
	logPath := ""
	if f, err := os.CreateTemp("", "ai-agent-room-agy-*.log"); err == nil {
		logPath = f.Name()
		f.Close()
		defer os.Remove(logPath)
	}
	// stream-json は途中経過（step_update）を1行ずつ出し、最後の event=result 行に結果が入る
	args := []string{"-p", prompt, "--output-format", "stream-json"}
	if sessionID != "" {
		args = append(args, "--conversation", sessionID)
	}
	if logPath != "" {
		args = append(args, "--log-file", logPath)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, extraArgs("AGY_ARGS")...)
	r, err := runProcess(withLineParser(ctx, agyProgress), a.bin, args, "", cwd)
	if err != nil {
		return TurnResult{}, err
	}
	j, ok := parseAgyResult(r.stdout)
	if !ok {
		return TurnResult{}, procFailure("agy", r)
	}
	if j.Status != "" && j.Status != "SUCCESS" {
		return TurnResult{}, cliFailure("agy: " + j.Status + " " + j.Error)
	}
	return TurnResult{Text: strings.TrimRight(j.Response, "\n "), SessionID: j.ConversationID, Model: agyModel(logPath), Usage: j.turnUsage}, nil
}

type agyResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	turnUsage      Usage  // このターンの使用量（step_update から集計する）
}

// agyUsage は agy の API 呼び出し1回分の使用量。input_tokens はキャッシュ分（cache_read_tokens）を含まない
type agyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// parseAgyResult は stream-json 出力の最後の event=result 行を読む。
// result 行の usage は会話の始めからの累計なので使わない。使用量は、このターンの
// step_update（agent_response、DONE）の usage を API 呼び出しごとに足し、文脈の大きさは最後の呼び出しの入力とする
func parseAgyResult(stdout string) (agyResult, bool) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var ev struct {
			Event  string    `json:"event"`
			Result agyResult `json:"result"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &ev) != nil || ev.Event != "result" {
			continue
		}
		res := ev.Result
		res.turnUsage = agyTurnUsage(lines[:i])
		return res, true
	}
	return agyResult{}, false
}

func agyTurnUsage(lines []string) Usage {
	var u Usage
	seen := map[int]bool{} // 同じ呼び出し（step_index）を二重に数えない
	for _, line := range lines {
		if !strings.Contains(line, `"usage"`) {
			continue
		}
		var st struct {
			Event      string `json:"event"`
			StepUpdate struct {
				StepIndex int       `json:"step_index"`
				State     string    `json:"state"`
				StepType  string    `json:"step_type"`
				Usage     *agyUsage `json:"usage"`
			} `json:"step_update"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &st) != nil || st.Event != "step_update" {
			continue
		}
		s := st.StepUpdate
		if s.Usage == nil || s.StepType != "agent_response" || s.State != "DONE" || seen[s.StepIndex] {
			continue
		}
		seen[s.StepIndex] = true
		in := s.Usage.InputTokens + s.Usage.CacheReadTokens
		u.Known = true
		u.Input += in
		u.CachedInput += s.Usage.CacheReadTokens
		u.Output += s.Usage.OutputTokens
		u.Context = in
	}
	return u
}

// agyModelRe は agy のログの「Propagating selected model override to backend: label="Gemini 3.8 Flash (High)"」に一致する
var agyModelRe = regexp.MustCompile(`selected model[^\n]*label="([^"]+)"`)

func agyModel(logPath string) string {
	if logPath == "" {
		return ""
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	m := agyModelRe.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return ""
	}
	return string(m[len(m)-1][1])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// claudeAllowedTools は AI Agent Room の MCP ツールを承認不要にする --allowedTools の値（カンマ区切り）
func claudeAllowedTools() string {
	names := make([]string, len(selfToolNames))
	for i, n := range selfToolNames {
		names[i] = "mcp__ai_agent_room__" + n
	}
	return strings.Join(names, ",")
}
