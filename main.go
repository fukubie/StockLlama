package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/notnil/chess"
)

//go:embed ui/index.html
var page string

//go:embed ui/app.js
var javascript string

type job struct {
	Busy    bool   `json:"busy"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Message string `json:"message"`
	Error   string `json:"error"`
	Percent int    `json:"percent"`
}
type app struct {
	mu         sync.Mutex
	settings   settings
	current    *profile
	game       *chess.Game
	human      chess.Color
	job        job
	dir        string
	token      string
	ollama     *exec.Cmd
	playEngine *engine
}

func newApp(dir string) *app {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		panic(err)
	}
	a := &app{dir: dir, token: hex.EncodeToString(token), settings: defaultSettings(), game: chess.NewGame(), human: chess.White}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err == nil {
		s := defaultSettings()
		if json.Unmarshal(data, &s) == nil && validSettings(s) == nil {
			s.APIKey = ""
			a.settings = s
		}
	}
	return a
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// errBadDrill is returned when a saved drill position cannot be parsed.
var errBadDrill = fmt.Errorf("this drill position is invalid; re-analyse the player to rebuild drills")

func fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	return json.NewDecoder(r.Body).Decode(v)
}

var syncFile = (*os.File).Sync

func atomicJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".save-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = syncFile(f)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (a *app) saveProfile(p *profile) error {
	return atomicJSON(filepath.Join(a.dir, "profiles", strings.ToLower(p.Username)+".json"), p)
}
func (a *app) profiles() []string {
	entries, _ := os.ReadDir(filepath.Join(a.dir, "profiles"))
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return names
}
func (a *app) state() map[string]any {
	settings := a.settings
	hasKey := settings.APIKey != ""
	settings.APIKey = ""
	settings.Folder = ""
	settings.Engine = ""
	return map[string]any{"settings": settings, "has_key": hasKey, "profile": profileSummary(a.current), "profiles": a.profiles(), "job": a.job, "board": a.board(""), "engine_installed": a.settings.Engine != ""}
}
func (a *app) board(message string) map[string]any {
	pos := a.game.Position()
	moves := []string{}
	for _, m := range a.game.ValidMoves() {
		moves = append(moves, m.String())
	}
	return map[string]any{"fen": pos.String(), "legal": moves, "turn": strings.ToLower(pos.Turn().Name()), "human": strings.ToLower(a.human.Name()), "outcome": a.game.Outcome(), "message": message}
}
func (a *app) begin(message string, fn func() error) error {
	if a.job.Busy {
		return fmt.Errorf("another task is running")
	}
	a.job = job{Busy: true, Message: message}
	go func() {
		err := fn()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.job.Busy = false
		if err != nil {
			a.job.Error = err.Error()
			a.job.Message = "Task failed"
		} else {
			a.job.Message = "Ready"
			a.job.Percent = 100
		}
	}()
	return nil
}
func (a *app) learn(username string, limit int, filename, content string) error {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var games []rawGame
	var p *profile
	var err error
	if filename != "" {
		games, p, err = importData(filename, []byte(content))
	} else {
		games, _, err = a.recent(ctx, username)
		if err == nil {
			if limit == 0 {
				limit = recommendedGames(games)
			}
			games = games[:min(limit, len(games))]
		}
	}
	if err != nil {
		return err
	}
	if p == nil {
		if len(games) > 200 {
			return fmt.Errorf("import at most 200 games at a time")
		}
		e, err := newEngine(s.Engine)
		if err != nil {
			return err
		}
		defer e.Close()
		p, err = buildProfile(games, username, e, func(done, total int) {
			a.mu.Lock()
			a.job.Done = done
			a.job.Total = total
			a.job.Message = "Analysing moves"
			a.job.Percent = min(99, 10+done*89/max(1, total))
			a.mu.Unlock()
		})
		if err != nil {
			return err
		}
	}
	if err = a.saveProfile(p); err != nil {
		return err
	}
	a.mu.Lock()
	a.current = p
	a.game = chess.NewGame()
	a.human = chess.White
	a.mu.Unlock()
	return nil
}
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		io.WriteString(w, javascript)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, strings.ReplaceAll(page, "__TOKEN__", a.token))
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		writeJSON(w, a.state())
	})
	mux.HandleFunc("GET /api/progress", func(w http.ResponseWriter, r *http.Request) { a.mu.Lock(); defer a.mu.Unlock(); writeJSON(w, a.job) })
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		s := a.settings
		s.APIKey = ""
		writeJSON(w, s)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]bool{"ready": true}) })
	mux.HandleFunc("POST /api/preview", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username string `json:"username"`
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		var games []rawGame
		var err error
		cached := false
		if in.Filename != "" {
			var p *profile
			games, p, err = importData(in.Filename, []byte(in.Content))
			if err == nil && p != nil {
				writeJSON(w, map[string]bool{"profile": true})
				return
			}
			if err == nil {
				if !usernameRE.MatchString(in.Username) {
					fail(w, fmt.Errorf("enter the player name from your games"))
					return
				}
				games = games[:min(8, len(games))]
				err = atomicJSON(filepath.Join(a.dir, "games", strings.ToLower(in.Username)+".json"), sample{games, time.Now().Unix()})
			}
		} else {
			games, cached, err = a.recent(r.Context(), in.Username)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"available": len(games), "recommended": recommendedGames(games), "cached": cached})
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			settings
			ClearKey bool `json:"clear_key"`
		}
		if err := decode(w, r, &input); err != nil {
			fail(w, err)
			return
		}
		if err := validSettings(input.settings); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.job.Busy {
			fail(w, fmt.Errorf("wait for the current task before changing settings"))
			return
		}
		s := input.settings
		if s.APIKey == "" && !input.ClearKey {
			s.APIKey = a.settings.APIKey
		}
		safe := s
		safe.APIKey = ""
		if err := atomicJSON(filepath.Join(a.dir, "settings.json"), safe); err != nil {
			fail(w, err)
			return
		}
		if s.Engine != a.settings.Engine {
			a.closeEngine()
		}
		a.settings = s
		writeJSON(w, map[string]bool{"saved": true, "has_key": s.APIKey != ""})
	})
	mux.HandleFunc("POST /api/models", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		s := a.settings
		a.mu.Unlock()
		names, message := detectModels(r.Context(), s)
		writeJSON(w, map[string]any{"models": names, "message": message})
	})
	mux.HandleFunc("GET /api/hardware", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		s := a.settings
		a.mu.Unlock()
		writeJSON(w, gpuUsage(r.Context(), s))
	})
	mux.HandleFunc("POST /api/hint", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode  string `json:"mode"`
			Index int    `json:"index"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == nil {
			fail(w, fmt.Errorf("load a player first"))
			return
		}
		if a.job.Busy {
			fail(w, fmt.Errorf("wait for analysis to finish"))
			return
		}
		pos := a.game.Position()
		if in.Mode == "drill" && in.Index >= 0 && in.Index < len(a.current.Puzzles) {
			g, err := gameAt(a.current.Puzzles[in.Index].FEN)
			if err != nil {
				fail(w, errBadDrill)
				return
			}
			pos = g.Position()
		}
		text, err := positionAdvice(a.current, pos, a)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]string{"text": shortText(text)})
	})
	mux.HandleFunc("POST /api/browse", func(w http.ResponseWriter, r *http.Request) {
		if runtime.GOOS != "windows" {
			fail(w, fmt.Errorf("paste the folder path on this operating system"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", `Add-Type -AssemblyName System.Windows.Forms; $picker = New-Object System.Windows.Forms.FolderBrowserDialog; $picker.Description = 'Choose your Ollama models folder'; if ($picker.ShowDialog() -eq 'OK') { [Console]::Write($picker.SelectedPath) }`)
		out, err := cmd.Output()
		if err != nil {
			fail(w, fmt.Errorf("folder picker unavailable; paste the path instead"))
			return
		}
		writeJSON(w, map[string]string{"folder": strings.TrimSpace(string(out))})
	})
	mux.HandleFunc("POST /api/ollama/start", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.ollama != nil {
			fail(w, fmt.Errorf("an app-managed Ollama instance is already running"))
			return
		}
		cmd, err := startOllama(a.settings)
		if err != nil {
			fail(w, err)
			return
		}
		a.ollama = cmd
		writeJSON(w, map[string]string{"message": "Ollama starting on port 11435. Wait a moment, then Detect models."})
	})
	mux.HandleFunc("POST /api/engine/install", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		err := a.begin("Downloading official Stockfish…", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			path, err := installEngine(ctx, func(percent int) { a.mu.Lock(); a.job.Percent = min(95, percent); a.mu.Unlock() })
			if err != nil {
				return err
			}
			e, err := newEngine(path)
			if err != nil {
				return err
			}
			e.Close()
			a.mu.Lock()
			defer a.mu.Unlock()
			a.closeEngine()
			a.settings.Engine = path
			s := a.settings
			s.APIKey = ""
			return atomicJSON(filepath.Join(a.dir, "settings.json"), s)
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"started": true})
	})
	mux.HandleFunc("POST /api/learn", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username string `json:"username"`
			Limit    int    `json:"limit"`
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		if in.Filename == "" && (!usernameRE.MatchString(in.Username) || in.Limit < 0 || in.Limit > 200) {
			fail(w, fmt.Errorf("enter a valid username and 1–200 games"))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		err := a.begin("Fetching games…", func() error { return a.learn(in.Username, in.Limit, in.Filename, in.Content) })
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"started": true})
	})
	mux.HandleFunc("POST /api/load", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username string `json:"username"`
		}
		if err := decode(w, r, &in); err != nil || !usernameRE.MatchString(in.Username) {
			fail(w, fmt.Errorf("invalid saved player"))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.job.Busy {
			fail(w, fmt.Errorf("wait for analysis to finish"))
			return
		}
		data, err := os.ReadFile(filepath.Join(a.dir, "profiles", strings.ToLower(in.Username)+".json"))
		if err != nil {
			fail(w, fmt.Errorf("saved player not found"))
			return
		}
		var p profile
		if err = json.Unmarshal(data, &p); err == nil {
			err = validateProfile(&p)
		}
		if err != nil {
			fail(w, err)
			return
		}
		a.current = &p
		a.game = chess.NewGame()
		a.human = chess.White
		writeJSON(w, a.state())
	})
	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == nil {
			fail(w, fmt.Errorf("load a player first"))
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+a.current.Username+`.json"`)
		writeJSON(w, a.current)
	})
	mux.HandleFunc("POST /api/coach", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode     string `json:"mode"`
			Index    int    `json:"index"`
			Question string `json:"question"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		p, s := a.current, a.settings
		g := a.game.Clone()
		if p != nil && in.Mode == "drill" && in.Index >= 0 && in.Index < len(p.Puzzles) {
			var err error
			if g, err = gameAt(p.Puzzles[in.Index].FEN); err != nil {
				a.mu.Unlock()
				fail(w, errBadDrill)
				return
			}
		}
		if p == nil {
			a.mu.Unlock()
			fail(w, fmt.Errorf("load a player first"))
			return
		}
		if a.job.Busy {
			a.mu.Unlock()
			fail(w, fmt.Errorf("wait for analysis to finish"))
			return
		}
		facts, err := positionAdvice(p, g.Position(), a)
		a.mu.Unlock()
		if err != nil {
			fail(w, err)
			return
		}
		answer, err := coachWithFacts(r.Context(), p, s, facts, in.Question)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]string{"text": answer})
	})
	mux.HandleFunc("POST /api/play", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Move  string `json:"move"`
			Reset bool   `json:"reset"`
			Color string `json:"color"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == nil {
			fail(w, fmt.Errorf("load a player first"))
			return
		}
		if a.job.Busy {
			fail(w, fmt.Errorf("wait for analysis to finish"))
			return
		}
		if in.Reset {
			a.game = chess.NewGame()
			a.human = chess.White
			if in.Color == "black" {
				a.human = chess.Black
			}
		}
		if a.game.Outcome() != chess.NoOutcome {
			writeJSON(w, a.board("Game finished. Start a new game to continue."))
			return
		}
		snapshot := a.game.Clone()
		if in.Move != "" {
			if a.game.Position().Turn() != a.human {
				fail(w, fmt.Errorf("wait for opponent move"))
				return
			}
			m, err := legalMove(a.game.Position(), in.Move)
			if err != nil {
				fail(w, err)
				return
			}
			a.game.Move(m)
		}
		message := "Your move"
		if a.game.Outcome() == chess.NoOutcome && a.game.Position().Turn() != a.human {
			m, source, err := chooseMove(a.game.Position(), a.current, a)
			if err != nil {
				a.game = snapshot
				fail(w, err)
				return
			}
			message = (chess.AlgebraicNotation{}).Encode(a.game.Position(), m) + " " + source
			a.game.Move(m)
		}
		writeJSON(w, a.board(message))
	})
	mux.HandleFunc("POST /api/drill", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Index  int    `json:"index"`
			Move   string `json:"move"`
			Reveal bool   `json:"reveal"`
		}
		if err := decode(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == nil || in.Index < 0 || in.Index >= len(a.current.Puzzles) {
			fail(w, fmt.Errorf("no drill at this index"))
			return
		}
		q := a.current.Puzzles[in.Index]
		g, err := gameAt(q.FEN)
		if err != nil {
			fail(w, errBadDrill)
			return
		}
		correct := false
		message := "Find the strongest reply"
		if in.Move != "" {
			m, err := legalMove(g.Position(), in.Move)
			if err != nil {
				fail(w, err)
				return
			}
			correct = m.String() == q.Best
			if correct {
				message = "Correct — you found the engine's preferred reply."
			} else {
				message = "Try again, or reveal the engine's preferred reply."
			}
		}
		result := map[string]any{"fen": q.FEN, "correct": correct, "message": message, "loss": q.Loss, "legal": []string{}}
		legal := []string{}
		for _, m := range g.ValidMoves() {
			legal = append(legal, m.String())
		}
		result["legal"] = legal
		if in.Reveal || correct {
			m, _ := legalMove(g.Position(), q.Best)
			result["solution"] = (chess.AlgebraicNotation{}).Encode(g.Position(), m)
		}
		writeJSON(w, result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			http.Error(w, "local access only", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host || u.Scheme != "http" {
				http.Error(w, "origin rejected", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-StockLlama") != a.token {
			http.Error(w, "session rejected; reload the app", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}
func main() {
	a := newApp("data")
	address := os.Getenv("STOCKLLAMA_ADDR")
	if address == "" {
		address = "127.0.0.1:8765"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		log.Fatal("STOCKLLAMA_ADDR must bind to 127.0.0.1")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("Cannot start StockLlama: ", err)
	}
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() { <-stop; server.Close() }()
	fmt.Printf("StockLlama is ready: http://%s\nPress Ctrl+C to stop.\n", listener.Addr())
	err = server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		log.Print(err)
	}
	if a.ollama != nil {
		a.ollama.Process.Kill()
		a.ollama.Wait()
	}
	a.closeEngine()
}
