package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeStateFile(path, []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := writeStateFile(path, []byte("2")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "2" {
		t.Fatalf("content = %q, want 2", b)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp file remains: %v", err)
	}
}

// 置き換えに失敗しても一時ファイルを残さない
func TestWriteStateFileRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o755); err != nil { // 置き換え先がディレクトリだと Rename は失敗する
		t.Fatal(err)
	}
	if err := writeStateFile(path, []byte("1")); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp file remains: %v", err)
	}
}

// 一時ファイルを書けない場合もエラーを返す
func TestWriteStateFileWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := writeStateFile(path, []byte("1")); err == nil {
		t.Fatal("want error")
	}
}
