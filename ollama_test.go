package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("STOCKLLAMA_TEST_OLLAMA") == "1" && len(os.Args) == 2 && os.Args[1] == "serve" {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestStartOllamaAgainAfterChildExits(t *testing.T) {
	t.Setenv("STOCKLLAMA_TEST_OLLAMA", "1")
	folder := t.TempDir()
	if err := os.Mkdir(filepath.Join(folder, "manifests"), 0700); err != nil {
		t.Fatal(err)
	}
	name := "ollama"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(folder, name), os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(dst, src)
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if copyErr != nil {
		t.Fatal(copyErr)
	}

	a := newApp(t.TempDir())
	t.Cleanup(a.stopOllama)
	a.settings = settings{Provider: "ollama", Endpoint: "http://127.0.0.1:11435", Folder: folder}
	start := func() (int, []byte) {
		return request(t, a, http.MethodPost, "/api/ollama/start", nil)
	}
	if status, body := start(); status != http.StatusOK {
		t.Fatalf("first start returned %d: %s", status, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := start()
		if status == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the app still refuses to start Ollama after the child exited")
}
