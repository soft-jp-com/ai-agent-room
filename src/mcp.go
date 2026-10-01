package main

// エージェントの自己管理用の MCP サーバ（streamable HTTP の JSON 応答のみ対応）。
// 接続先は全 CLI 共通の固定エンドポイント /mcp。呼び出し元の識別は、起動ごとに依頼文で渡す
// ジョブ用トークン（ツールの必須引数）で行う。トークンはログ・画面に出さない。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// selfToolURL は CLI に渡す MCP の接続先（main で設定。空なら自己管理ツールを使わない）
var selfToolURL string

const selfToolName = "set_my_model"

// redactedToken はログ・発言・画面でジョブ用トークンの代わりに表示する文字列
const redactedToken = "[ジョブ用トークン]"

// jobTicket は実行中のジョブ1件。ジョブの終了時に削除する（削除後のトークンは無効）
type jobTicket struct {
	agentID  string
	gen      int // 起動時の無効化用世代（Room.gen）
	modelVer int // 起動時のモデル設定の版番号
}

func newJobToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *Room) handleMCP(w http.ResponseWriter, req *http.Request) {
	var m rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&m); err != nil {
		writeRPC(w, nil, nil, &rpcError{-32700, "parse error"})
		return
	}
	if m.ID == nil { // 通知（notifications/initialized 等）には応答しない
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		writeRPC(w, m.ID, map[string]any{
			"protocolVersion": firstNonEmpty(p.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ai_agent_room", "version": "1"},
		}, nil)
	case "ping":
		writeRPC(w, m.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, m.ID, map[string]any{"tools": append([]any{map[string]any{
			"name": selfToolName,
			"description": "AI Agent Room グループチャットで、あなた自身が次の発言から使うモデルを切り替えます。" +
				"作業内容と利用状況を見て、必要なときだけ呼んでください。token には依頼文で渡されたジョブ用トークンを指定します。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"token": map[string]any{"type": "string", "description": "依頼文で渡されたジョブ用トークン"},
					"model": map[string]any{"type": "string", "description": "モデルの候補のいずれか。空文字なら CLI の既定のモデルに戻す"},
				},
				"required": []string{"token", "model"},
			},
		}}, leaseToolDefs()...)}, nil)
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Token   string `json:"token"`
				Model   string `json:"model"`
				Name    string `json:"name"`
				Minutes int    `json:"minutes"`
			} `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil || !slices.Contains(selfToolNames, p.Name) {
			writeRPC(w, m.ID, nil, &rpcError{-32602, "unknown tool"})
			return
		}
		var msg string
		var err error
		switch p.Name {
		case leaseToolName:
			msg, err = r.leaseByAgent(p.Arguments.Token, p.Arguments.Name, p.Arguments.Minutes)
		case releaseToolName:
			msg, err = r.releaseByAgent(p.Arguments.Token, p.Arguments.Name)
		default:
			msg, err = r.setModelByAgent(p.Arguments.Token, p.Arguments.Model)
		}
		text := msg
		if err != nil {
			text = err.Error()
		}
		writeRPC(w, m.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"isError": err != nil,
		}, nil)
	default:
		writeRPC(w, m.ID, nil, &rpcError{-32601, "method not found"})
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	res := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		res["error"] = e
	} else {
		res["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// setModelByAgent はエージェント自身からのモデル変更要求を検証して適用する。
// 受付条件: トークンが実行中のジョブのもの・世代が一致・起動後に人間がモデルを変更していない・候補にあるモデル
func (r *Room) setModelByAgent(token, model string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.jobs[token]
	if !ok {
		r.log.Warn("agent.model.set.rejected", "reason", "invalid_token")
		return "", fmt.Errorf("トークンが無効です（ジョブが終了しているか、誤ったトークンです）")
	}
	a := r.agent(t.agentID)
	// 不正なモデル名にトークンが含まれ得るので、ログ・応答では伏せる
	shown := model
	for tok := range r.jobs {
		shown = strings.ReplaceAll(shown, tok, redactedToken)
	}
	log := r.log.With("agent", t.agentID, "model", shown, "by", "agent")
	switch {
	case t.gen != r.gen:
		log.Warn("agent.model.set.rejected", "reason", "generation")
		return "", fmt.Errorf("会話がリセットまたは停止されたため受け付けられません")
	case t.modelVer != a.modelVer:
		log.Warn("agent.model.set.rejected", "reason", "version")
		return "", fmt.Errorf("このジョブの起動後にモデル設定が変更されたため受け付けられません")
	case model != "" && !slices.ContainsFunc(a.Adapter.Models(), func(o ModelOption) bool { return o.ID == model }):
		log.Warn("agent.model.set.rejected", "reason", "not_candidate")
		return "", fmt.Errorf("%q はモデルの候補にありません", shown)
	}
	if a.modelSel != model {
		r.applyModelLocked(a, model, "agent")
	}
	t.modelVer = a.modelVer // 同じジョブからの続けての変更は受け付ける
	return fmt.Sprintf("次の発言から %s を使います。", firstNonEmpty(model, "既定のモデル")), nil
}
