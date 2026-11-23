package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// deadEngine returns an engine whose stdin write side is already broken, which is
// what the callers see once Stockfish has exited.
func deadEngine(t *testing.T) *engine {
	t.Helper()
	pr, pw := io.Pipe()
	pr.Close()
	t.Cleanup(func() { pw.Close() })
	return &engine{in: pw, lines: make(chan string)}
}

func TestSendReportsWriteError(t *testing.T) {
	e := deadEngine(t)
	if err := e.send("uci"); err == nil {
		t.Fatal("send returned nil after the write failed")
	}
}

func TestAnalyseFailsFastWhenWriteFails(t *testing.T) {
	e := deadEngine(t)
	start := time.Now()
	if _, err := e.Analyse("startpos", 10); err == nil {
		t.Fatal("Analyse returned nil error after the write failed")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Analyse waited %v before reporting a dead engine", d)
	}
}

func TestNewEngineFailsWhenProcessExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	path := filepath.Join(t.TempDir(), "fake-stockfish")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := newEngine(path); err == nil {
		t.Fatal("newEngine succeeded with a process that exits immediately")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("newEngine waited %v for a process that already exited", d)
	}
}
