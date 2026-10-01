package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileVersionsDiff(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.go", "same")
	write("edit.go", "old")
	write("gone.go", "x")
	write(".git/HEAD", "ref")
	write("logs/chat.jsonl", "a")

	logDir := filepath.Join(dir, "logs")
	before := takeFileSnapshot(dir, logDir)
	if _, ok := before["logs/chat.jsonl"]; ok {
		t.Fatal("ログの置き場を記録している")
	}
	if _, ok := before[".git/HEAD"]; ok {
		t.Fatal("点で始まるディレクトリを記録している")
	}

	write("edit.go", "new body")
	// 更新時刻の分解能が粗いファイルシステムでも差分が出るよう、時刻を明示してずらす
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(dir, "edit.go"), future, future)
	write("sub/new.go", "hello")
	os.Remove(filepath.Join(dir, "gone.go"))
	write("logs/chat.jsonl", "ab")

	changes := diffFileSnapshots(dir, before, takeFileSnapshot(dir, logDir))
	if len(changes) != 3 {
		t.Fatalf("差分は3件のはず: %+v", changes)
	}
	if c := changes[0]; c.path != "edit.go" || c.added || c.deleted || c.hash != "ae907ab9" {
		t.Errorf("edit.go: %+v", c)
	}
	if c := changes[1]; c.path != "gone.go" || !c.deleted {
		t.Errorf("gone.go: %+v", c)
	}
	if c := changes[2]; c.path != "sub/new.go" || !c.added || len(c.hash) != fileVerHashLen {
		t.Errorf("sub/new.go: %+v", c)
	}

	text := formatFileVersions("Claude Code 2", changes)
	for _, want := range []string{"Claude Code 2 のターンの間に変わったファイル（3件）", "`edit.go` 更新", "sha256:ae907ab9", "`gone.go` 削除", "`sub/new.go` 新規"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q を含まない:\n%s", want, text)
		}
	}
}

func TestFileVersionsNoChange(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	snap := takeFileSnapshot(dir)
	if got := formatFileVersions("x", diffFileSnapshots(dir, snap, takeFileSnapshot(dir))); got != "" {
		t.Errorf("変更がないのに本文がある: %q", got)
	}
	// 記録できなかった場合（nil）は何も出さない
	if got := diffFileSnapshots(dir, nil, snap); got != nil {
		t.Errorf("記録がないのに差分がある: %+v", got)
	}
}

func TestFileVersionsListLimit(t *testing.T) {
	var changes []fileChange
	for range fileVerMaxList + 5 {
		changes = append(changes, fileChange{path: "f", deleted: true})
	}
	if text := formatFileVersions("x", changes); !strings.Contains(text, "ほか 5件") {
		t.Errorf("上限を超えた分がまとめられていない:\n%s", text)
	}
}
