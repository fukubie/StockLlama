package main

import (
	"container/heap"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/notnil/chess"
)

type puzzle struct {
	FEN    string `json:"fen"`
	Best   string `json:"best_move"`
	Loss   int    `json:"loss_cp"`
	Played string `json:"played"`
}
type profile struct {
	Version  int                       `json:"version"`
	Username string                    `json:"username"`
	Games    int                       `json:"games"`
	Skipped  int                       `json:"skipped"`
	Moves    map[string]map[string]int `json:"moves"`
	Openings map[string]int            `json:"openings"`
	Record   map[string]int            `json:"record"`
	Puzzles  []puzzle                  `json:"puzzles"`
	Analysed int                       `json:"analysed_moves"`
	Loss     int                       `json:"loss_cp_total"`
}
type rawGame struct {
	PGN   string `json:"pgn"`
	Rules string `json:"rules"`
	End   int64  `json:"end_time"`
}

type recentHeap []rawGame

func (h recentHeap) Len() int           { return len(h) }
func (h recentHeap) Less(i, j int) bool { return h[i].End < h[j].End }
func (h recentHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *recentHeap) Push(v any)        { *h = append(*h, v.(rawGame)) }
func (h *recentHeap) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,25}$`)

func key(pos *chess.Position) string { return strings.Join(strings.Fields(pos.String())[:4], " ") }
func legalMove(pos *chess.Position, text string) (*chess.Move, error) {
	for _, m := range pos.ValidMoves() {
		if m.String() == text || (chess.AlgebraicNotation{}).Encode(pos, m) == text {
			return m, nil
		}
	}
	return nil, fmt.Errorf("Illegal move. Select a highlighted square or enter SAN / UCI")
}
func gameAt(fen string) (*chess.Game, error) {
	option, err := chess.FEN(fen)
	if err != nil {
		return nil, err
	}
	return chess.NewGame(option), nil
}

var errRequest = errors.New("request failed")

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("server returned %d", int(e)) }

// getJSON returns source-neutral errors; callers add wording for the site they called.
func getJSON(ctx context.Context, client *http.Client, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "StockLlama/0.2 (open-source local chess preparation)")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", errRequest, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return statusError(res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(target)
}
func chessComError(err error) error {
	var status statusError
	if errors.As(err, &status) {
		return fmt.Errorf("Chess.com returned %d; check username or try again later", int(status))
	}
	if errors.Is(err, errRequest) {
		return fmt.Errorf("cannot reach Chess.com; check internet or import a file")
	}
	return err
}
func fetchGames(ctx context.Context, client *http.Client, base, username string, limit int) ([]rawGame, error) {
	if !usernameRE.MatchString(username) || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("valid username and 1–200 games required")
	}
	base = strings.TrimRight(base, "/") + "/" + strings.ToLower(username) + "/games"
	var archives struct {
		Archives []string `json:"archives"`
	}
	if err := getJSON(ctx, client, base+"/archives", &archives); err != nil {
		return nil, chessComError(err)
	}
	result := []rawGame{}
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `/\d{4}/\d{2}$`)
	for i := len(archives.Archives) - 1; i >= max(0, len(archives.Archives)-3); i-- {
		url := archives.Archives[i]
		if !pattern.MatchString(url) {
			continue
		}
		var month struct {
			Games []rawGame `json:"games"`
		}
		if err := getJSON(ctx, client, url, &month); err != nil {
			return nil, chessComError(err)
		}
		recent := &recentHeap{}
		capacity := limit - len(result)
		for _, g := range month.Games {
			if (g.Rules == "" || g.Rules == "chess") && g.PGN != "" {
				if recent.Len() < capacity {
					heap.Push(recent, g)
				} else if g.End > (*recent)[0].End {
					heap.Pop(recent)
					heap.Push(recent, g)
				}
			}
		}
		sort.SliceStable(*recent, func(i, j int) bool { return (*recent)[i].End > (*recent)[j].End })
		result = append(result, (*recent)...)
		if len(result) == limit {
			return result, nil
		}
	}
	return result, nil
}
func parsePGN(text string) ([]rawGame, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	// Split only at a new header section after movetext; comments and variations stay with their game.
	starts := pgnTagPair.FindAllStringIndex(text, -1)
	offsets := []int{0}
	previous := 0
	for _, s := range starts {
		if hasMovetext(text[previous:s[0]]) {
			offsets = append(offsets, s[0])
		}
		previous = s[0]
	}
	offsets = append(offsets, len(text))
	games := []rawGame{}
	for i := 0; i+1 < len(offsets); i++ {
		part := strings.TrimSpace(text[offsets[i]:offsets[i+1]])
		if part != "" {
			option, e := chess.PGN(strings.NewReader(part))
			if e != nil {
				return nil, fmt.Errorf("invalid PGN: %w", e)
			}
			if len(chess.NewGame(option).Moves()) == 0 {
				return nil, fmt.Errorf("PGN game contains no valid moves")
			}
			games = append(games, rawGame{PGN: part})
		}
	}
	if len(games) == 0 {
		return nil, fmt.Errorf("file contains no PGN games")
	}
	return games, nil
}

// Only real tag-pair lines such as [Event "x"] count as headers, so a wrapped
// comment line that starts with an annotation like [%clk 0:09:58] stays movetext.
var pgnTagPair = regexp.MustCompile(`(?m)^\s*\[[A-Za-z0-9_]+\s*"`)

// hasMovetext reports whether s contains a non-blank line that is not a header.
func hasMovetext(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !pgnTagPair.MatchString(line) {
			return true
		}
	}
	return false
}
func importData(name string, data []byte) ([]rawGame, *profile, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(name), ".pgn"):
		g, e := parsePGN(string(data))
		return g, nil, e
	case strings.HasSuffix(strings.ToLower(name), ".csv"):
		rows, e := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff"))).ReadAll()
		if e != nil || len(rows) < 2 {
			return nil, nil, fmt.Errorf("CSV needs a header and at least one row")
		}
		col := -1
		for i, h := range rows[0] {
			if strings.EqualFold(strings.TrimSpace(h), "pgn") {
				col = i
			}
		}
		if col < 0 {
			return nil, nil, fmt.Errorf("CSV needs a pgn column containing full PGN text")
		}
		games := []rawGame{}
		for _, r := range rows[1:] {
			games = append(games, rawGame{PGN: r[col]})
		}
		return games, nil, nil
	case strings.HasSuffix(strings.ToLower(name), ".json"):
		var p profile
		if json.Unmarshal(data, &p) == nil && p.Version != 0 {
			if e := validateProfile(&p); e != nil {
				return nil, nil, e
			}
			return nil, &p, nil
		}
		var games []rawGame
		if e := json.Unmarshal(data, &games); e == nil && len(games) > 0 {
			return games, nil, nil
		}
		var wrapped struct {
			Games []rawGame `json:"games"`
		}
		if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Games) > 0 {
			return wrapped.Games, nil, nil
		}
		return nil, nil, fmt.Errorf("JSON must be an exported profile, a games array, or {games: [{pgn: ...}]}")
	}
	return nil, nil, fmt.Errorf("choose a PGN, CSV, or JSON file")
}
func validateProfile(p *profile) error {
	if p.Version != 1 || !usernameRE.MatchString(p.Username) || p.Games < 1 || p.Moves == nil || p.Openings == nil || p.Record == nil || p.Analysed < 0 || p.Loss < 0 {
		return fmt.Errorf("invalid or unsupported player profile")
	}
	for fen, counts := range p.Moves {
		g, e := gameAt(fen + " 0 1")
		if e != nil {
			return fmt.Errorf("invalid profile position")
		}
		for u, n := range counts {
			if _, e = legalMove(g.Position(), u); e != nil || n < 1 {
				return fmt.Errorf("invalid learned move or frequency")
			}
		}
	}
	for _, q := range p.Puzzles {
		g, e := gameAt(q.FEN)
		if e != nil {
			return fmt.Errorf("invalid drill position")
		}
		if _, e = legalMove(g.Position(), q.Best); e != nil {
			return fmt.Errorf("invalid drill solution")
		}
	}
	return nil
}
func buildProfile(games []rawGame, username string, e analyser, progress func(int, int)) (*profile, error) {
	if !usernameRE.MatchString(username) {
		return nil, fmt.Errorf("enter a valid target username")
	}
	p := &profile{Version: 1, Username: username, Moves: map[string]map[string]int{}, Openings: map[string]int{}, Record: map[string]int{}, Puzzles: []puzzle{}}
	parsed := make([]*chess.Game, len(games))
	total := 0
	for i, raw := range games {
		if raw.Rules != "" && raw.Rules != "chess" {
			continue
		}
		option, err := chess.PGN(strings.NewReader(raw.PGN))
		if err != nil {
			return nil, fmt.Errorf("game %d has invalid PGN", i+1)
		}
		g := chess.NewGame(option)
		parsed[i] = g
		target := chess.NoColor
		for _, tag := range g.TagPairs() {
			if strings.EqualFold(tag.Value, username) {
				if tag.Key == "White" {
					target = chess.White
				}
				if tag.Key == "Black" {
					target = chess.Black
				}
			}
		}
		n := 0
		for _, pos := range g.Positions()[:len(g.Moves())] {
			if pos.Turn() == target {
				n++
			}
		}
		total += min(24, n)
	}
	done := 0
	if progress != nil {
		progress(0, total)
	}
	cache := map[string]evaluation{}
	analyse := func(pos *chess.Position) (evaluation, error) {
		fen := pos.String()
		if value, ok := cache[fen]; ok {
			return value, nil
		}
		value, err := e.Analyse(fen, 10)
		if err == nil && len(cache) < 4096 {
			cache[fen] = value
		}
		return value, err
	}
	for i, raw := range games {
		if raw.Rules != "" && raw.Rules != "chess" {
			p.Skipped++
			continue
		}
		game := parsed[i]
		tags := map[string]string{}
		for _, t := range game.TagPairs() {
			tags[t.Key] = t.Value
		}
		if tags["Variant"] != "" && tags["Variant"] != "Standard" {
			p.Skipped++
			continue
		}
		color := chess.NoColor
		if strings.EqualFold(tags["White"], username) {
			color = chess.White
		} else if strings.EqualFold(tags["Black"], username) {
			color = chess.Black
		}
		if color == chess.NoColor || len(game.Moves()) == 0 {
			p.Skipped++
			continue
		}
		p.Games++
		opening := tags["Opening"]
		if opening == "" {
			opening = tags["ECO"]
		}
		if opening == "" {
			opening = "Unclassified"
		}
		p.Openings[opening]++
		outcome := "unknown"
		switch tags["Result"] {
		case "1/2-1/2":
			outcome = "draw"
		case "1-0", "0-1":
			outcome = "loss"
			if (tags["Result"] == "1-0") == (color == chess.White) {
				outcome = "win"
			}
		}
		p.Record[strings.ToLower(color.Name())+"_"+outcome]++
		positions := game.Positions()
		analysed := 0
		for j, move := range game.Moves() {
			before, after := positions[j], positions[j+1]
			if before.Turn() != color {
				continue
			}
			k := key(before)
			if p.Moves[k] == nil {
				p.Moves[k] = map[string]int{}
			}
			p.Moves[k][move.String()]++
			if e == nil || analysed >= 24 {
				continue
			}
			analysed++
			a, err := analyse(before)
			if err != nil {
				return nil, err
			}
			var b evaluation
			if len(after.ValidMoves()) == 0 {
				if after.Status() == chess.Checkmate {
					b.CP = -10000
				}
			} else {
				b, err = analyse(after)
				if err != nil {
					return nil, err
				}
			}
			loss := max(0, a.CP+b.CP) // Each UCI score is relative to the side to move; turns changed.
			p.Analysed++
			p.Loss += loss
			if loss >= 120 && b.Move != "" && len(p.Puzzles) < 64 {
				p.Puzzles = append(p.Puzzles, puzzle{after.String(), b.Move, loss, move.String()})
			}
			done++
			if progress != nil {
				progress(done, total)
			}
		}
	}
	if p.Games == 0 {
		return nil, fmt.Errorf("no standard games found for %s; match the PGN White or Black name", username)
	}
	if progress != nil {
		progress(total, total)
	}
	return p, nil
}
func chooseMove(pos *chess.Position, p *profile, e analyser) (*chess.Move, string, error) {
	counts := p.Moves[key(pos)]
	total := 0
	for _, m := range pos.ValidMoves() {
		total += counts[m.String()]
	}
	if total > 0 {
		n := rand.IntN(total)
		for _, m := range pos.ValidMoves() {
			n -= counts[m.String()]
			if n < 0 {
				return m, "learned", nil
			}
		}
	}
	v, err := e.Analyse(pos.String(), 80)
	if err != nil {
		return nil, "", err
	}
	m, err := legalMove(pos, v.Move)
	return m, "Stockfish fallback", err
}
func report(p *profile) string {
	names := make([]string, 0, len(p.Openings))
	for s := range p.Openings {
		names = append(names, s)
	}
	sort.Slice(names, func(i, j int) bool {
		if p.Openings[names[i]] == p.Openings[names[j]] {
			return names[i] < names[j]
		}
		return p.Openings[names[i]] > p.Openings[names[j]]
	})
	s := fmt.Sprintf("%s has %d studied games and %d saved mistakes\n", p.Username, p.Games, len(p.Puzzles))
	if len(names) > 0 {
		s += fmt.Sprintf("Prepare %s seen in %d games\n", names[0], p.Openings[names[0]])
	}
	return s + "Practice the saved replies before sparring"
}

var apiClient = &http.Client{Timeout: 40 * time.Second}
