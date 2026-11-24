package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/notnil/chess"
)

func request(t *testing.T, a *app, method, path string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, bytes.NewReader(data))
	r.Header.Set("X-StockLlama", a.token)
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func TestSecurity(t *testing.T) {
	a := newApp(t.TempDir())
	for _, tc := range []struct {
		name, host, origin, token string
		status                    int
	}{
		{"valid", "127.0.0.1:8765", "http://127.0.0.1:8765", a.token, 200},
		{"no token", "127.0.0.1:8765", "", "", 403},
		{"foreign origin", "127.0.0.1:8765", "https://evil.invalid", a.token, 403},
		{"DNS rebinding", "evil.invalid:8765", "", a.token, 403},
		{"localhost", "localhost:8765", "http://localhost:8765", a.token, 200},
		{"IPv6 loopback", "[::1]:8765", "http://[::1]:8765", a.token, 200},
		{"different loopback origin", "localhost:8765", "http://127.0.0.1:8765", a.token, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+"/api/settings", strings.NewReader(`{"provider":"builtin"}`))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-StockLlama", tc.token)
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
func TestSecretsNeverPersistOrReturn(t *testing.T) {
	dir := t.TempDir()
	a := newApp(dir)
	s := defaultSettings()
	s.APIKey = "test-super-secret-key"
	status, data := request(t, a, "POST", "/api/settings", s)
	if status != 200 {
		t.Fatal(string(data))
	}
	status, data = request(t, a, "GET", "/api/state", nil)
	if status != 200 || bytes.Contains(data, []byte(s.APIKey)) {
		t.Fatal("key leaked in state")
	}
	saved, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if bytes.Contains(saved, []byte(s.APIKey)) {
		t.Fatal("key written to disk")
	}
	if newApp(dir).settings.APIKey != "" {
		t.Fatal("key survived restart")
	}
	status, _ = request(t, a, "POST", "/api/settings", map[string]any{"provider": "builtin", "clear_key": true})
	if status != 200 || a.settings.APIKey != "" {
		t.Fatal("forget key failed")
	}
}
func TestProfileSaveLoadExport(t *testing.T) {
	a := newApp(t.TempDir())
	p := fixture(t)
	if err := a.saveProfile(p); err != nil {
		t.Fatal(err)
	}
	status, data := request(t, a, "POST", "/api/load", map[string]string{"username": "target"})
	if status != 200 || a.current == nil {
		t.Fatal(string(data))
	}
	status, data = request(t, a, "GET", "/api/export", nil)
	var copy profile
	if json.Unmarshal(data, &copy) != nil || status != 200 || copy.Username != p.Username {
		t.Fatal(string(data))
	}
	if len(a.profiles()) != 1 {
		t.Fatal(a.profiles())
	}
	status, _ = request(t, a, "POST", "/api/load", map[string]string{"username": "../settings"})
	if status != 400 {
		t.Fatal("accepted traversal")
	}
}
func TestLoadMissingPlayer(t *testing.T) {
	a := newApp(t.TempDir())
	code, _ := request(t, a, "POST", "/api/load", map[string]string{"username": "absent"})
	if code != 400 {
		t.Fatal(code)
	}
}
func TestNoProfileEndpoints(t *testing.T) {
	a := newApp(t.TempDir())
	for _, path := range []string{"coach", "play", "drill"} {
		t.Run(path, func(t *testing.T) {
			code, _ := request(t, a, "POST", "/api/"+path, map[string]any{})
			if code != 400 {
				t.Fatal(code)
			}
		})
	}
}
func TestDrillWrongCorrectReveal(t *testing.T) {
	a := newApp(t.TempDir())
	a.current = fixture(t)
	a.current.Puzzles = []puzzle{{FEN: chess.StartingPosition().String(), Best: "e2e4", Loss: 200}}
	for _, tc := range []struct {
		name, move                string
		reveal, correct, solution bool
	}{{"initial", "", false, false, false}, {"wrong", "d4", false, false, false}, {"correct", "e4", false, true, true}, {"reveal", "", true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			code, data := request(t, a, "POST", "/api/drill", map[string]any{"index": 0, "move": tc.move, "reveal": tc.reveal})
			var v map[string]any
			json.Unmarshal(data, &v)
			if code != 200 || v["correct"] != tc.correct || (v["solution"] != nil) != tc.solution {
				t.Fatal(code, string(data))
			}
		})
	}
	code, _ := request(t, a, "POST", "/api/drill", map[string]any{"index": -1})
	if code != 400 {
		t.Fatal(code)
	}
}
func TestCorruptedDrillFENReturnsError(t *testing.T) {
	a := newApp(t.TempDir())
	a.current = fixture(t)
	a.current.Puzzles = []puzzle{{FEN: "not a fen", Best: "e2e4", Loss: 200}}
	for _, path := range []string{"/api/hint", "/api/coach", "/api/drill"} {
		t.Run(path, func(t *testing.T) {
			code, data := request(t, a, "POST", path, map[string]any{"mode": "drill", "index": 0})
			if code != 400 || !strings.Contains(string(data), "drill position is invalid") {
				t.Fatal(code, string(data))
			}
		})
	}
}
func TestFailedEngineRollsBackHumanMove(t *testing.T) {
	a := newApp(t.TempDir())
	a.current = fixture(t)
	a.settings.Engine = "missing-engine"
	before := a.game.FEN()
	code, _ := request(t, a, "POST", "/api/play", map[string]string{"move": "e4"})
	if code != 400 || a.game.FEN() != before {
		t.Fatal("human move was not rolled back")
	}
}
func TestInvalidMoveDoesNotChangeBoard(t *testing.T) {
	a := newApp(t.TempDir())
	a.current = fixture(t)
	code, _ := request(t, a, "POST", "/api/play", map[string]string{"move": "e2e5"})
	if code != 400 || len(a.game.Moves()) != 0 {
		t.Fatal(code)
	}
}
func TestBusyJobCannotOverlap(t *testing.T) {
	a := newApp(t.TempDir())
	a.job.Busy = true
	if e := a.begin("second", func() error { return nil }); e == nil {
		t.Fatal("overlapped jobs")
	}
	code, _ := request(t, a, "POST", "/api/settings", defaultSettings())
	if code != 400 {
		t.Fatal("settings changed during a task")
	}
}
func TestImportSavedProfileOffline(t *testing.T) {
	a := newApp(t.TempDir())
	p := fixture(t)
	data, _ := json.Marshal(p)
	a.settings.Engine = "missing-engine"
	if err := a.learn("", 0, "saved.json", string(data)); err != nil {
		t.Fatal(err)
	}
	if a.current.Username != "Target" {
		t.Fatal("not loaded")
	}
}
func TestIndexAnd404(t *testing.T) {
	a := newApp(t.TempDir())
	code, data := request(t, a, "GET", "/", nil)
	if code != 200 || !bytes.Contains(data, []byte(a.token)) || bytes.Contains(data, []byte("__TOKEN__")) {
		t.Fatal("index not served")
	}
	code, _ = request(t, a, "GET", "/unknown", nil)
	if code != 404 {
		t.Fatal(code)
	}
}
func TestSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     settings
		valid bool
	}{{"offline", settings{Provider: "builtin"}, true}, {"local", settings{Provider: "ollama", Endpoint: "http://127.0.0.1:11434"}, true}, {"HTTPS", settings{Provider: "api", Endpoint: "https://example.com/v1"}, true}, {"HTTP remote", settings{Provider: "api", Endpoint: "http://example.com/v1"}, false}, {"embedded credentials", settings{Provider: "api", Endpoint: "https://secret@example.com/v1"}, false}, {"query credential", settings{Provider: "api", Endpoint: "https://example.com?key=secret"}, false}, {"remote ollama", settings{Provider: "ollama", Endpoint: "https://example.com"}, false}, {"invalid provider", settings{Provider: "unknown"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			if (validSettings(tc.s) == nil) != tc.valid {
				t.Fatal(tc.s)
			}
		})
	}
}
func TestDiskModelDiscovery(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"registry.ollama.ai/library/llama3.1/8b", "registry.ollama.ai/library/qwen3/latest"} {
		path := filepath.Join(root, "manifests", filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0700)
		os.WriteFile(path, []byte("{}"), 0600)
	}
	models := diskModels(root)
	if strings.Join(models, ",") != "llama3.1:8b,qwen3:latest" {
		t.Fatal(models)
	}
}
func TestOllamaDiscoveryAndCoach(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"local:8b"}]}`)
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if in["model"] != "local:8b" {
			t.Error(in)
		}
		fmt.Fprint(w, `{"response":"Practice tactical replies."}`)
	}))
	defer server.Close()
	s := settings{Provider: "ollama", Endpoint: server.URL, Model: "local:8b"}
	names, _ := detectModels(context.Background(), s)
	if len(names) != 1 || names[0] != "local:8b" {
		t.Fatal(names)
	}
	answer, e := coach(context.Background(), fixture(t), s)
	if e != nil || answer != "Practice tactical replies" {
		t.Fatal(answer, e)
	}
}
func TestAPIDiscoveryAndCoach(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-key" {
			t.Error("missing key")
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"coach-model"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Review your openings."}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := settings{Provider: "api", Endpoint: server.URL + "/v1", Model: "coach-model", APIKey: "private-key"}
	names, _ := detectModels(context.Background(), s)
	if len(names) != 1 || names[0] != "coach-model" {
		t.Fatal(names)
	}
	answer, e := coach(context.Background(), fixture(t), s)
	if e != nil || answer != "Review your openings" {
		t.Fatal(answer, e)
	}
}
func TestProviderErrorRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, "private-key echoed by remote")
	}))
	defer server.Close()
	s := settings{Provider: "api", Endpoint: server.URL, Model: "m", APIKey: "private-key"}
	_, e := coach(context.Background(), fixture(t), s)
	if e == nil || strings.Contains(e.Error(), "private-key") {
		t.Fatal(e)
	}
}
func TestProviderRedirectDoesNotForwardKey(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	s := settings{Provider: "api", Endpoint: source.URL, Model: "m", APIKey: "key"}
	if _, e := coach(context.Background(), fixture(t), s); e == nil || called {
		t.Fatal("followed redirect")
	}
}
func TestBuiltinCoachAndMissingModel(t *testing.T) {
	p := fixture(t)
	s := settings{Provider: "builtin"}
	answer, e := coach(context.Background(), p, s)
	if e != nil || !strings.Contains(answer, "Target") {
		t.Fatal(e)
	}
	s = settings{Provider: "ollama", Endpoint: "http://127.0.0.1:1"}
	if _, e = coach(context.Background(), p, s); e == nil {
		t.Fatal("missing model accepted")
	}
}
func TestInvalidEngine(t *testing.T) {
	if e, err := newEngine("no-such-stockfish"); err == nil {
		e.Close()
		t.Fatal("missing executable accepted")
	}
}

func TestLearnEndpointRestoresOfflineProfile(t *testing.T) {
	a := newApp(t.TempDir())
	data, _ := json.Marshal(fixture(t))
	code, body := request(t, a, "POST", "/api/learn", map[string]any{"filename": "player.json", "content": string(data)})
	if code != 200 {
		t.Fatal(string(body))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		busy, failure, p := a.job.Busy, a.job.Error, a.current
		a.mu.Unlock()
		if !busy {
			if failure != "" || p == nil {
				t.Fatal(failure)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline import stalled")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMalformedEndpointBodies(t *testing.T) {
	a := newApp(t.TempDir())
	for _, endpoint := range []string{"settings", "learn", "load", "play", "drill"} {
		t.Run(endpoint, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/"+endpoint, strings.NewReader("{"))
			r.Header.Set("X-StockLlama", a.token)
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatal(w.Code)
			}
		})
	}
}

func TestModelAndCoachEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[{"name":"test:small"}]}`) }))
	defer server.Close()
	a := newApp(t.TempDir())
	a.settings = settings{Provider: "ollama", Endpoint: server.URL}
	code, data := request(t, a, "POST", "/api/models", map[string]any{})
	if code != 200 || !strings.Contains(string(data), "test:small") {
		t.Fatal(string(data))
	}
	a.current = fixture(t)
	a.settings.Provider = "builtin"
	code, data = request(t, a, "POST", "/api/coach", map[string]any{})
	if code != 200 || !strings.Contains(string(data), "Target") {
		t.Fatal(string(data))
	}
}

func TestOllamaStartValidation(t *testing.T) {
	for _, s := range []settings{{Provider: "builtin"}, {Provider: "ollama", Endpoint: "http://127.0.0.1:11434"}, {Provider: "ollama", Endpoint: "http://127.0.0.1:11435", Folder: t.TempDir()}} {
		if cmd, e := startOllama(s); e == nil {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("accepted invalid Ollama configuration")
		}
	}
}

func TestDiskDiscoveryFallback(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifests", "registry.ollama.ai", "library", "local", "small")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte("{}"), 0600)
	s := settings{Provider: "ollama", Endpoint: "http://127.0.0.1:1", Folder: root}
	names, message := detectModels(context.Background(), s)
	if len(names) != 1 || names[0] != "local:small" || !strings.Contains(message, "disk") {
		t.Fatal(names, message)
	}
}

func TestInvalidProviderResponses(t *testing.T) {
	for _, body := range []string{"not json", `{"choices":[]}`, `{"choices":[{"message":{"content":""}}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		_, err := coach(context.Background(), fixture(t), settings{Provider: "api", Endpoint: server.URL, Model: "test"})
		server.Close()
		if err == nil {
			t.Fatal("accepted empty/invalid model response")
		}
	}
}

func TestRealSparringEndpoints(t *testing.T) {
	path := os.Getenv("STOCKLLAMA_TEST_ENGINE")
	if path == "" {
		t.Skip("set STOCKLLAMA_TEST_ENGINE")
	}
	a := newApp(t.TempDir())
	a.current = fixture(t)
	a.settings.Engine = path
	code, data := request(t, a, "POST", "/api/play", map[string]any{"reset": true, "color": "black"})
	if code != 200 || !strings.Contains(string(data), "learned") {
		t.Fatal(code, string(data))
	}
	code, data = request(t, a, "POST", "/api/play", map[string]any{"move": "e5"})
	if code != 200 || len(a.game.Moves()) != 3 {
		t.Fatal(code, string(data))
	}
	a.current.Moves = map[string]map[string]int{}
	code, data = request(t, a, "POST", "/api/play", map[string]any{"reset": true, "color": "black"})
	if code != 200 || !strings.Contains(string(data), "Stockfish fallback") {
		t.Fatal(code, string(data))
	}
}
