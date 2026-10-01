package main

// AI Agent Room: Claude Code / Codex CLI / Antigravity CLI と人間が同じチャットで会話するローカルWebアプリ。
//
// API（レスポンスは成功時 {"data": ...}、失敗時 {"error_code", "message", "request_id"}）
//   GET  /api/events    SSE で履歴スナップショットと以降のイベントを配信
//   POST /api/messages     {"text": "..."} 人間の発言
//   POST /api/discussions  {"topic": "...", "rounds": 3} ディスカッション開始
//   POST /api/freetalk     {"topic": "..."} フリートーク開始（topic は省略可）
//   POST /api/stop         実行中のターン・待機・ディスカッションを中止
//   POST /api/reset        履歴と各エージェントのセッションを破棄
//   PUT  /api/settings     {"max_hops": 10, "delay_sec": 3, "leader": "claude", "rotate_tokens": 500000, "lang": "en"}（leader は空文字で指定なし。rotate_tokens は 0 で切り替えなし。lang は ja / en）
//   GET  /api/models       エージェントごとの選べるモデル
//   PUT  /api/agents/{id}  {"model": "opus"} エージェントのモデルを変更（空文字で既定に戻す）
//   GET  /api/logs         過去のチャットログ一覧
//   GET  /api/logs/{name}  チャットログの内容
//   GET  /api/logs/{name}/export  チャットログを Markdown で書き出す
//   POST /api/logs/{name}/branch  {"upto": 12} ログの発言 12 までを引き継いで新しい会話を始める
//   POST /api/diag         各 CLI を --version で起動して、見つからない・起動失敗・時間切れ・バージョンを返す（診断）
//   GET  /api/search       ?q=語&from=発言者&limit=件数 過去ログを横断して発言を探す

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	port := flag.Int("port", 8787, "待ち受けポート（127.0.0.1 のみ）")
	workdir := flag.String("workdir", "", "エージェントの作業ディレクトリ（既定: 前回の作業ディレクトリ、なければカレントディレクトリ）")
	maxHops := flag.Int("max-hops", 100, "人間の発言1回あたりのエージェントの最大ターン数（0 で無制限）")
	delaySec := flag.Int("delay", 3, "エージェントの発言後、次のエージェントを起動するまでの待ち秒数")
	noOpen := flag.Bool("no-open", false, "起動時にブラウザを開かない")
	flag.Parse()

	exe, _ := os.Executable()
	logDir := filepath.Join(filepath.Dir(exe), "logs")
	if os.Getenv("AI_AGENT_ROOM_LOG_DIR") != "" {
		logDir = os.Getenv("AI_AGENT_ROOM_LOG_DIR")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "ログディレクトリを作成できません:", err)
		os.Exit(1)
	}
	lf, err := os.OpenFile(filepath.Join(logDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ログファイルを開けません:", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(lf, nil))
	// 改名前（AI Chat）の保存先にある認証トークンと設定を引き継ぐ。保存先を使う処理より前に行う
	if from, to, err := migrateLegacySecretDir(); err != nil {
		log.Warn("config.migrate", "error", err.Error())
		fmt.Fprintln(os.Stderr, "以前の設定フォルダを移せませんでした（新しい設定で起動します）:", err)
	} else if from != "" {
		log.Info("config.migrate", "from", from, "to", to)
	}

	wd := *workdir
	if wd == "" {
		wd = savedWorkdir(logDir) // 画面で変更した作業ディレクトリを再起動後も使う
	}
	if wd == "" {
		wd, _ = os.Getwd()
	}
	wd, _ = filepath.Abs(wd)

	agents, err := loadAgents(logDir) // 既定の3つと、画面から追加したエージェント
	if err != nil {
		log.Warn("agents.load", "error", err.Error())
	}
	room := NewRoom(agents, wd, logDir, *maxHops, time.Duration(*delaySec)*time.Second, log)
	// 画面で変えた設定を読み込む。コマンドラインで指定した項目はそちらを優先する
	cliSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { cliSet[strings.ReplaceAll(f.Name, "-", "_")] = true })
	cliSet["delay_sec"] = cliSet["delay"]
	if sd, err := defaultSecretDir(); err == nil {
		initProtection(sd, exeDir(), wd) // 禁止ルールとコードブロックの警告に使う保護するパス
		room.SetConfigDir(filepath.Join(sd, configDirName))
	}
	room.LoadSettings(cliSet)
	// 人間が編集する設定（rules.md など）は作業ディレクトリの外に置き、エージェントには起動時に禁止ルールで読み書きさせない
	room.SetEnvProfile(detectEnvironment(), defaultConfigDir(), exeDir()) // 環境とルールを会話に載せる（修正案 7.3）

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/events", room.handleEvents)
	mux.HandleFunc("POST /api/messages", room.handlePostMessage)
	mux.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) { room.Stop(); writeData(w, http.StatusOK, nil) })
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) { room.Reset(); writeData(w, http.StatusOK, nil) })
	mux.HandleFunc("PUT /api/settings", room.handleSettings)
	mux.HandleFunc("POST /api/summarize", room.handleSummarize)
	mux.HandleFunc("POST /api/discussions", room.handleStartDiscussion)
	mux.HandleFunc("POST /api/freetalk", room.handleStartFreeTalk)
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) { writeData(w, http.StatusOK, room.Models()) })
	mux.HandleFunc("PUT /api/agents/{id}", room.handleUpdateAgent)
	mux.HandleFunc("POST /api/agents", room.handleAddAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", room.handleRemoveAgent)
	mux.HandleFunc("POST /api/agents/{id}/retry", room.handleRetryAgent)
	mux.HandleFunc("POST /api/agents/{id}/interactive", room.handleStartInteractive) // 対話モードの窓を開く（案9）
	mux.HandleFunc("DELETE /api/agents/{id}/interactive", room.handleEndInteractive) // ［対話を終了］（macOS・Linux）
	mux.HandleFunc("GET /api/agents/{id}/live", room.handleLive)
	mux.HandleFunc("GET /api/logs", room.handleListLogs)
	mux.HandleFunc("GET /api/logs/{name}", room.handleReadLog)
	mux.HandleFunc("GET /api/logs/{name}/export", room.handleExportLog)
	mux.HandleFunc("POST /api/diag", room.handleDiag)
	mux.HandleFunc("POST /api/logs/{name}/branch", room.handleBranchLog)
	mux.HandleFunc("GET /api/search", room.handleSearch)
	mux.HandleFunc("POST /api/messages/{id}/blocks/{n}/run", room.handleRunBlock)
	mux.HandleFunc("POST /api/messages/{id}/blocks/{n}/cancel", room.handleCancelBlock)
	mux.HandleFunc("POST /api/config/open", room.handleOpenConfig)
	mux.HandleFunc("POST /mcp", room.handleMCP)
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) }) // SSE ストリームは提供しない

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "待ち受けできません:", err)
		os.Exit(1)
	}
	secretDir, err := defaultSecretDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "認証トークンの保存先を決められません:", err)
		os.Exit(1)
	}
	token, err := loadOrCreateAuthToken(logDir, secretDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "認証トークンを用意できません:", err)
		os.Exit(1)
	}
	url := "http://" + addr
	selfToolURL = url + "/mcp"
	openURL := url + "/?token=" + token // 画面の API を使うには、この URL で開く
	fmt.Println("AI Agent Room:", openURL)
	fmt.Println("作業ディレクトリ:", wd)
	fmt.Println("ログ:", logDir)
	for _, a := range agents {
		mark := "✓"
		if !a.Adapter.Available() {
			mark = "✗ (見つかりません)"
		}
		fmt.Printf("  %s %s (@%s)\n", mark, a.Name, a.ID)
	}
	log.Info("server.start", "addr", addr, "workdir", wd)
	room.refreshQuotas(true) // プランの残り利用枠を起動時に取得する（定期的な取得はしない）
	if !*noOpen {
		openBrowser(openURL)
	}
	if err := http.Serve(ln, withRequestLog(log, localOnly(*port, requireAuth(*port, token, mux)))); err != nil {
		log.Error("server.stop", "error", err.Error())
	}
}

// ---- ハンドラ --------------------------------------------------------------------

func (room *Room) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "STREAM_UNSUPPORTED", "ストリーミングに対応していません")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, unsub := room.Subscribe()
	defer unsub()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok { // 詰まって切断された。画面が再接続して snapshot を受け取り直す
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (room *Room) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		ReplyTo int    `json:"reply_to"` // 返信先の発言ID（省略時は返信でない）
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	m, err := room.PostHumanReply(body.Text, body.ReplyTo)
	if errors.Is(err, ErrEmptyMessage) {
		writeError(w, r, http.StatusBadRequest, "EMPTY_MESSAGE", err.Error())
		return
	}
	if errors.Is(err, ErrInvalidReply) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REPLY", err.Error())
		return
	}
	writeData(w, http.StatusCreated, m)
}

func (room *Room) handleStartDiscussion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Topic  string `json:"topic"`
		Rounds int    `json:"rounds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	m, err := room.StartDiscussion(body.Topic, body.Rounds)
	switch {
	case errors.Is(err, ErrEmptyTopic):
		writeError(w, r, http.StatusBadRequest, "EMPTY_TOPIC", err.Error())
	case errors.Is(err, ErrNoAgents):
		writeError(w, r, http.StatusConflict, "NO_AGENTS", err.Error())
	case errors.Is(err, ErrDiscussionBusy):
		writeError(w, r, http.StatusConflict, "DISCUSSION_BUSY", err.Error())
	default:
		writeData(w, http.StatusCreated, m)
	}
}

func (room *Room) handleStartFreeTalk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Topic string `json:"topic"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	err := room.StartFreeTalk(strings.TrimSpace(body.Topic))
	switch {
	case errors.Is(err, ErrNoAgents):
		writeError(w, r, http.StatusConflict, "NO_AGENTS", err.Error())
	case errors.Is(err, ErrDiscussionBusy):
		writeError(w, r, http.StatusConflict, "DISCUSSION_BUSY", err.Error())
	default:
		writeData(w, http.StatusCreated, nil)
	}
}

// handleSummarize は会話の要約を作ってから新しい会話を始める。要約は裏で作るので受け付けたら 202 を返す
func (room *Room) handleSummarize(w http.ResponseWriter, r *http.Request) {
	err := room.SummarizeAndReset()
	switch {
	case errors.Is(err, ErrNoAgents):
		writeError(w, r, http.StatusConflict, "NO_AGENTS", err.Error())
	case errors.Is(err, ErrSummarizing):
		writeError(w, r, http.StatusConflict, "SUMMARIZING", err.Error())
	case errors.Is(err, ErrNothingToSumm):
		writeError(w, r, http.StatusBadRequest, "NOTHING_TO_SUMMARIZE", err.Error())
	default:
		writeData(w, http.StatusAccepted, nil)
	}
}

// handleAddAgent は指定した種類のエージェントを追加する（例: {"type": "claude"} で claude2）
func (room *Room) handleAddAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	a, err := room.AddAgent(body.Type)
	switch {
	case errors.Is(err, ErrInvalidAgentType):
		writeError(w, r, http.StatusBadRequest, "INVALID_AGENT_TYPE", err.Error())
	case errors.Is(err, ErrAgentsBusy):
		writeError(w, r, http.StatusConflict, "AGENTS_BUSY", err.Error())
	case errors.Is(err, ErrAgentLimit):
		writeError(w, r, http.StatusConflict, "AGENT_LIMIT", err.Error())
	default:
		writeData(w, http.StatusCreated, map[string]string{"id": a.ID})
	}
}

// handleRemoveAgent は追加したエージェントを削除する
func (room *Room) handleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	err := room.RemoveAgent(r.PathValue("id"))
	switch {
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	case errors.Is(err, ErrAgentDefault):
		writeError(w, r, http.StatusBadRequest, "AGENT_DEFAULT", err.Error())
	case errors.Is(err, ErrAgentsBusy):
		writeError(w, r, http.StatusConflict, "AGENTS_BUSY", err.Error())
	default:
		writeData(w, http.StatusOK, nil)
	}
}

func (room *Room) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  *string `json:"model"`
		Paused *bool   `json:"paused"` // 一時停止（true）・再開（false）
		// TimeoutSec はこのエージェントだけの1ターンの上限時間（秒）。0 で会話全体の設定に戻す（案 8.3）
		TimeoutSec *int `json:"timeout_sec"`
		// Permission は権限の段階（""＝既定、read_only、workspace_write）。人間の画面からだけ変える（案8）
		Permission *string `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	var err error
	if body.Model != nil {
		err = room.SetAgentModel(r.PathValue("id"), *body.Model)
	}
	if err == nil && body.Paused != nil {
		err = room.SetPaused(r.PathValue("id"), *body.Paused)
	}
	if err == nil && body.TimeoutSec != nil {
		if err = room.SetAgentTimeout(r.PathValue("id"), *body.TimeoutSec); err == nil {
			room.SaveSettings()
		}
	}
	if err == nil && body.Permission != nil {
		err = room.SetAgentPermission(r.PathValue("id"), *body.Permission)
	}
	switch {
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	case errors.Is(err, ErrInvalidModel):
		writeError(w, r, http.StatusBadRequest, "INVALID_MODEL", err.Error())
	case errors.Is(err, ErrInvalidPermission):
		writeError(w, r, http.StatusBadRequest, "INVALID_PERMISSION", err.Error())
	default:
		writeData(w, http.StatusOK, nil)
	}
}

// handleRetryAgent は失敗したターンをもう一度実行させる（案 8.3）
func (room *Room) handleRetryAgent(w http.ResponseWriter, r *http.Request) {
	err := room.RetryAgent(r.PathValue("id"))
	switch {
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	case errors.Is(err, ErrAgentPaused):
		writeError(w, r, http.StatusConflict, "AGENT_PAUSED", err.Error())
	case errors.Is(err, ErrAgentThinking):
		writeError(w, r, http.StatusConflict, "AGENT_THINKING", err.Error())
	default:
		writeData(w, http.StatusOK, nil)
	}
}

func (room *Room) handleEndInteractive(w http.ResponseWriter, r *http.Request) {
	err := room.EndInteractive(r.PathValue("id"))
	switch {
	case err == nil:
		writeData(w, http.StatusOK, nil)
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	case errors.Is(err, ErrNotInteractive):
		writeError(w, r, http.StatusConflict, "NOT_INTERACTIVE", err.Error())
	default: // 窓が閉じたことを自動で検知できる環境
		writeError(w, r, http.StatusConflict, "INTERACTIVE_AUTO", err.Error())
	}
}

func (room *Room) handleStartInteractive(w http.ResponseWriter, r *http.Request) {
	err := room.StartInteractive(r.PathValue("id"))
	switch {
	case err == nil:
		writeData(w, http.StatusOK, nil)
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	case errors.Is(err, ErrInteractiveUnsupported):
		writeError(w, r, http.StatusConflict, "INTERACTIVE_UNSUPPORTED", err.Error())
	case errors.Is(err, ErrInteractiveRunning):
		writeError(w, r, http.StatusConflict, "INTERACTIVE_RUNNING", err.Error())
	case errors.Is(err, ErrAgentThinking):
		writeError(w, r, http.StatusConflict, "AGENT_THINKING", err.Error())
	default: // CLI を起動できなかった
		writeError(w, r, http.StatusInternalServerError, "INTERACTIVE_START_FAILED", err.Error())
	}
}

func (room *Room) handleListLogs(w http.ResponseWriter, r *http.Request) {
	list, err := room.ListLogs()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "LOG_READ_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, list)
}

func (room *Room) handleReadLog(w http.ResponseWriter, r *http.Request) {
	msgs, err := room.ReadLog(r.PathValue("name"))
	switch {
	case errors.Is(err, ErrLogNotFound):
		writeError(w, r, http.StatusNotFound, "LOG_NOT_FOUND", err.Error())
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "LOG_READ_FAILED", err.Error())
	default:
		writeData(w, http.StatusOK, msgs)
	}
}

// handleExportLog はチャットログを Markdown で返す（案 4）。ブラウザでそのまま保存できるようにファイル名を付ける。
func (room *Room) handleExportLog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	md, err := room.ExportMarkdown(name)
	switch {
	case errors.Is(err, ErrLogNotFound):
		writeError(w, r, http.StatusNotFound, "LOG_NOT_FOUND", err.Error())
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "LOG_READ_FAILED", err.Error())
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename="+strings.TrimSuffix(name, ".jsonl")+".md")
		w.Write([]byte(md))
	}
}

// handleBranchLog は過去ログの指定した発言までを引き継いで新しい会話を始める（案3）
func (room *Room) handleBranchLog(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Upto int `json:"upto"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	err := room.BranchFromLog(r.PathValue("name"), body.Upto)
	switch {
	case errors.Is(err, ErrLogNotFound):
		writeError(w, r, http.StatusNotFound, "LOG_NOT_FOUND", err.Error())
	case errors.Is(err, ErrBranchPointNotFound):
		writeError(w, r, http.StatusBadRequest, "MESSAGE_NOT_FOUND", err.Error())
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "LOG_READ_FAILED", err.Error())
	default:
		writeData(w, http.StatusOK, nil)
	}
}

// handleSearch は過去ログを横断して発言を探す（案 4）
func (room *Room) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	res, err := room.SearchMessages(q.Get("q"), q.Get("from"), limit)
	switch {
	case errors.Is(err, ErrEmptyQuery):
		writeError(w, r, http.StatusBadRequest, "EMPTY_QUERY", err.Error())
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "LOG_READ_FAILED", err.Error())
	default:
		writeData(w, http.StatusOK, res)
	}
}

func (room *Room) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxHops      *int    `json:"max_hops"`
		DelaySec     *int    `json:"delay_sec"`
		Leader       *string `json:"leader"`
		Workdir      *string `json:"workdir"`
		RotateTokens *int    `json:"rotate_tokens"`
		// CommandLeaderOnly が true なら、進行役（と人間）の発言のコードブロックだけを実行できる（修正案 7.1）
		CommandLeaderOnly *bool `json:"command_leader_only"`
		// TurnTimeoutSec は1ターンの上限時間（秒。案 8.3）
		TurnTimeoutSec *int `json:"turn_timeout_sec"`
		// Lang は言語の設定（"ja" / "en"）。環境とルールの投稿は次の投稿（起動時・新しい会話の開始時）から変わる
		Lang *string `json:"lang"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	// 検証に失敗しうる作業ディレクトリと進行役を先に確かめ、失敗時はほかの設定を変えない
	var workdir string
	if body.Workdir != nil {
		var err error
		if workdir, err = normalizeWorkdir(*body.Workdir); err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_WORKDIR", err.Error())
			return
		}
	}
	if body.Lang != nil && *body.Lang != langJa && *body.Lang != langEn {
		writeError(w, r, http.StatusBadRequest, "INVALID_LANG", ErrInvalidLang.Error())
		return
	}
	if body.Leader != nil {
		err := room.SetLeader(*body.Leader)
		switch {
		case errors.Is(err, ErrAgentNotFound):
			writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
			return
		case errors.Is(err, ErrAgentNoLeader):
			writeError(w, r, http.StatusBadRequest, "AGENT_UNAVAILABLE", err.Error())
			return
		}
	}
	if body.Workdir != nil {
		if err := room.SetWorkdir(workdir); err != nil { // 確かめた後に消された場合
			writeError(w, r, http.StatusBadRequest, "INVALID_WORKDIR", err.Error())
			return
		}
	}
	if body.MaxHops != nil {
		room.SetMaxHops(*body.MaxHops)
	}
	if body.DelaySec != nil {
		room.SetDelay(*body.DelaySec)
	}
	if body.RotateTokens != nil {
		room.SetRotateTokens(*body.RotateTokens)
	}
	if body.CommandLeaderOnly != nil {
		room.SetCommandLeaderOnly(*body.CommandLeaderOnly)
	}
	if body.TurnTimeoutSec != nil {
		room.SetTurnTimeout(*body.TurnTimeoutSec)
	}
	if body.Lang != nil {
		room.SetLang(*body.Lang) // 値は上で確かめた
	}
	room.SaveSettings()
	writeData(w, http.StatusOK, nil)
}

// handleOpenConfig は人間が編集する設定のフォルダ（rules.md・capabilities.json）をエクスプローラーで開く。
// 設定は作業ディレクトリの外にあり、エージェントからは読み書きできない
func (room *Room) handleOpenConfig(w http.ResponseWriter, r *http.Request) {
	dir := room.ConfigDir()
	if dir == "" {
		writeError(w, r, http.StatusInternalServerError, "CONFIG_DIR_UNAVAILABLE", "設定フォルダの場所を決められません")
		return
	}
	if err := openConfigDir(dir); err != nil {
		writeError(w, r, http.StatusInternalServerError, "CONFIG_DIR_OPEN_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]string{"dir": dir})
}

// blockParams は /api/messages/{id}/blocks/{n}/... の発言IDとブロック番号（0 始まり）を読む
func blockParams(r *http.Request) (int, int, bool) {
	id, err1 := strconv.Atoi(r.PathValue("id"))
	n, err2 := strconv.Atoi(r.PathValue("n"))
	return id, n, err1 == nil && err2 == nil
}

// handleRunBlock は発言中のコードブロックを実行し始める（修正案 7.1）。本文は受け取らず、保存済みの発言から取り直す
func (room *Room) handleRunBlock(w http.ResponseWriter, r *http.Request) {
	id, n, ok := blockParams(r)
	if !ok {
		writeError(w, r, http.StatusNotFound, "BLOCK_NOT_FOUND", ErrCommandNotFound.Error())
		return
	}
	var body CommandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "JSONを解析できません")
		return
	}
	run, err := room.RunBlock(id, n, body)
	switch {
	case err == nil:
		writeData(w, http.StatusAccepted, run)
	case errors.Is(err, ErrCommandNotFound):
		writeError(w, r, http.StatusNotFound, "BLOCK_NOT_FOUND", err.Error())
	case errors.Is(err, ErrCommandNotRunable):
		writeError(w, r, http.StatusBadRequest, "BLOCK_NOT_RUNNABLE", err.Error())
	case errors.Is(err, ErrCommandUnsupportedOS):
		writeError(w, r, http.StatusBadRequest, "BLOCK_UNSUPPORTED_OS", err.Error())
	case errors.Is(err, ErrCommandExpired):
		writeError(w, r, http.StatusGone, "BLOCK_EXPIRED", err.Error())
	case errors.Is(err, ErrCommandUsed):
		writeError(w, r, http.StatusConflict, "BLOCK_ALREADY_RUN", err.Error())
	case errors.Is(err, ErrCommandBusy):
		writeError(w, r, http.StatusConflict, "COMMAND_BUSY", err.Error())
	case errors.Is(err, ErrCommandForbidden):
		writeError(w, r, http.StatusForbidden, "LEADER_ONLY", err.Error())
	case errors.Is(err, ErrCommandConfirm):
		writeError(w, r, http.StatusPreconditionRequired, "CONFIRM_REQUIRED", err.Error())
	default: // 作業フォルダが見つからない
		writeError(w, r, http.StatusBadRequest, "INVALID_WORKDIR", err.Error())
	}
}

func (room *Room) handleCancelBlock(w http.ResponseWriter, r *http.Request) {
	id, n, ok := blockParams(r)
	if !ok {
		writeError(w, r, http.StatusNotFound, "BLOCK_NOT_FOUND", ErrCommandNotFound.Error())
		return
	}
	if err := room.CancelBlock(id, n); err != nil {
		writeError(w, r, http.StatusConflict, "COMMAND_NOT_RUNNING", err.Error())
		return
	}
	writeData(w, http.StatusOK, nil)
}

// ---- 共通: レスポンス形式・request_id・アクセスログ ------------------------------------------

type ctxKey struct{}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error_code": code, "message": msg, "request_id": w.Header().Get("X-Request-Id")})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", id)
		start := time.Now()
		l := log.With("request_id", id, "method", r.Method, "path", r.URL.Path)
		l.Info("request.start")
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		l.Info("request.end", "status", rec.status, "latency_ms", time.Since(start).Milliseconds())
	})
}

// localOnly は他サイトからのリクエスト（CSRF・DNSリバインディング）を拒否する。
// エージェントはコマンドを実行できるため、このUI以外からの操作は受け付けない。
func localOnly(port int, next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN_HOST", "許可されていないホストです")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !allowed[strings.TrimPrefix(o, "http://")] {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN_ORIGIN", "許可されていないオリジンです")
			return
		}
		if r.Method != http.MethodGet && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type は application/json にしてください")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
