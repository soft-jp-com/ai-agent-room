package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 画面の API の認証。
// 起動時に表示する URL（?token=...）でトークンを渡し、画面はそれを Cookie（HttpOnly、SameSite=Strict）で保持する。
// トークンは作業ディレクトリの外（%LOCALAPPDATA%\ai-agent-room）に保存し、再起動しても開いたままの画面が使えるようにする。
// エージェントがトークンを読んで実行 API などを直接呼べないようにするため（ログディレクトリはエージェントから読める）。
// /mcp はジョブ用トークンで確かめるので対象外。静的ファイル（画面の HTML）は秘密を含まないので対象外。

const authTokenFile = "auth-token"

// authTokenPath はトークンの保存先を返す。ログディレクトリは作業ディレクトリの下にあってエージェントから読めるので、
// その外（secretDir。既定は %LOCALAPPDATA%\ai-agent-room）に置く。ログディレクトリごとに別のファイルにする
func authTokenPath(logDir, secretDir string) string {
	return filepath.Join(secretDir, authTokenFile+"-"+logDirKey(logDir))
}

// logDirKey はログディレクトリごとに異なる短い識別子（同じ PC で別の AI Agent Room を動かしても保存先が混ざらないように）
func logDirKey(logDir string) string {
	abs, err := filepath.Abs(logDir)
	if err != nil {
		abs = logDir
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return hex.EncodeToString(sum[:6])
}

// appDirName は秘密と設定の保存先のフォルダ名。legacyAppDirName は改名前（AI Chat）のフォルダ名
const (
	appDirName       = "ai-agent-room"
	legacyAppDirName = "ai_chat"
)

// defaultSecretDir は、エージェントの作業ディレクトリの外にある、秘密の保存先
// 環境変数 AI_AGENT_ROOM_CONFIG_DIR で変えられる。既定は OS ごとの標準の場所
// （Windows: %LOCALAPPDATA%\ai-agent-room、macOS: ~/Library/Application Support/ai-agent-room、Linux: ~/.config/ai-agent-room）
func defaultSecretDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("AI_AGENT_ROOM_CONFIG_DIR")); d != "" {
		return filepath.Abs(d)
	}
	base, err := secretBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDirName), nil
}

// secretBaseDir は保存先のフォルダを置く OS ごとの標準の場所
func secretBaseDir() (string, error) {
	if runtime.GOOS == "windows" {
		return os.UserCacheDir() // Roaming ではなく Local に置く（端末の外に同期させない）
	}
	return os.UserConfigDir()
}

// migrateLegacySecretDir は、改名前の保存先（…\ai_chat）があり新しい保存先がなければ、フォルダごと新しい名前に移す。
// 認証トークンと設定を引き継ぐため。移したら移動元と移動先を返す（移すものがなければ空）。
// AI_AGENT_ROOM_CONFIG_DIR で保存先を指定しているときは何もしない
func migrateLegacySecretDir() (from, to string, err error) {
	if strings.TrimSpace(os.Getenv("AI_AGENT_ROOM_CONFIG_DIR")) != "" {
		return "", "", nil
	}
	base, err := secretBaseDir()
	if err != nil {
		return "", "", err
	}
	return migrateDir(filepath.Join(base, legacyAppDirName), filepath.Join(base, appDirName))
}

// migrateDir は from があり to がなければ from を to に移す
func migrateDir(from, to string) (string, string, error) {
	if st, err := os.Stat(from); err != nil || !st.IsDir() {
		return "", "", nil
	}
	if _, err := os.Stat(to); err == nil {
		return "", "", nil // 新しい保存先がすでにある（移行済み）。旧いフォルダは人間が確かめて消す
	}
	if err := os.Rename(from, to); err != nil {
		return "", "", err
	}
	return from, to, nil
}

// loadOrCreateAuthToken は保存済みのトークンを読み、なければ乱数で作って保存する。
// 以前の保存先（logDir/auth-token）にあれば引き継いで新しい保存先へ移し、古いファイルは消す（開いている画面をそのまま使えるようにする）
func loadOrCreateAuthToken(logDir, secretDir string) (string, error) {
	path := authTokenPath(logDir, secretDir)
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			os.Remove(filepath.Join(logDir, authTokenFile)) // 移す途中で終わった場合の残り
			return t, nil
		}
	}
	oldPath := filepath.Join(logDir, authTokenFile)
	var t string
	if b, err := os.ReadFile(oldPath); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			t = s
		}
	}
	if t == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		t = hex.EncodeToString(b)
	}
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(t), 0o600); err != nil {
		return "", err
	}
	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("以前のトークンのファイルを消せません（%s）: %w", oldPath, err)
	}
	return t, nil
}

// authCookieName はポートごとに分ける。Cookie はポートを区別しないため、別のポートで動く AI Agent Room と混ざらないようにする。
func authCookieName(port int) string { return fmt.Sprintf("ai_agent_room_token_%d", port) }

func tokenEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func requireAuth(port int, token string, next http.Handler) http.Handler {
	name := authCookieName(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 起動時の URL で開いたときは Cookie を渡し、アドレスバーからトークンを消す
		if q := r.URL.Query().Get("token"); q != "" && r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			if !tokenEqual(q, token) {
				writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "トークンが正しくありません。起動時に表示された URL から開き直してください")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			q := r.URL.Query()
			q.Del("token")
			u := r.URL.Path
			if len(q) > 0 {
				u += "?" + q.Encode()
			}
			http.Redirect(w, r, u, http.StatusSeeOther)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			c, err := r.Cookie(name)
			if err != nil || !tokenEqual(c.Value, token) {
				writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "認証されていません。起動時に表示された URL から開き直してください")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
