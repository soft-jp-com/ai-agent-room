package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthTokenPersists(t *testing.T) {
	dir, secret := t.TempDir(), t.TempDir()
	a, err := loadOrCreateAuthToken(dir, secret)
	if err != nil || len(a) != 64 {
		t.Fatalf("token=%q err=%v", a, err)
	}
	b, _ := loadOrCreateAuthToken(dir, secret)
	if a != b {
		t.Fatalf("再起動でトークンが変わった: %q → %q", a, b)
	}
	if _, err := os.Stat(authTokenPath(dir, secret)); err != nil {
		t.Fatal(err)
	}
	// ログディレクトリ（エージェントから読める場所）には置かない
	if _, err := os.Stat(filepath.Join(dir, authTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("ログディレクトリにトークンがある: %v", err)
	}
	// ログディレクトリが違えば別のトークン
	if c, _ := loadOrCreateAuthToken(t.TempDir(), secret); c == a {
		t.Fatal("別のログディレクトリで同じトークンになった")
	}
}

// 以前の保存先（logDir/auth-token）のトークンを引き継ぎ、古いファイルは消す
func TestAuthTokenMigratesFromLogDir(t *testing.T) {
	dir, secret := t.TempDir(), t.TempDir()
	const old = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, authTokenFile), []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrCreateAuthToken(dir, secret)
	if err != nil || got != old {
		t.Fatalf("引き継げない: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, authTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("古いファイルが残っている: %v", err)
	}
	if again, _ := loadOrCreateAuthToken(dir, secret); again != old {
		t.Fatalf("移したあとの再起動で変わった: %q", again)
	}
}

func TestRequireAuth(t *testing.T) {
	const port, token = 8787, "0123456789abcdef0123456789abcdef"
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := requireAuth(port, token, ok)
	cookie := &http.Cookie{Name: authCookieName(port), Value: token}

	do := func(method, target string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	cases := []struct {
		name   string
		method string
		target string
		cookie *http.Cookie
		want   int
	}{
		{"API はトークンなしで 401", "POST", "/api/messages", nil, http.StatusUnauthorized},
		{"SSE もトークンなしで 401", "GET", "/api/events", nil, http.StatusUnauthorized},
		{"違うトークンの Cookie は 401", "GET", "/api/models", &http.Cookie{Name: authCookieName(port), Value: "x"}, http.StatusUnauthorized},
		{"別ポートの Cookie は 401", "GET", "/api/models", &http.Cookie{Name: authCookieName(9999), Value: token}, http.StatusUnauthorized},
		{"正しい Cookie なら通る", "POST", "/api/messages", cookie, http.StatusOK},
		{"API のクエリのトークンは使えない", "GET", "/api/models?token=" + token, nil, http.StatusUnauthorized},
		{"画面の HTML は認証なしで返す", "GET", "/", nil, http.StatusOK},
		{"/mcp はジョブ用トークンで確かめるので対象外", "POST", "/mcp", nil, http.StatusOK},
		{"違うトークンの URL は 401", "GET", "/?token=wrong", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		if w := do(c.method, c.target, c.cookie); w.Code != c.want {
			t.Errorf("%s: status=%d want=%d", c.name, w.Code, c.want)
		}
	}

	// 起動時の URL で開くと Cookie が付き、トークンを消した URL へ移る
	w := do("GET", "/live.html?id=claude&token="+token, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/live.html?id=claude" {
		t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
	res := w.Result()
	var got *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == authCookieName(port) {
			got = c
		}
	}
	if got == nil || got.Value != token || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie=%+v", got)
	}
}

// 改名前の保存先（ai_chat）は、新しい保存先がなければフォルダごと移す。新しい保存先があれば触らない
func TestMigrateDir(t *testing.T) {
	base := t.TempDir()
	from, to := filepath.Join(base, legacyAppDirName), filepath.Join(base, appDirName)
	if err := os.MkdirAll(filepath.Join(from, configDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "auth-token-x"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotFrom, gotTo, err := migrateDir(from, to)
	if err != nil || gotFrom != from || gotTo != to {
		t.Fatalf("移せない: %q %q %v", gotFrom, gotTo, err)
	}
	if b, err := os.ReadFile(filepath.Join(to, "auth-token-x")); err != nil || string(b) != "t" {
		t.Fatalf("移した先にトークンがない: %q %v", b, err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("移動元が残っている: %v", err)
	}
	// 2回目（移行済み）と、両方ある場合は何もしない
	if f, _, err := migrateDir(from, to); f != "" || err != nil {
		t.Fatalf("移行済みなのに移した: %q %v", f, err)
	}
	os.MkdirAll(from, 0o700)
	if f, _, err := migrateDir(from, to); f != "" || err != nil {
		t.Fatalf("新しい保存先があるのに移した: %q %v", f, err)
	}
}
