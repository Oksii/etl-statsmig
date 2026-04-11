package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// ── Types ──────────────────────────────────────────────────────────────────────

// RoundScores is the metadata.scores payload written into each round.
type RoundScores struct {
	Alpha         int         `json:"alpha"`
	Beta          int         `json:"beta"`
	CompletedMaps int         `json:"completed_maps"`
	MatchFinished bool        `json:"match_finished"`
	MatchWinner   string      `json:"match_winner,omitempty"`
	AlphaTeamname string      `json:"alpha_teamname,omitempty"`
	BetaTeamname  string      `json:"beta_teamname,omitempty"`
	Round         roundDetail `json:"round"`
}

type roundDetail struct {
	MapNum    int    `json:"map_num"`
	RoundNum  int    `json:"round_num"`
	Winner    string `json:"winner"`
	WinnerET  int    `json:"winner_et"`
	AlphaSide int    `json:"alpha_side"`
	Fullhold  bool   `json:"fullhold"`
}

// scoreRoundInput holds the per-round data needed for scoring.
type scoreRoundInput struct {
	RoundNum   int
	Winnerteam int
	Timelimit  string
	NextTL     string
}

type fullholdEntry struct {
	r1Fullhold  bool
	alphaWonR1  bool
	hasR1Data   bool
	provisional bool
}

// ── Core algorithm ─────────────────────────────────────────────────────────────

// alphaExpectedSide returns 1 (axis) or 2 (allies).
// Port of scores.lua: alpha is axis when (mapNum+roundNum) is even.
// map1r1=axis, map1r2=allies, map2r1=allies, map2r2=axis, map3r1=axis, map3r2=allies
func alphaExpectedSide(mapNum, roundNum int) int {
	if (mapNum+roundNum)%2 == 0 {
		return 1 // axis
	}
	return 2 // allies
}

// computeMatchScores replays ET stopwatch scoring across all rounds in order.
// Returns one RoundScores per round reflecting cumulative state after that round.
// ngMode mirrors scores.lua ng_mode: match termination is suppressed (scores accumulate indefinitely).
// alphaTN/betaTN are passed through into each RoundScores for storage only.
func computeMatchScores(rounds []scoreRoundInput, alphaTN, betaTN string, ngMode bool) []RoundScores {
	if len(rounds) == 0 {
		return nil
	}

	alphaScore := 0
	betaScore := 0
	matchFinished := false
	matchWinner := ""
	processed := 0
	fullholdBuf := map[int]*fullholdEntry{}
	prevFileRoundNum := 0
	prevFullhold := false

	results := make([]RoundScores, len(rounds))

	for i, r := range rounds {
		mapNum := processed/2 + 1
		fullhold := r.Timelimit == r.NextTL

		// Detect "fullhold r2 recorded as r1" — a bug in the old Lua module where the
		// second half of a fullhold map was stored with round_info.round=1 instead of 2.
		// When two consecutive file-round=1 entries appear and the first was a fullhold,
		// the second is actually r2 (sides swapped). Use effectiveRoundNum=2 for alpha_side
		// computation only; scoring still uses r.RoundNum to replicate the Lua's behavior
		// and match cumulative scores already stored in upgraded reference files.
		effectiveRoundNum := r.RoundNum
		if r.RoundNum == 1 && prevFileRoundNum == 1 && prevFullhold {
			effectiveRoundNum = 2
		}

		alphaSide := alphaExpectedSide(mapNum, effectiveRoundNum)
		alphaWon := r.Winnerteam == alphaSide
		winner := "beta"
		if alphaWon {
			winner = "alpha"
		}

		if r.RoundNum == 1 {
			fb := &fullholdEntry{
				r1Fullhold: fullhold,
				alphaWonR1: alphaWon,
				hasR1Data:  true,
			}
			fullholdBuf[mapNum] = fb

			if fullhold {
				if alphaWon {
					alphaScore++
				} else {
					betaScore++
				}
				fb.provisional = true

				// Clinch: only at exactly 3-0; suppressed in ng mode
				if !ngMode && ((alphaScore == 3 && betaScore == 0) || (betaScore == 3 && alphaScore == 0)) {
					matchFinished = true
					matchWinner = "alpha"
					if betaScore > alphaScore {
						matchWinner = "beta"
					}
				}
			}
		} else {
			fb := fullholdBuf[mapNum]
			if fb == nil {
				fb = &fullholdEntry{}
			}

			// Remove R1 provisional if present
			if fb.provisional {
				if fb.alphaWonR1 {
					alphaScore--
				} else {
					betaScore--
				}
				fb.provisional = false
			}

			if fb.r1Fullhold && fullhold {
				// Double fullhold: +1 each
				alphaScore++
				betaScore++
			} else {
				// Normal or no-R1-data fallback: +2 to R2 winner
				if alphaWon {
					alphaScore += 2
				} else {
					betaScore += 2
				}
			}

			// Termination mirrors scores.lua (suppressed in ng mode):
			//   completed_maps >= 2 and score >= 3 → early finish (4-0, 3-1, etc.)
			//   completed_maps >= 3                → always finish after map 3
			if !matchFinished && !ngMode {
				if mapNum >= 2 && (alphaScore >= 3 || betaScore >= 3) {
					matchFinished = true
				} else if mapNum >= 3 {
					matchFinished = true
				}
			}
			if matchFinished && matchWinner == "" {
				switch {
				case alphaScore > betaScore:
					matchWinner = "alpha"
				case betaScore > alphaScore:
					matchWinner = "beta"
				default:
					matchWinner = "draw"
				}
			}
		}

		prevFileRoundNum = r.RoundNum
		prevFullhold = fullhold
		processed++
		completedMaps := processed / 2

		results[i] = RoundScores{
			Alpha:         alphaScore,
			Beta:          betaScore,
			CompletedMaps: completedMaps,
			MatchFinished: matchFinished,
			MatchWinner:   matchWinner,
			AlphaTeamname: alphaTN,
			BetaTeamname:  betaTN,
			Round: roundDetail{
				MapNum:    mapNum,
				RoundNum:  r.RoundNum,
				Winner:    winner,
				WinnerET:  r.Winnerteam,
				AlphaSide: alphaSide,
				Fullhold:  fullhold,
			},
		}
	}

	return results
}

// ── Helpers shared by upgrade, rescore, scorecheck ────────────────────────────

// extractTeamNames reads alpha_teamname / beta_teamname from the match-level map.
func extractTeamNames(matchMap map[string]json.RawMessage) (alphaTN, betaTN string) {
	if v, ok := matchMap["alpha_teamname"]; ok {
		json.Unmarshal(v, &alphaTN) //nolint: errcheck
	}
	if v, ok := matchMap["beta_teamname"]; ok {
		json.Unmarshal(v, &betaTN) //nolint: errcheck
	}
	return
}

// collectScoreInputs reads round_info from each round and returns scoreRoundInputs.
func collectScoreInputs(rounds []map[string]json.RawMessage) []scoreRoundInput {
	inputs := make([]scoreRoundInput, 0, len(rounds))
	for _, round := range rounds {
		rdRaw, ok := round["round_data"]
		if !ok {
			continue
		}
		var rd map[string]json.RawMessage
		if err := json.Unmarshal(rdRaw, &rd); err != nil {
			continue
		}
		riRaw, ok := rd["round_info"]
		if !ok {
			continue
		}
		var ri struct {
			Round         int    `json:"round"`
			Winnerteam    int    `json:"winnerteam"`
			Timelimit     string `json:"timelimit"`
			NextTimeLimit string `json:"nextTimeLimit"`
		}
		if err := json.Unmarshal(riRaw, &ri); err != nil {
			continue
		}
		inputs = append(inputs, scoreRoundInput{
			RoundNum:   ri.Round,
			Winnerteam: ri.Winnerteam,
			Timelimit:  ri.Timelimit,
			NextTL:     ri.NextTimeLimit,
		})
	}
	return inputs
}

// injectRoundScores replaces the "scores" field inside rd["metadata"] with s.
func injectRoundScores(rd map[string]json.RawMessage, s RoundScores) error {
	metaRaw, ok := rd["metadata"]
	if !ok {
		return fmt.Errorf("no metadata key")
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	scoresBytes, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal scores: %w", err)
	}
	meta["scores"] = scoresBytes
	newMeta, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("re-marshal metadata: %w", err)
	}
	rd["metadata"] = newMeta
	return nil
}

// isNgModeMatch returns true when the match was run under ng_scores (no termination).
// Detected by state == "unknown match" in the match map — these are untracked ad-hoc games.
func isNgModeMatch(matchMap map[string]json.RawMessage) bool {
	v, ok := matchMap["state"]
	if !ok {
		return false
	}
	var state string
	if err := json.Unmarshal(v, &state); err != nil {
		return false
	}
	return state == "unknown match"
}

// applyScoresToFile parses the already-marshalled rounds, runs scoring, and
// injects scores back. Returns the updated rounds slice.
func applyScoresToFile(rounds []map[string]json.RawMessage, matchMap map[string]json.RawMessage) ([]map[string]json.RawMessage, string) {
	alphaTN, betaTN := extractTeamNames(matchMap)
	inputs := collectScoreInputs(rounds)
	scores := computeMatchScores(inputs, alphaTN, betaTN, isNgModeMatch(matchMap))

	si := 0
	for i, round := range rounds {
		if si >= len(scores) {
			break
		}
		rdRaw, ok := round["round_data"]
		if !ok {
			continue
		}
		var rd map[string]json.RawMessage
		if err := json.Unmarshal(rdRaw, &rd); err != nil {
			continue
		}
		if _, has := rd["metadata"]; !has {
			si++
			continue
		}
		if err := injectRoundScores(rd, scores[si]); err != nil {
			si++
			continue
		}
		rdBytes, err := json.Marshal(rd)
		if err != nil {
			si++
			continue
		}
		round["round_data"] = rdBytes
		rounds[i] = round
		si++
	}

	// Build mismatch warning if match-level scores are present
	warn := ""
	if len(scores) > 0 {
		final := scores[len(scores)-1]
		var storedAlpha, storedBeta int
		var storedWinner string
		hasStoredAlpha := false
		hasStoredBeta := false
		hasStoredWinner := false
		if v, ok := matchMap["alpha_score"]; ok {
			if json.Unmarshal(v, &storedAlpha) == nil {
				hasStoredAlpha = true
			}
		}
		if v, ok := matchMap["beta_score"]; ok {
			if json.Unmarshal(v, &storedBeta) == nil {
				hasStoredBeta = true
			}
		}
		if v, ok := matchMap["winner"]; ok {
			if json.Unmarshal(v, &storedWinner) == nil {
				hasStoredWinner = true
			}
		}
		if hasStoredAlpha && hasStoredBeta && (final.Alpha != storedAlpha || final.Beta != storedBeta) {
			warn = fmt.Sprintf("scores mismatch: computed %d-%d vs stored %d-%d",
				final.Alpha, final.Beta, storedAlpha, storedBeta)
		}
		if hasStoredWinner && storedWinner != "" && final.MatchWinner != storedWinner {
			if warn != "" {
				warn += fmt.Sprintf("; winner mismatch: computed %q vs stored %q", final.MatchWinner, storedWinner)
			} else {
				warn = fmt.Sprintf("winner mismatch: computed %q vs stored %q", final.MatchWinner, storedWinner)
			}
		}

		// Overwrite match-level fields with authoritative computed values.
		if ab, err := json.Marshal(final.Alpha); err == nil {
			matchMap["alpha_score"] = ab
		}
		if bb, err := json.Marshal(final.Beta); err == nil {
			matchMap["beta_score"] = bb
		}
		if wb, err := json.Marshal(final.MatchWinner); err == nil {
			matchMap["winner"] = wb
		}
	}

	return rounds, warn
}

// ── rescore subcommand ─────────────────────────────────────────────────────────

func runRescoreCmd(args []string) {
	fs := flag.NewFlagSet("rescore", flag.ExitOnError)
	inDir := fs.String("in", "", "Directory with upgraded JSON files to rescore in-place (required)")
	workers := fs.Int("workers", runtime.NumCPU(), "Number of parallel worker goroutines")
	fs.Parse(args) //nolint: errcheck

	if *inDir == "" {
		fmt.Fprintln(os.Stderr, "Usage: etl-statsmig rescore --in <dir> [--workers N]")
		fs.PrintDefaults()
		os.Exit(1)
	}

	inRoot, err := filepath.Abs(*inDir)
	if err != nil {
		log.Fatalf("bad input path: %v", err)
	}

	jobs := make(chan string, *workers*8)

	var (
		wg        sync.WaitGroup
		processed uint64
		errCount  uint64
		warnCount uint64
	)

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				warn, err := rescoreFile(path)
				if err != nil {
					log.Printf("ERROR %s: %v", path, err)
					atomic.AddUint64(&errCount, 1)
				} else if warn != "" {
					log.Printf("WARN  %s: %s", path, warn)
					atomic.AddUint64(&warnCount, 1)
				}
				n := atomic.AddUint64(&processed, 1)
				if n%1000 == 0 {
					log.Printf("rescored %d files...", n)
				}
			}
		}()
	}

	filepath.WalkDir(inRoot, func(path string, d os.DirEntry, err error) error { //nolint: errcheck
		if err != nil {
			log.Printf("walk error at %s: %v", path, err)
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".json") {
			jobs <- path
		}
		return nil
	})

	close(jobs)
	wg.Wait()

	n := atomic.LoadUint64(&processed)
	e := atomic.LoadUint64(&errCount)
	w := atomic.LoadUint64(&warnCount)
	fmt.Printf("Done. Files: %d  Errors: %d  Warnings: %d\n", n, e, w)
	if e > 0 {
		os.Exit(1)
	}
}

func rescoreFile(path string) (warn string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("parse root: %w", err)
	}
	matchRaw, ok := root["match"]
	if !ok {
		return "", fmt.Errorf("no 'match' key")
	}
	var matchMap map[string]json.RawMessage
	if err := json.Unmarshal(matchRaw, &matchMap); err != nil {
		return "", fmt.Errorf("parse match: %w", err)
	}
	roundsRaw, ok := matchMap["rounds"]
	if !ok {
		return "", nil // no rounds, skip
	}
	var rounds []map[string]json.RawMessage
	if err := json.Unmarshal(roundsRaw, &rounds); err != nil {
		return "", fmt.Errorf("parse rounds: %w", err)
	}
	if len(rounds) == 0 {
		return "", nil
	}

	rounds, warn = applyScoresToFile(rounds, matchMap)

	roundsBytes, err := json.Marshal(rounds)
	if err != nil {
		return warn, fmt.Errorf("marshal rounds: %w", err)
	}
	matchMap["rounds"] = roundsBytes

	matchBytes, err := json.Marshal(matchMap)
	if err != nil {
		return warn, fmt.Errorf("marshal match: %w", err)
	}
	root["match"] = matchBytes

	outBytes, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return warn, fmt.Errorf("marshal output: %w", err)
	}

	if err := os.WriteFile(path, outBytes, 0o644); err != nil {
		return warn, fmt.Errorf("write: %w", err)
	}
	// mtime intentionally not updated on rescore
	return warn, nil
}

// ── scorecheck subcommand ──────────────────────────────────────────────────────

func runScorecheckCmd(args []string) {
	fs := flag.NewFlagSet("scorecheck", flag.ExitOnError)
	refDir := fs.String("ref", "", "Directory with reference new-format JSON files (required)")
	fs.Parse(args) //nolint: errcheck

	if *refDir == "" {
		fmt.Fprintln(os.Stderr, "Usage: etl-statsmig scorecheck --ref <dir>")
		fs.PrintDefaults()
		os.Exit(1)
	}

	var totalRounds, passRounds, failRounds int
	var totalFiles, passFiles, failFiles int

	filepath.WalkDir(*refDir, func(path string, d os.DirEntry, err error) error { //nolint: errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}

		totalFiles++
		passed, failed, failLines, ferr := scorecheckFile(path)
		if ferr != nil {
			fmt.Printf("[FAIL] %s: %v\n", filepath.Base(path), ferr)
			failFiles++
			return nil
		}

		totalRounds += passed + failed
		passRounds += passed
		failRounds += failed

		if failed == 0 {
			fmt.Printf("[PASS] %s (%d rounds)\n", filepath.Base(path), passed)
			passFiles++
		} else {
			fmt.Printf("[FAIL] %s (%d pass, %d fail)\n", filepath.Base(path), passed, failed)
			for _, line := range failLines {
				fmt.Println(line)
			}
			failFiles++
		}
		return nil
	})

	fmt.Println()
	fmt.Println(strings.Repeat("═", 45))
	fmt.Printf("  Files  : %d total  %d pass  %d fail\n", totalFiles, passFiles, failFiles)
	fmt.Printf("  Rounds : %d total  %d pass  %d fail\n", totalRounds, passRounds, failRounds)
	fmt.Println(strings.Repeat("═", 45))

	if failRounds > 0 || failFiles > 0 {
		os.Exit(1)
	}
}

func scorecheckFile(path string) (passed, failed int, failLines []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read: %w", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, 0, nil, fmt.Errorf("parse root: %w", err)
	}
	matchRaw, ok := root["match"]
	if !ok {
		return 0, 0, nil, fmt.Errorf("no 'match' key")
	}
	var matchMap map[string]json.RawMessage
	if err := json.Unmarshal(matchRaw, &matchMap); err != nil {
		return 0, 0, nil, fmt.Errorf("parse match: %w", err)
	}
	roundsRaw, ok := matchMap["rounds"]
	if !ok {
		return 0, 0, nil, nil
	}
	var rounds []map[string]json.RawMessage
	if err := json.Unmarshal(roundsRaw, &rounds); err != nil {
		return 0, 0, nil, fmt.Errorf("parse rounds: %w", err)
	}

	// Collect inputs and existing stored scores side by side
	type refRound struct {
		input  scoreRoundInput
		stored *RoundScores
	}
	var refRounds []refRound

	for _, round := range rounds {
		rdRaw, ok := round["round_data"]
		if !ok {
			continue
		}
		var rd map[string]json.RawMessage
		if err := json.Unmarshal(rdRaw, &rd); err != nil {
			continue
		}
		riRaw, ok := rd["round_info"]
		if !ok {
			continue
		}
		var ri struct {
			Round         int    `json:"round"`
			Winnerteam    int    `json:"winnerteam"`
			Timelimit     string `json:"timelimit"`
			NextTimeLimit string `json:"nextTimeLimit"`
		}
		if err := json.Unmarshal(riRaw, &ri); err != nil {
			continue
		}

		inp := scoreRoundInput{
			RoundNum:   ri.Round,
			Winnerteam: ri.Winnerteam,
			Timelimit:  ri.Timelimit,
			NextTL:     ri.NextTimeLimit,
		}

		var stored *RoundScores
		if metaRaw, ok := rd["metadata"]; ok {
			var meta map[string]json.RawMessage
			if json.Unmarshal(metaRaw, &meta) == nil {
				if scoresRaw, ok := meta["scores"]; ok {
					var s RoundScores
					if json.Unmarshal(scoresRaw, &s) == nil {
						stored = &s
					}
				}
			}
		}

		refRounds = append(refRounds, refRound{input: inp, stored: stored})
	}

	if len(refRounds) == 0 {
		return 0, 0, nil, nil
	}

	// Extract team names from the first stored score entry
	alphaTN, betaTN := "", ""
	for _, rr := range refRounds {
		if rr.stored != nil {
			alphaTN = rr.stored.AlphaTeamname
			betaTN = rr.stored.BetaTeamname
			break
		}
	}

	inputs := make([]scoreRoundInput, len(refRounds))
	for i, rr := range refRounds {
		inputs[i] = rr.input
	}

	computed := computeMatchScores(inputs, alphaTN, betaTN, isNgModeMatch(matchMap))

	for i, rr := range refRounds {
		if rr.stored == nil {
			// No stored scores to compare (old file) — skip
			continue
		}
		c := computed[i]
		s := rr.stored
		roundLabel := fmt.Sprintf("round %d (map %d r%d)", i+1, c.Round.MapNum, c.Round.RoundNum)

		var mismatches []string
		if c.Alpha != s.Alpha {
			mismatches = append(mismatches, fmt.Sprintf("alpha: got %d want %d", c.Alpha, s.Alpha))
		}
		if c.Beta != s.Beta {
			mismatches = append(mismatches, fmt.Sprintf("beta: got %d want %d", c.Beta, s.Beta))
		}
		if c.CompletedMaps != s.CompletedMaps {
			mismatches = append(mismatches, fmt.Sprintf("completed_maps: got %d want %d", c.CompletedMaps, s.CompletedMaps))
		}
		if c.MatchFinished != s.MatchFinished {
			mismatches = append(mismatches, fmt.Sprintf("match_finished: got %v want %v", c.MatchFinished, s.MatchFinished))
		}
		if c.MatchWinner != s.MatchWinner {
			mismatches = append(mismatches, fmt.Sprintf("match_winner: got %q want %q", c.MatchWinner, s.MatchWinner))
		}
		if c.Round.MapNum != s.Round.MapNum {
			mismatches = append(mismatches, fmt.Sprintf("round.map_num: got %d want %d", c.Round.MapNum, s.Round.MapNum))
		}
		if c.Round.RoundNum != s.Round.RoundNum {
			mismatches = append(mismatches, fmt.Sprintf("round.round_num: got %d want %d", c.Round.RoundNum, s.Round.RoundNum))
		}
		if c.Round.Winner != s.Round.Winner {
			mismatches = append(mismatches, fmt.Sprintf("round.winner: got %q want %q", c.Round.Winner, s.Round.Winner))
		}
		if c.Round.WinnerET != s.Round.WinnerET {
			mismatches = append(mismatches, fmt.Sprintf("round.winner_et: got %d want %d", c.Round.WinnerET, s.Round.WinnerET))
		}
		if c.Round.AlphaSide != s.Round.AlphaSide {
			mismatches = append(mismatches, fmt.Sprintf("round.alpha_side: got %d want %d", c.Round.AlphaSide, s.Round.AlphaSide))
		}
		if c.Round.Fullhold != s.Round.Fullhold {
			mismatches = append(mismatches, fmt.Sprintf("round.fullhold: got %v want %v", c.Round.Fullhold, s.Round.Fullhold))
		}

		if len(mismatches) == 0 {
			passed++
		} else {
			failed++
			failLines = append(failLines, fmt.Sprintf("  FAIL %s: %s", roundLabel, strings.Join(mismatches, "; ")))
		}
	}

	return passed, failed, failLines, nil
}
