package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type evaluation struct {
	CP   int
	Move string
}
type analyser interface {
	Analyse(string, int) (evaluation, error)
}
type engine struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	lines  chan string
	cancel context.CancelFunc
}

func enginePath() string {
	if p := os.Getenv("STOCKFISH_PATH"); p != "" {
		return p
	}
	if p, e := exec.LookPath("stockfish"); e == nil {
		return p
	}
	found := ""
	filepath.WalkDir("engines", func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && strings.Contains(strings.ToLower(d.Name()), "stockfish") && (strings.HasSuffix(p, ".exe") || filepath.Ext(p) == "") {
			found = p
		}
		return nil
	})
	return found
}

func newEngine(path string) (*engine, error) {
	if path == "" {
		return nil, fmt.Errorf("Stockfish not found. Install it in Settings")
	}
	if abs, e := filepath.Abs(path); e == nil {
		if _, e = os.Stat(abs); e == nil {
			path = abs
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, path)
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("cannot start Stockfish: %w", err)
	}
	e := &engine{cmd, in, make(chan string, 256), cancel}
	go func() {
		defer close(e.lines)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			select {
			case e.lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	err = e.send("uci")
	if err == nil {
		err = e.until("uciok")
	}
	if err == nil {
		err = e.send("setoption name Threads value 1")
	}
	if err == nil {
		err = e.send("setoption name Hash value 16")
	}
	if err == nil {
		err = e.send("isready")
	}
	if err == nil {
		err = e.until("readyok")
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}
func (e *engine) send(s string) error {
	if _, err := fmt.Fprintln(e.in, s); err != nil {
		return fmt.Errorf("cannot write to Stockfish: %w", err)
	}
	return nil
}
func (e *engine) until(want string) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case s, ok := <-e.lines:
			if !ok {
				return fmt.Errorf("Stockfish stopped unexpectedly")
			}
			if strings.HasPrefix(s, want) {
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("Stockfish timed out")
		}
	}
}
func (e *engine) Analyse(fen string, ms int) (evaluation, error) {
	result := evaluation{}
	if err := e.send("position fen " + fen); err != nil {
		return result, err
	}
	if err := e.send(fmt.Sprintf("go movetime %d", ms)); err != nil {
		return result, err
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-e.lines:
			if !ok {
				return result, fmt.Errorf("Stockfish stopped unexpectedly")
			}
			f := strings.Fields(line)
			for i := 0; i+2 < len(f); i++ {
				if f[i] == "score" {
					n, _ := strconv.Atoi(f[i+2])
					if f[i+1] == "mate" {
						if n > 0 {
							n = 10000
						} else {
							n = -10000
						}
					}
					result.CP = n
				}
			}
			if len(f) > 1 && f[0] == "bestmove" {
				result.Move = f[1]
				return result, nil
			}
		case <-timer.C:
			return result, fmt.Errorf("Stockfish search timed out")
		}
	}
}
func (e *engine) Close() { e.cancel(); e.in.Close(); e.cmd.Wait() }
