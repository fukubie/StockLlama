package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/notnil/chess"
)

const demo = `[Event "Synthetic"]
[White "Target"]
[Black "Other"]
[Result "1-0"]
[Opening "King's Pawn Game"]

1. e4 e5 2. Qh5 Nc6 3. Bc4 Nf6 4. Qxf7# 1-0`

func fixture(t *testing.T) *profile {
	t.Helper()
	p, e := buildProfile([]rawGame{{PGN: demo}}, "Target", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

type fakeEngine struct {
	calls  int
	values []evaluation
	err    error
}

func (e *fakeEngine) Analyse(string, int) (evaluation, error) {
	e.calls++
	if e.err != nil {
		return evaluation{}, e.err
	}
	if len(e.values) == 0 {
		return evaluation{CP: 0, Move: "e2e4"}, nil
	}
	v := e.values[0]
	e.values = e.values[1:]
	return v, nil
}
func TestTargetModel(t *testing.T) {
	p := fixture(t)
	start := chess.StartingPosition()
	if p.Games != 1 || p.Record["white_win"] != 1 || p.Moves[key(start)]["e2e4"] != 1 {
		t.Fatalf("wrong profile: %+v", p)
	}
	m, _ := legalMove(start, "e4")
	if len(p.Moves[key(start.Update(m))]) != 0 {
		t.Fatal("learned other player's moves")
	}
	if err := validateProfile(p); err != nil {
		t.Fatal(err)
	}
}
func TestColorsAndResults(t *testing.T) {
	for _, tc := range []struct{ name, user, result, want string }{{"white win", "target", "1-0", "white_win"}, {"white loss", "Target", "0-1", "white_loss"}, {"white draw", "Target", "1/2-1/2", "white_draw"}, {"black win", "OTHER", "0-1", "black_win"}, {"black loss", "Other", "1-0", "black_loss"}, {"black draw", "Other", "1/2-1/2", "black_draw"}, {"unknown", "Other", "*", "black_unknown"}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.ReplaceAll(demo, "1-0", tc.result)
			p, e := buildProfile([]rawGame{{PGN: raw}}, tc.user, nil, nil)
			if e != nil || p.Record[tc.want] != 1 {
				t.Fatalf("%+v %v", p, e)
			}
		})
	}
}
func TestLegalMoves(t *testing.T) {
	for _, tc := range []struct {
		input string
		valid bool
	}{{"e4", true}, {"e2e4", true}, {"Nf3", true}, {"e5", false}, {"e2e5", false}, {"x", false}, {"e2e4q", false}} {
		t.Run(tc.input, func(t *testing.T) {
			_, e := legalMove(chess.StartingPosition(), tc.input)
			if (e == nil) != tc.valid {
				t.Fatal(e)
			}
		})
	}
}
func TestSpecialMoves(t *testing.T) {
	for _, tc := range []struct{ fen, move string }{{"r3k2r/8/8/8/8/8/8/R3K2R w KQkq - 0 1", "O-O"}, {"4k3/P7/8/8/8/8/8/4K3 w - - 0 1", "a7a8q"}, {"4k3/8/8/3pP3/8/8/8/4K3 w - d6 0 1", "e5d6"}} {
		t.Run(tc.move, func(t *testing.T) {
			g, e := gameAt(tc.fen)
			if e != nil {
				t.Fatal(e)
			}
			m, e := legalMove(g.Position(), tc.move)
			if e != nil {
				t.Fatal(e)
			}
			if e = g.Move(m); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestLearnedMoveAndFallback(t *testing.T) {
	p := fixture(t)
	e := &fakeEngine{}
	m, source, err := chooseMove(chess.StartingPosition(), p, e)
	if err != nil || m.String() != "e2e4" || source != "learned" || e.calls != 0 {
		t.Fatal(m, source, err)
	}
	p.Moves = map[string]map[string]int{}
	m, source, err = chooseMove(chess.StartingPosition(), p, e)
	if err != nil || source != "Stockfish fallback" || m.String() != "e2e4" || e.calls != 1 {
		t.Fatal(m, source, err)
	}
}
func TestScorePerspective(t *testing.T) {
	for _, target := range []string{"Target", "Other"} {
		t.Run(target, func(t *testing.T) {
			raw := strings.Split(demo, "1. e4")[0] + "1. e4 e5 1-0"
			e := &fakeEngine{values: []evaluation{{CP: 20, Move: "e2e4"}, {CP: 200, Move: "g1f3"}}}
			p, err := buildProfile([]rawGame{{PGN: raw}}, target, e, nil)
			if err != nil || len(p.Puzzles) != 1 || p.Puzzles[0].Loss != 220 {
				t.Fatalf("%+v %v", p, err)
			}
			turn := strings.Fields(p.Puzzles[0].FEN)[1]
			if (target == "Target" && turn != "b") || (target == "Other" && turn != "w") {
				t.Fatal("wrong drill side")
			}
		})
	}
}
func TestNoFalseBlunderOnImprovement(t *testing.T) {
	raw := strings.Split(demo, "1. e4")[0] + "1. e4 e5 1-0"
	e := &fakeEngine{values: []evaluation{{CP: 20}, {CP: -50}}}
	p, err := buildProfile([]rawGame{{PGN: raw}}, "Target", e, nil)
	if err != nil || p.Loss != 0 || len(p.Puzzles) != 0 {
		t.Fatal(p, err)
	}
}
func TestBadTrainingInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  rawGame
		user string
	}{{"bad username", rawGame{PGN: demo}, "../bad"}, {"wrong player", rawGame{PGN: demo}, "Nobody"}, {"variant", rawGame{PGN: demo, Rules: "chess960"}, "Target"}, {"invalid PGN", rawGame{PGN: strings.Replace(demo, "e4", "e9", 1)}, "Target"}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildProfile([]rawGame{tc.raw}, tc.user, nil, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
func TestPGNMultiple(t *testing.T) {
	g, err := parsePGN(demo + "\n\n" + demo)
	if err != nil || len(g) != 2 {
		t.Fatal(len(g), err)
	}
}
func TestPGNHeadersContainingOneDot(t *testing.T) {
	dated := `[Event "Round 1. Open"]
[Date "2024.01.15"]
[White "Target"]
[Black "Other"]
[Result "1-0"]

1. e4 e5 2. Qh5 Nc6 3. Bc4 Nf6 4. Qxf7# 1-0`
	for _, tc := range []struct {
		text string
		want int
	}{{dated, 1}, {dated + "\n\n" + dated, 2}} {
		g, err := parsePGN(tc.text)
		if err != nil || len(g) != tc.want {
			t.Fatalf("got %d games, want %d: %v", len(g), tc.want, err)
		}
	}
}

func TestPGNWrappedCommentStartingWithAnnotation(t *testing.T) {
	game := `[Event "Casual"]
[White "Target"]
[Black "Other"]
[Result "*"]

1. e4 { this is a long comment that wraps
[%clk 0:09:58] } e5 2. Nf3 *`
	for _, tc := range []struct {
		text string
		want int
	}{
		{game, 1},
		{game + "\n\n" + game, 2},
		// PGN does not require a space between the tag name and its value.
		{"[Event\"A\"]\n[Result \"*\"]\n1. e4 e5 *\n[Event\"B\"]\n1. d4 d5 *", 2},
	} {
		g, err := parsePGN(tc.text)
		if err != nil || len(g) != tc.want {
			t.Fatalf("got %d games, want %d: %v", len(g), tc.want, err)
		}
	}
}
func TestImports(t *testing.T) {
	games, _ := json.Marshal([]rawGame{{PGN: demo}})
	wrapped, _ := json.Marshal(map[string]any{"games": []rawGame{{PGN: demo}}})
	saved, _ := json.Marshal(fixture(t))
	csv := "pgn\n\"" + strings.ReplaceAll(demo, "\"", "\"\"") + "\"\n"
	for _, tc := range []struct {
		name, content string
		isProfile     bool
	}{{"sample.pgn", demo, false}, {"sample.csv", csv, false}, {"games.json", string(games), false}, {"archive.json", string(wrapped), false}, {"profile.json", string(saved), true}} {
		t.Run(tc.name, func(t *testing.T) {
			g, p, e := importData(tc.name, []byte(tc.content))
			if e != nil {
				t.Fatal(e)
			}
			if tc.isProfile {
				if p == nil {
					t.Fatal("missing profile")
				}
			} else if len(g) != 1 {
				t.Fatal("missing game")
			}
		})
	}
}
func TestBadImports(t *testing.T) {
	for _, tc := range []struct{ name, content string }{{"a.txt", "x"}, {"a.csv", "name\nx"}, {"a.csv", "pgn"}, {"a.csv", "pgn,x\na"}, {"a.json", "null"}, {"a.json", "{}"}, {"a.json", "{"}, {"a.pgn", "1. z9"}} {
		t.Run(tc.name+tc.content, func(t *testing.T) {
			if _, _, e := importData(tc.name, []byte(tc.content)); e == nil {
				t.Fatal("accepted invalid file")
			}
		})
	}
}
func TestProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*profile)
	}{{"version", func(p *profile) { p.Version = 2 }}, {"path traversal", func(p *profile) { p.Username = "../bad" }}, {"negative count", func(p *profile) { p.Moves[key(chess.StartingPosition())]["e2e4"] = -1 }}, {"illegal move", func(p *profile) { p.Moves[key(chess.StartingPosition())]["e2e5"] = 1 }}, {"bad FEN", func(p *profile) { p.Moves["bad"] = map[string]int{} }}, {"bad drill", func(p *profile) { p.Puzzles = []puzzle{{FEN: "bad", Best: "e2e4"}} }}} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			tc.edit(p)
			if validateProfile(p) == nil {
				t.Fatal("accepted invalid profile")
			}
		})
	}
}
func TestFetchAcrossMonths(t *testing.T) {
	var server *httptest.Server
	calls := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/target/games/archives":
			fmt.Fprintf(w, `{"archives":[%q,%q,"https://evil.invalid/2026/01"]}`, server.URL+"/target/games/2026/01", server.URL+"/target/games/2026/02")
		case "/target/games/2026/02":
			fmt.Fprint(w, `{"games":[{"pgn":"skip","rules":"chess960"},{"pgn":"recent","end_time":2}]}`)
		default:
			fmt.Fprint(w, `{"games":[{"pgn":"old","end_time":1}]}`)
		}
	}))
	defer server.Close()
	g, e := fetchGames(context.Background(), server.Client(), server.URL, "TARGET", 2)
	if e != nil || len(g) != 2 || g[0].PGN != "recent" || g[1].PGN != "old" || calls != 3 {
		t.Fatal(g, calls, e)
	}
}
func TestFetchErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer s.Close()
	for _, tc := range []struct {
		user string
		n    int
	}{{"bad/user", 1}, {"Target", 0}, {"Target", 201}, {"Target", 2}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			if _, e := fetchGames(context.Background(), s.Client(), s.URL, tc.user, tc.n); e == nil {
				t.Fatal("expected error")
			}
		})
	}
}
func TestRealStockfish(t *testing.T) {
	path := os.Getenv("STOCKLLAMA_TEST_ENGINE")
	if path == "" {
		t.Skip("set STOCKLLAMA_TEST_ENGINE for real-engine integration")
	}
	e, err := newEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := buildProfile([]rawGame{{PGN: demo}}, "Other", e, nil)
	if err != nil || p.Analysed != 3 || len(p.Puzzles) == 0 {
		t.Fatal(p, err)
	}
	p.Moves = map[string]map[string]int{}
	m, source, err := chooseMove(chess.StartingPosition(), p, e)
	if err != nil || m == nil || source != "Stockfish fallback" {
		t.Fatal(m, source, err)
	}
}
func TestFetchErrorsNameChessCom(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	_, e := fetchGames(context.Background(), s.Client(), s.URL, "target", 1)
	if e == nil || e.Error() != "Chess.com returned 403; check username or try again later" {
		t.Fatal(e)
	}
	s.Close()
	_, e = fetchGames(context.Background(), s.Client(), s.URL, "target", 1)
	if e == nil || e.Error() != "cannot reach Chess.com; check internet or import a file" {
		t.Fatal(e)
	}
}
