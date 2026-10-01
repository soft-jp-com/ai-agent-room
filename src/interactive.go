package main

// 対話モードでの起動（案9）。ログインや認証など、非対話の実行では進められない操作のために、
// エージェントの CLI を新しいコンソール窓で対話モードのまま起動する。画面の中に端末（PTY）は組み込まない。
// セッションがあれば同じセッションを再開し、なければ AI Agent Room の会話と結び付かない新しいセッションで起動する。
// 窓を開いている間はそのエージェントを会話に参加させず（a.interactive。保存しない）、窓が閉じたら戻す。
// macOS・Linux は端末アプリが CLI の終了を待たずに戻るため、CLI の終了を目印のファイルで知り（interactive_unix.go）、
// 窓を強制的に閉じた場合に備えて画面に［対話を終了］を出す。
// AI Agent Room が終了しても窓はそのまま残る（AI Agent Room を再起動すると、開いたままの窓は追跡しない）。

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

var (
	ErrInteractiveUnsupported = errors.New("この環境では対話モードの窓を開けません（Windows のデスクトップで動いているときだけ使えます）")
	ErrInteractiveRunning     = errors.New("このエージェントはすでに対話モードの窓を開いています")
)

// InteractiveStarter は、対話モードで起動するコマンドを返せる CLI のアダプタが実装する。
// 非対話用の引数（-p、--output-format、exec、--json など）は含めず、権限・禁止ルール・モデル・*_ARGS だけを引き継ぐ
type InteractiveStarter interface {
	InteractiveCommand(sessionID, model, permission string) (bin string, args []string)
}

func (a *claudeAdapter) InteractiveCommand(sessionID, model, permission string) (string, []string) {
	var args []string
	if s := claudeDenySettings(); s != "" {
		args = append(args, "--settings="+s)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return a.bin, append(args, argsWithPermission(a, "CLAUDE_ARGS", permission)...)
}

func (a *codexAdapter) InteractiveCommand(sessionID, model, permission string) (string, []string) {
	args := append([]string{}, a.pre...)
	if sessionID != "" {
		args = append(args, "resume", sessionID)
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	return a.bin, append(args, argsWithPermission(a, "CODEX_ARGS", permission)...)
}

// agy は -p（--print）を付けなければ対話モードで起動する
func (a *agyAdapter) InteractiveCommand(sessionID, model, permission string) (string, []string) {
	var args []string
	if sessionID != "" {
		args = append(args, "--conversation", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return a.bin, append(args, argsWithPermission(a, "AGY_ARGS", permission)...)
}

// consoleSession は開いた対話モードの窓。Windows はプロセスの終了で、macOS・Linux は目印のファイルで閉じたことを知る
type consoleSession interface {
	Pid() int                        // ログ用（分からなければ 0）
	Wait() (exitCode int, err error) // CLI が終わるか Stop されるまで待つ
	Stop()                           // 人間の［対話を終了］で待つのをやめる（窓は閉じない）
	AutoDetect() bool                // 窓が閉じたことを自動で検知できるか（できなければ画面に［対話を終了］を出す）
}

var (
	errConsoleStopped   = errors.New("人間が対話を終了にしました")
	ErrNotInteractive   = errors.New("このエージェントは対話モードの窓を開いていません")
	ErrInteractiveNoEnd = errors.New("この環境では、窓を閉じると自動で戻ります")
)

// processSession はプロセスの終了を待つ窓（Windows。テストでも使う）
type processSession struct {
	proc     *os.Process
	stop     chan struct{}
	stopOnce sync.Once
}

func newProcessSession(p *os.Process) *processSession {
	return &processSession{proc: p, stop: make(chan struct{})}
}

func (s *processSession) Pid() int         { return s.proc.Pid }
func (s *processSession) AutoDetect() bool { return true }
func (s *processSession) Stop()            { s.stopOnce.Do(func() { close(s.stop) }) }
func (s *processSession) Wait() (int, error) {
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		ps, err := s.proc.Wait()
		code := -1
		if ps != nil {
			code = ps.ExitCode()
		}
		done <- result{code, err}
	}()
	select {
	case r := <-done:
		return r.code, r.err
	case <-s.stop:
		return -1, errConsoleStopped
	}
}

// startConsole は対話モードの窓を開く。テストでは差し替える
var startConsole = startNewConsole

// StartInteractive はエージェント id の CLI を対話モードの窓で起動する。窓が閉じるのは待たない
func (r *Room) StartInteractive(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil || a.state == "unavailable" {
		return ErrAgentNotFound
	}
	st, ok := a.Adapter.(InteractiveStarter)
	if !ok || !interactiveOK {
		return ErrInteractiveUnsupported
	}
	if a.interactive {
		return ErrInteractiveRunning
	}
	if a.state == "thinking" { // 同じセッションを二重に使わない
		return ErrAgentThinking
	}
	bin, args := st.InteractiveCommand(a.sessionID, a.modelSel, a.permission)
	cs, err := startConsole(bin, args, r.workdir)
	if err != nil {
		r.log.Error("agent.interactive.start", "agent", a.ID, "error", err.Error())
		return err
	}
	a.interactive, a.console = true, cs
	linked := a.sessionID != ""
	r.log.Info("agent.interactive.start", "agent", a.ID, "pid", cs.Pid(), "session_id", a.sessionID, "linked", linked, "cwd", r.workdir, "auto_detect", cs.AutoDetect())
	how := "今のセッションを引き継いでいます"
	if !linked {
		how = "新しいセッションで、この会話とは結び付けていません"
	}
	until := "窓を閉じるまで"
	if !cs.AutoDetect() {
		until = "CLI を終了するか、画面の［対話を終了］を押すまで"
	}
	r.postLocked("system", fmt.Sprintf("%s を対話モードで新しい窓に開きました（%s）。%s、%s は会話に参加しません。", a.Name, how, until, a.Name), "system")
	r.pushStatusLocked()
	go r.waitInteractive(a, cs)
	return nil
}

// EndInteractive は人間の［対話を終了］で、窓が閉じたことを検知できない環境（macOS・Linux）のエージェントを会話に戻す
func (r *Room) EndInteractive(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil {
		return ErrAgentNotFound
	}
	if !a.interactive || a.console == nil {
		return ErrNotInteractive
	}
	if a.console.AutoDetect() {
		return ErrInteractiveNoEnd
	}
	r.log.Info("agent.interactive.stop", "agent", a.ID, "by", "human")
	a.console.Stop()
	return nil
}

// waitInteractive は対話モードの窓が閉じるのを待ち、エージェントを会話に戻す
func (r *Room) waitInteractive(a *Agent, cs consoleSession) {
	code, err := cs.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	a.interactive, a.console = false, nil
	log := r.log.With("agent", a.ID, "pid", cs.Pid(), "exit_code", code)
	if err != nil {
		log = log.With("error", err.Error())
	}
	log.Info("agent.interactive.end")
	if r.agent(a.ID) != a { // 窓を開いている間に削除された
		return
	}
	msg := fmt.Sprintf("%s の対話モードの窓が閉じました。会話への参加を再開します。", a.Name)
	if errors.Is(err, errConsoleStopped) {
		msg = fmt.Sprintf("%s の対話モードを終了にしました。会話への参加を再開します（窓がまだ開いていれば閉じてください。同じセッションを二重に使わないため）。", a.Name)
	}
	r.postLocked("system", msg, "system")
	// フリートーク中なら、戻ったエージェントも聞き始める（SetPaused の再開と同じ）
	if r.free != nil && a.state == "idle" && a.active() {
		a.state = "listening"
		go r.freeLoop(r.free, a)
	}
	r.pushStatusLocked()
}

// interactiveOK は窓を表示できる環境か。起動時に1回だけ調べる（status のたびに調べない。テストでは差し替える）
var interactiveOK = interactiveSupported()
