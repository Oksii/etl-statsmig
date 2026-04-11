package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Public entry point ────────────────────────────────────────────────────────

type validateConfig struct {
	oldDir  string
	outDir  string
	refDir  string
	workers int
	verbose bool
}

// runValidateCmd parses validate sub-command args and runs validation.
func runValidateCmd(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	oldDir := fs.String("in", "", "Directory with original (old-format) JSON files (required)")
	outDir := fs.String("out", "", "Directory with upgraded output JSON files (required)")
	refDir := fs.String("ref", "", "Directory with reference new-format JSON files for structural comparison (optional)")
	workers := fs.Int("workers", runtime.NumCPU(), "Number of parallel worker goroutines")
	verbose := fs.Bool("verbose", false, "Print PASS lines and per-file detail for warnings")
	fs.Parse(args) //nolint: errcheck

	if *oldDir == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "Usage: etl-statsmig validate --in <dir> --out <dir> [--ref <dir>] [--workers N] [--verbose]")
		fs.PrintDefaults()
		os.Exit(1)
	}

	cfg := validateConfig{
		oldDir:  *oldDir,
		outDir:  *outDir,
		refDir:  *refDir,
		workers: *workers,
		verbose: *verbose,
	}
	runValidate(cfg)
}

// ── Validation result types ───────────────────────────────────────────────────

// ValidationResult holds all findings for one (old, out) file pair.
type ValidationResult struct {
	Path          string   // relative path (from out dir)
	Errors        []string // hard failures
	Warnings      []string // soft issues
	PassThrough   bool     // true when every round was already new-format (not upgraded by us)
	PTRoundCount  int      // number of pass-through rounds (for summary line)
}

func (r *ValidationResult) errorf(format string, args ...interface{}) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}
func (r *ValidationResult) warnf(format string, args ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// ── Schema key path extraction (for ref structural comparison) ─────────────────

type keyPathSet map[string]struct{}

// buildRefSchema walks refDir and extracts all JSON key paths from all files.
// Array indices are normalised to [].
func buildRefSchema(refDir string) keyPathSet {
	schema := make(keyPathSet)
	var mu sync.Mutex
	filepath.WalkDir(refDir, func(path string, d os.DirEntry, err error) error { //nolint: errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var v interface{}
		if json.Unmarshal(data, &v) != nil {
			return nil
		}
		local := make(keyPathSet)
		extractKeyPaths(v, "", local)
		mu.Lock()
		for k := range local {
			schema[k] = struct{}{}
		}
		mu.Unlock()
		return nil
	})
	return schema
}

func extractKeyPaths(v interface{}, prefix string, out keyPathSet) {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, child := range val {
			norm := normalizeDynamicKey(k)
			var p string
			if prefix == "" {
				p = norm
			} else {
				p = prefix + "." + norm
			}
			out[p] = struct{}{}
			extractKeyPaths(child, p, out)
		}
	case []interface{}:
		p := prefix + "[]"
		out[p] = struct{}{}
		for _, item := range val {
			extractKeyPaths(item, p, out)
		}
	}
}

// normalizeDynamicKey replaces data-dependent key names with stable placeholders
// so schema comparison works across different matches (different player GUIDs,
// Discord snowflakes, leveltime-keyed event maps, etc.).
func normalizeDynamicKey(k string) string {
	if len(k) == 0 {
		return k
	}
	// 32-char uppercase hex = player GUID
	if len(k) == 32 && isHex(k) {
		return "<guid>"
	}
	// All-digit key
	if isAllDigits(k) {
		switch {
		case len(k) >= 17: // Discord snowflake IDs
			return "<snowflake>"
		case len(k) >= 4: // leveltime or any numeric key (obj_*, shoves, etc.)
			return "<timestamp>"
		}
	}
	return k
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ── Core validation logic ─────────────────────────────────────────────────────

func runValidate(cfg validateConfig) {
	// Collect all out files
	type filePair struct{ rel, oldPath, outPath string }
	var pairs []filePair

	oldAbs, _ := filepath.Abs(cfg.oldDir)
	outAbs, _ := filepath.Abs(cfg.outDir)

	filepath.WalkDir(outAbs, func(path string, d os.DirEntry, err error) error { //nolint: errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}
		rel, _ := filepath.Rel(outAbs, path)
		pairs = append(pairs, filePair{
			rel:     rel,
			oldPath: filepath.Join(oldAbs, rel),
			outPath: path,
		})
		return nil
	})

	if len(pairs) == 0 {
		fmt.Println("No JSON files found in output directory.")
		return
	}

	// Build ref schema if requested
	var refSchema keyPathSet
	if cfg.refDir != "" {
		refSchema = buildRefSchema(cfg.refDir)
		fmt.Printf("Reference schema: %d unique key paths from %s\n\n", len(refSchema), cfg.refDir)
		// Also echoed into log after log file is opened (see below)
	}

	// Process in parallel
	jobs := make(chan filePair, cfg.workers*8)
	var mu sync.Mutex
	var results []ValidationResult
	var wg sync.WaitGroup

	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				res := validateFilePair(p.rel, p.oldPath, p.outPath, refSchema)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}
		}()
	}
	for _, p := range pairs {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	// Sort for deterministic output
	sort.Slice(results, func(i, j int) bool {
		return results[i].Path < results[j].Path
	})

	// Open log file in cwd
	logName := time.Now().Format("validation_2006_01_02-15_04_05.log")
	logFile, logErr := os.Create(logName)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create log file %s: %v\n", logName, logErr)
	}
	logW := io.Discard
	if logFile != nil {
		logW = logFile
		defer logFile.Close()
	}

	// emit writes to both stdout and log when the condition is met.
	// Log always mirrors stdout — no extra verbosity in the file.
	emit := func(show bool, format string, args ...interface{}) {
		if show {
			fmt.Printf(format, args...)
			fmt.Fprintf(logW, format, args...)
		}
	}

	if logFile != nil {
		fmt.Printf("Log: %s\n\n", logName)
		fmt.Fprintf(logW, "Log: %s\n\n", logName)
		if cfg.refDir != "" {
			fmt.Fprintf(logW, "Reference schema: %d unique key paths from %s\n\n", len(refSchema), cfg.refDir)
		}
	}

	// Print results
	var passed, warned, failed, skipped int
	for _, res := range results {
		// Pass-through files (already new-format, not upgraded by us) are
		// recorded as [SKIP] — they contain no errors meaningful to this tool.
		if res.PassThrough {
			skipped++
			emit(cfg.verbose, "[SKIP] %s (%d rounds, pass-through)\n", res.Path, res.PTRoundCount)
			continue
		}

		hasErr := len(res.Errors) > 0
		hasWarn := len(res.Warnings) > 0

		switch {
		case hasErr:
			failed++
			emit(true, "[FAIL] %s\n", res.Path)
			for _, e := range res.Errors {
				emit(true, "         ERROR: %s\n", e)
			}
			for _, w := range res.Warnings {
				emit(true, "         WARN:  %s\n", w)
			}
		case hasWarn:
			warned++
			passed++
			emit(cfg.verbose, "[WARN] %s\n", res.Path)
			for _, w := range res.Warnings {
				emit(cfg.verbose, "         WARN: %s\n", w)
			}
		default:
			passed++
			emit(cfg.verbose, "[PASS] %s\n", res.Path)
		}
	}

	// Summary — always shown on both stdout and log
	emit(true, "\n═══════════════════════════════════════\n")
	emit(true, "  Validation Summary\n")
	emit(true, "═══════════════════════════════════════\n")
	emit(true, "  Total files  : %d\n", len(pairs))
	emit(true, "  Upgraded     : %d\n", len(pairs)-skipped)
	emit(true, "    Passed     : %d\n", passed)
	emit(true, "    Warnings   : %d\n", warned)
	emit(true, "    Failed     : %d\n", failed)
	emit(true, "  Pass-through : %d (skipped)\n", skipped)
	emit(true, "═══════════════════════════════════════\n")

	if failed > 0 {
		os.Exit(1)
	}
}

// validateFilePair validates a single (old, out) file pair.
func validateFilePair(rel, oldPath, outPath string, refSchema keyPathSet) ValidationResult {
	res := ValidationResult{Path: rel}

	// ── Load both files ───────────────────────────────────────────────────────

	outData, err := os.ReadFile(outPath)
	if err != nil {
		res.errorf("cannot read out file: %v", err)
		return res
	}
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		res.errorf("old file missing or unreadable (%s): %v", oldPath, err)
		// Still validate out file structure partially
	}

	var outRoot map[string]json.RawMessage
	if err := json.Unmarshal(outData, &outRoot); err != nil {
		res.errorf("out file is not valid JSON: %v", err)
		return res
	}

	var oldRoot map[string]json.RawMessage
	hasOld := false
	if oldData != nil {
		if err := json.Unmarshal(oldData, &oldRoot); err != nil {
			res.errorf("old file is not valid JSON: %v", err)
		} else {
			hasOld = true
		}
	}

	// ── Top-level structure ──────────────────────────────────────────────────

	outMatchRaw, ok := outRoot["match"]
	if !ok {
		res.errorf("out file missing top-level 'match' key")
		return res
	}

	var outMatch map[string]json.RawMessage
	if err := json.Unmarshal(outMatchRaw, &outMatch); err != nil {
		res.errorf("out match is not a valid object: %v", err)
		return res
	}

	var oldMatch map[string]json.RawMessage
	if hasOld {
		if v, ok := oldRoot["match"]; ok {
			json.Unmarshal(v, &oldMatch) //nolint: errcheck
		}
	}

	// File mtime check
	checkFileMtime(&res, outPath, outMatch)

	// ── Rounds ───────────────────────────────────────────────────────────────

	outRoundsRaw, ok := outMatch["rounds"]
	if !ok {
		res.errorf("out match missing 'rounds'")
		return res
	}
	var outRounds []map[string]json.RawMessage
	if err := json.Unmarshal(outRoundsRaw, &outRounds); err != nil {
		res.errorf("out rounds is not a valid array: %v", err)
		return res
	}

	var oldRounds []map[string]json.RawMessage
	if hasOld && oldMatch != nil {
		if v, ok := oldMatch["rounds"]; ok {
			json.Unmarshal(v, &oldRounds) //nolint: errcheck
		}
	}

	if hasOld && len(outRounds) != len(oldRounds) {
		res.errorf("round count mismatch: old=%d out=%d", len(oldRounds), len(outRounds))
	}

	for i, outRound := range outRounds {
		rdRaw, ok := outRound["round_data"]
		if !ok {
			res.errorf("round %d: missing round_data", i+1)
			continue
		}
		var outRD map[string]json.RawMessage
		if err := json.Unmarshal(rdRaw, &outRD); err != nil {
			res.errorf("round %d: round_data not valid object", i+1)
			continue
		}

		var oldRD map[string]json.RawMessage
		if i < len(oldRounds) {
			if v, ok := oldRounds[i]["round_data"]; ok {
				json.Unmarshal(v, &oldRD) //nolint: errcheck
			}
		}

		prefix := fmt.Sprintf("round %d", i+1)
		if validateRoundData(&res, prefix, outRD, oldRD) {
			res.PTRoundCount++
		}
	}

	// If every round was pass-through, mark the file as such and skip ref comparison.
	if len(outRounds) > 0 && res.PTRoundCount == len(outRounds) {
		res.PassThrough = true
		return res
	}

	// ── Structural ref comparison ─────────────────────────────────────────────

	if refSchema != nil {
		var outAsInterface interface{}
		json.Unmarshal(outData, &outAsInterface) //nolint: errcheck
		outPaths := make(keyPathSet)
		extractKeyPaths(outAsInterface, "", outPaths)

		var missing []string
		for k := range refSchema {
			if _, ok := outPaths[k]; !ok {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		for _, m := range missing {
			// Some missing paths are expected (spawn/revive/pickup events can't be reconstructed)
			if isExpectedMissing(m) {
				continue
			}
			res.warnf("ref path absent in out: %s", m)
		}
	}

	return res
}

// isExpectedMissing returns true for key paths that cannot be reconstructed from old data.
// All paths use normalised placeholders (<guid>, <snowflake>, <timestamp>).
func isExpectedMissing(path string) bool {
	switch path {
	// ── Gamelog event fields present only in new-format events (spawn, pickup,
	//    weapon_fire, revive) — old format never recorded these. ─────────────
	case "match.rounds[].round_data.gamelog[].team",          // spawn
		"match.rounds[].round_data.gamelog[].weapons",        // spawn
		"match.rounds[].round_data.gamelog[].weapons[]",      // spawn
		"match.rounds[].round_data.gamelog[].item",           // pickup
		"match.rounds[].round_data.gamelog[].owner",          // pickup
		"match.rounds[].round_data.gamelog[].owner_pos",      // pickup
		"match.rounds[].round_data.gamelog[].owner_stance",   // pickup
		"match.rounds[].round_data.gamelog[].owner_stance.is_prone",
		"match.rounds[].round_data.gamelog[].owner_stance.is_crouch",
		"match.rounds[].round_data.gamelog[].owner_stance.is_downed",
		"match.rounds[].round_data.gamelog[].owner_stance.is_sprint",
		"match.rounds[].round_data.gamelog[].owner_stance.is_leaning",
		"match.rounds[].round_data.gamelog[].owner_stance.is_mounted",
		"match.rounds[].round_data.gamelog[].owner_stance.is_disguised",
		"match.rounds[].round_data.gamelog[].owner_stance.is_carrying_obj",
		"match.rounds[].round_data.gamelog[].pos",       // pickup / weapon_fire
		"match.rounds[].round_data.gamelog[].pitch",     // weapon_fire
		"match.rounds[].round_data.gamelog[].yaw",       // weapon_fire
		"match.rounds[].round_data.gamelog[].flag",      // obj_flag_captured extra field
		"match.rounds[].round_data.gamelog[].vsay_text", // vsay messages

	// ── Stance & position snapshots on kill/damage events — not in old data ──
	"match.rounds[].round_data.gamelog[].axis_alive",
		"match.rounds[].round_data.gamelog[].allies_alive",
		"match.rounds[].round_data.gamelog[].killer_pos",
		"match.rounds[].round_data.gamelog[].victim_pos",
		"match.rounds[].round_data.gamelog[].killer_class",
		"match.rounds[].round_data.gamelog[].victim_class",
		"match.rounds[].round_data.gamelog[].killer_health",
		"match.rounds[].round_data.gamelog[].victim_health",
		"match.rounds[].round_data.gamelog[].killer_stance",
		"match.rounds[].round_data.gamelog[].victim_stance",
		"match.rounds[].round_data.gamelog[].killer_reinf",
		"match.rounds[].round_data.gamelog[].killer_stance.is_prone",
		"match.rounds[].round_data.gamelog[].killer_stance.is_crouch",
		"match.rounds[].round_data.gamelog[].killer_stance.is_downed",
		"match.rounds[].round_data.gamelog[].killer_stance.is_sprint",
		"match.rounds[].round_data.gamelog[].killer_stance.is_leaning",
		"match.rounds[].round_data.gamelog[].killer_stance.is_mounted",
		"match.rounds[].round_data.gamelog[].killer_stance.is_disguised",
		"match.rounds[].round_data.gamelog[].killer_stance.is_carrying_obj",
		"match.rounds[].round_data.gamelog[].victim_stance.is_prone",
		"match.rounds[].round_data.gamelog[].victim_stance.is_crouch",
		"match.rounds[].round_data.gamelog[].victim_stance.is_downed",
		"match.rounds[].round_data.gamelog[].victim_stance.is_sprint",
		"match.rounds[].round_data.gamelog[].victim_stance.is_leaning",
		"match.rounds[].round_data.gamelog[].victim_stance.is_mounted",
		"match.rounds[].round_data.gamelog[].victim_stance.is_disguised",
		"match.rounds[].round_data.gamelog[].victim_stance.is_carrying_obj",
		"match.rounds[].round_data.gamelog[].stance",
		"match.rounds[].round_data.gamelog[].stance.is_prone",
		"match.rounds[].round_data.gamelog[].stance.is_crouch",
		"match.rounds[].round_data.gamelog[].stance.is_downed",
		"match.rounds[].round_data.gamelog[].stance.is_sprint",
		"match.rounds[].round_data.gamelog[].stance.is_leaning",
		"match.rounds[].round_data.gamelog[].stance.is_mounted",
		"match.rounds[].round_data.gamelog[].stance.is_disguised",
		"match.rounds[].round_data.gamelog[].stance.is_carrying_obj",

	// ── metadata fields populated only by new gather system ──────────────────
	"match.rounds[].round_data.metadata.scores",
		"match.rounds[].round_data.metadata.scores.beta",
		"match.rounds[].round_data.metadata.scores.alpha",
		"match.rounds[].round_data.metadata.scores.round",
		"match.rounds[].round_data.metadata.scores.round.winner",
		"match.rounds[].round_data.metadata.scores.round.map_num",
		"match.rounds[].round_data.metadata.scores.round.fullhold",
		"match.rounds[].round_data.metadata.scores.round.round_num",
		"match.rounds[].round_data.metadata.scores.round.winner_et",
		"match.rounds[].round_data.metadata.scores.round.alpha_side",
		"match.rounds[].round_data.metadata.scores.beta_teamname",
		"match.rounds[].round_data.metadata.scores.alpha_teamname",
		"match.rounds[].round_data.metadata.scores.completed_maps",
		"match.rounds[].round_data.metadata.scores.match_finished",
		"match.rounds[].round_data.metadata.scores.match_winner",
		"match.rounds[].round_data.metadata.features",
		"match.rounds[].round_data.metadata.features.auto_map",
		"match.rounds[].round_data.metadata.features.auto_sort",
		"match.rounds[].round_data.metadata.features.auto_start",
		"match.rounds[].round_data.metadata.features.auto_config",
		"match.rounds[].round_data.metadata.features.auto_rename",
		"match.rounds[].round_data.metadata.features.auto_scores",

	// ── player_stats fields not tracked in old Lua module ────────────────────
	"match.rounds[].round_data.player_stats.<guid>.player_speed",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.kph_avg",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.mph_avg",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.ups_avg",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.kph_peak",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.mph_peak",
		"match.rounds[].round_data.player_stats.<guid>.player_speed.ups_peak",
		// Distance / spawn / stance tracking added after old-format era
		"match.rounds[].round_data.player_stats.<guid>.spawn_count",
		"match.rounds[].round_data.player_stats.<guid>.distance_travelled_meters",
		"match.rounds[].round_data.player_stats.<guid>.distance_travelled_spawn",
		"match.rounds[].round_data.player_stats.<guid>.distance_travelled_spawn_avg",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_crouch",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_disguise",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_lean",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_mg",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_objcarrier",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_prone",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_sprint",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_turtle",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.in_vehiclescort",
		"match.rounds[].round_data.player_stats.<guid>.stance_stats_seconds.is_downed",
		// weaponStats added after old-format era
		"match.rounds[].round_data.player_stats.<guid>.weaponStats[]",

	// ── round_info fields added after old-format era ──────────────────────────
	"match.rounds[].round_data.round_info.round_start_unix",
		"match.rounds[].round_data.round_info.round_end_unix",
		// round_start/round_end absent in very old files; preservation is enforced
		// per-field by validateRoundInfo so the ref-schema warning is noise.
		"match.rounds[].round_data.round_info.round_start",
		"match.rounds[].round_data.round_info.round_end",
		// server_ip/port/original_match_id absent in some very old files
		"match.rounds[].round_data.round_info.server_ip",
		"match.rounds[].round_data.round_info.server_port",
		"match.rounds[].round_data.round_info.original_match_id",

	// ── metadata optional fields ──────────────────────────────────────────────
	// server_port uses omitempty; absent when not recorded in old file
	"match.rounds[].round_data.metadata.server_port",

	// ── gamelog[].unixtime — only present when round_start_unix is available ──
	"match.rounds[].round_data.gamelog[].unixtime",

	// ── match-level score/result fields from the gather system ───────────────
	"match.alpha_score",
		"match.beta_score",
		"match.match_score",

	// ── ranks_start / ranks_end — Discord-keyed, data not present in old files ─
	"match.ranks_start.<snowflake>",
		"match.ranks_end.<snowflake>",
		"match.ranks_end[]",

	// ── player profile fields added after old-format era ─────────────────────
	"match.alpha_team[].country",
		"match.beta_team[].country",
		// nick/id absent when old file had no team roster attached
		"match.alpha_team[].nick",
		"match.alpha_team[].id",
		"match.beta_team[].nick",
		"match.beta_team[].id",

	// ── match-level fields absent in some old files ───────────────────────────
	"match.original_match_id",
		"match.server.port",
		"match.server.instance",

	// ── round_filename — ref files store an external round reference pointer;
	//    we always embed round_data inline and never write this key ────────────
	"match.rounds[].round_filename",

	// ── round_data sub-paths absent when old file had round_filename-only rounds
	//    (external references with no embedded data). The upgrader copies these
	//    as-is. Hard errors in validateRoundData already catch genuinely missing
	//    sections, so these ref-schema warnings are redundant noise. ────────────
	"match.rounds[].round_data",
		"match.rounds[].round_data.round_info",
		"match.rounds[].round_data.round_info.round",
		"match.rounds[].round_data.round_info.mapname",
		"match.rounds[].round_data.round_info.matchID",
		"match.rounds[].round_data.round_info.config",
		"match.rounds[].round_data.round_info.servername",
		"match.rounds[].round_data.round_info.stats_version",
		"match.rounds[].round_data.round_info.mod_version",
		"match.rounds[].round_data.round_info.et_version",
		"match.rounds[].round_data.round_info.winnerteam",
		"match.rounds[].round_data.round_info.defenderteam",
		"match.rounds[].round_data.round_info.timelimit",
		"match.rounds[].round_data.round_info.nextTimeLimit",
		"match.rounds[].round_data.metadata",
		"match.rounds[].round_data.metadata.config",
		"match.rounds[].round_data.metadata.servername",
		"match.rounds[].round_data.metadata.matchID",
		"match.rounds[].round_data.metadata.server_ip",
		"match.rounds[].round_data.metadata.et_version",
		"match.rounds[].round_data.metadata.mod_version",
		"match.rounds[].round_data.metadata.stats_version",
		"match.rounds[].round_data.gamelog",
		"match.rounds[].round_data.gamelog[]",
		"match.rounds[].round_data.gamelog[].group",
		"match.rounds[].round_data.gamelog[].label",
		"match.rounds[].round_data.gamelog[].match_id",
		"match.rounds[].round_data.gamelog[].round_id",
		"match.rounds[].round_data.gamelog[].leveltime",
		"match.rounds[].round_data.player_stats",

	// ── newer player_stats entry format — ref files embed guid/name/team/rounds
	//    directly inside each player entry; old format does not have these ─────
	"match.rounds[].round_data.player_stats.<guid>",
		"match.rounds[].round_data.player_stats.<guid>.guid",
		"match.rounds[].round_data.player_stats.<guid>.name",
		"match.rounds[].round_data.player_stats.<guid>.team",
		"match.rounds[].round_data.player_stats.<guid>.rounds",
		"match.rounds[].round_data.player_stats.<guid>.weaponStats":
		return true
	}

	// Gamelog event-specific fields are data-dependent — only present on certain
	// event types. The ref file is a different match, so absence of e.g. a
	// "damage" field just means this match had no damage events, not a schema gap.
	// Required fields (group, label, match_id, round_id, leveltime) are validated
	// directly by validateGamelog, not by the ref schema comparison.
	optionalGamelogFields := []string{
		"match.rounds[].round_data.gamelog[].killer",
		"match.rounds[].round_data.gamelog[].victim",
		"match.rounds[].round_data.gamelog[].weapon",
		"match.rounds[].round_data.gamelog[].damage",
		"match.rounds[].round_data.gamelog[].damage_flags",
		"match.rounds[].round_data.gamelog[].hit_region",
		"match.rounds[].round_data.gamelog[].message",
		"match.rounds[].round_data.gamelog[].command",
		"match.rounds[].round_data.gamelog[].class",
		"match.rounds[].round_data.gamelog[].objective",
		"match.rounds[].round_data.gamelog[].player",
		"match.rounds[].round_data.gamelog[].victim_reinf",
		"match.rounds[].round_data.gamelog[].killer_reinf",
	}
	for _, f := range optionalGamelogFields {
		if path == f {
			return true
		}
	}

	// Player_stats obj_* and shove paths are data-dependent (only present when
	// those objective/shove events occurred during a match). The ref file and
	// old file are different matches with different gameplay, so absence of a
	// specific obj type is normal — not a schema gap.
	optionalPlayerPrefixes := []string{
		"match.rounds[].round_data.player_stats.<guid>.obj_",
		"match.rounds[].round_data.player_stats.<guid>.shoves_",
	}
	for _, pfx := range optionalPlayerPrefixes {
		if strings.HasPrefix(path, pfx) {
			return true
		}
	}

	return false
}

// isPassThroughRound returns true when a round was NOT upgraded by this tool —
// i.e., it already had a gamelog when the upgrader ran. We detect this by
// checking whether stats_version == OldStatsVersion: we only ever write "1.2.4",
// so any other value means the round came from the live new-format Lua module.
func isPassThroughRound(outRD map[string]json.RawMessage) bool {
	riRaw, ok := outRD["round_info"]
	if !ok {
		return false
	}
	var ri map[string]json.RawMessage
	if err := json.Unmarshal(riRaw, &ri); err != nil {
		return false
	}
	v, ok := ri["stats_version"]
	if !ok {
		return false // no stats_version → old format → we upgraded it
	}
	var sv string
	json.Unmarshal(v, &sv) //nolint: errcheck
	return sv != OldStatsVersion
}

// ── Round-data validation ─────────────────────────────────────────────────────

// validateRoundData returns true if the round was a pass-through (not upgraded by us).
func validateRoundData(res *ValidationResult, prefix string, outRD, oldRD map[string]json.RawMessage) (passThrough bool) {
	if isPassThroughRound(outRD) {
		return true // skip all checks — we didn't touch this round
	}

	// Required new sections
	for _, key := range []string{"round_info", "player_stats", "gamelog", "metadata"} {
		if _, ok := outRD[key]; !ok {
			res.errorf("%s: missing '%s' in round_data", prefix, key)
		}
	}

	// Parse out round_info
	var outRI, oldRI map[string]json.RawMessage
	if v, ok := outRD["round_info"]; ok {
		json.Unmarshal(v, &outRI) //nolint: errcheck
	}
	if oldRD != nil {
		if v, ok := oldRD["round_info"]; ok {
			json.Unmarshal(v, &oldRI) //nolint: errcheck
		}
	}

	validateRoundInfo(res, prefix, outRI, oldRI)

	// Parse gamelog
	var outGamelog []map[string]json.RawMessage
	if v, ok := outRD["gamelog"]; ok {
		json.Unmarshal(v, &outGamelog) //nolint: errcheck
	}

	// Parse old collections for count checks
	var oldRI_typed OldRoundInfo
	if oldRD != nil {
		if v, ok := oldRD["round_info"]; ok {
			json.Unmarshal(v, &oldRI_typed) //nolint: errcheck
		}
	}

	validateGamelog(res, prefix, outGamelog, outRI, oldRI_typed)

	// Parse metadata
	if v, ok := outRD["metadata"]; ok {
		var meta map[string]json.RawMessage
		json.Unmarshal(v, &meta) //nolint: errcheck
		validateMetadata(res, prefix, meta)
	}

	// Parse player_stats
	var outPS map[string]json.RawMessage
	if v, ok := outRD["player_stats"]; ok {
		json.Unmarshal(v, &outPS) //nolint: errcheck
	}
	var oldPS map[string]json.RawMessage
	if oldRD != nil {
		if v, ok := oldRD["player_stats"]; ok {
			json.Unmarshal(v, &oldPS) //nolint: errcheck
		}
	}
	validatePlayerStats(res, prefix, outPS, oldPS, outRI)
	return false
}

// ── round_info checks ─────────────────────────────────────────────────────────

func validateRoundInfo(res *ValidationResult, prefix string, outRI, oldRI map[string]json.RawMessage) {
	if outRI == nil {
		return
	}

	// Must have version fields
	for field, expected := range map[string]string{
		"stats_version": OldStatsVersion,
		"et_version":    OldETVersion,
		"mod_version":   OldModVersion,
	} {
		v, ok := outRI[field]
		if !ok {
			res.errorf("%s: round_info missing '%s'", prefix, field)
			continue
		}
		var got string
		json.Unmarshal(v, &got) //nolint: errcheck
		if got != expected {
			res.errorf("%s: round_info.%s = %q, want %q", prefix, field, got, expected)
		}
	}

	// Must NOT have old collection fields
	for _, forbidden := range []string{"messages", "obituaries", "damageStats"} {
		if _, ok := outRI[forbidden]; ok {
			res.errorf("%s: round_info still contains '%s' (should have been moved to gamelog)", prefix, forbidden)
		}
	}

	// Core fields must be preserved from old
	if oldRI != nil {
		for _, field := range []string{"round", "mapname", "matchID", "winnerteam", "round_start", "round_end"} {
			outV, outOK := outRI[field]
			oldV, oldOK := oldRI[field]
			if oldOK && !outOK {
				res.errorf("%s: round_info.%s present in old but absent in out", prefix, field)
				continue
			}
			if oldOK && outOK && string(outV) != string(oldV) {
				res.errorf("%s: round_info.%s value changed: old=%s out=%s", prefix, field, oldV, outV)
			}
		}
	}
}

// ── metadata checks ───────────────────────────────────────────────────────────

func validateMetadata(res *ValidationResult, prefix string, meta map[string]json.RawMessage) {
	if meta == nil {
		res.errorf("%s: metadata is nil or unparseable", prefix)
		return
	}
	required := []string{"servername", "config", "matchID", "server_ip", "stats_version", "et_version", "mod_version", "features", "scores"}
	for _, k := range required {
		if _, ok := meta[k]; !ok {
			res.errorf("%s: metadata missing '%s'", prefix, k)
		}
	}

	for field, expected := range map[string]string{
		"stats_version": OldStatsVersion,
		"et_version":    OldETVersion,
		"mod_version":   OldModVersion,
	} {
		if v, ok := meta[field]; ok {
			var got string
			json.Unmarshal(v, &got) //nolint: errcheck
			if got != expected {
				res.errorf("%s: metadata.%s = %q, want %q", prefix, field, got, expected)
			}
		}
	}
}

// ── gamelog checks ────────────────────────────────────────────────────────────

func validateGamelog(res *ValidationResult, prefix string,
	events []map[string]json.RawMessage,
	outRI map[string]json.RawMessage,
	oldRI OldRoundInfo) {

	if events == nil {
		res.errorf("%s: gamelog is nil or unparseable", prefix)
		return
	}
	if len(events) == 0 {
		res.errorf("%s: gamelog is empty", prefix)
		return
	}

	// Count events by label
	labelCount := make(map[string]int)
	for _, e := range events {
		var label string
		if v, ok := e["label"]; ok {
			json.Unmarshal(v, &label) //nolint: errcheck
		}
		labelCount[label]++
	}

	// Must start with round_start
	var firstLabel string
	if v, ok := events[0]["label"]; ok {
		json.Unmarshal(v, &firstLabel) //nolint: errcheck
	}
	if firstLabel != "round_start" {
		res.errorf("%s: gamelog first event label = %q, want 'round_start'", prefix, firstLabel)
	}

	// Must end with round_end
	var lastLabel string
	if v, ok := events[len(events)-1]["label"]; ok {
		json.Unmarshal(v, &lastLabel) //nolint: errcheck
	}
	if lastLabel != "round_end" {
		res.errorf("%s: gamelog last event label = %q, want 'round_end'", prefix, lastLabel)
	}

	// Inner events (between the forced round_start/round_end bookends) must be
	// sorted by leveltime. The bookends are excluded from this check because
	// player-side events (e.g. class_change at round teardown) can legitimately
	// carry leveltimes outside the nominal round window.
	if len(events) > 2 {
		var prevLT int64 = -1
		for i, e := range events[1 : len(events)-1] {
			var lt int64
			if v, ok := e["leveltime"]; ok {
				json.Unmarshal(v, &lt) //nolint: errcheck
			}
			if lt < prevLT {
				res.errorf("%s: gamelog inner events not sorted at index %d (prev=%d cur=%d)", prefix, i+1, prevLT, lt)
				break
			}
			prevLT = lt
		}
	}

	// All events must have required base fields
	requiredEventFields := []string{"group", "label", "match_id", "round_id", "leveltime"}
	for i, e := range events {
		for _, f := range requiredEventFields {
			if _, ok := e[f]; !ok {
				res.errorf("%s: gamelog[%d] missing required field '%s'", prefix, i, f)
			}
		}
		// obj_flag_captured must use "flag" field, not "objective"
		var evtLabel string
		if v, ok := e["label"]; ok {
			json.Unmarshal(v, &evtLabel) //nolint: errcheck
		}
		if evtLabel == "obj_flag_captured" {
			if _, ok := e["flag"]; !ok {
				res.errorf("%s: gamelog[%d] obj_flag_captured event missing 'flag' field", prefix, i)
			}
			if _, ok := e["objective"]; ok {
				res.errorf("%s: gamelog[%d] obj_flag_captured event incorrectly uses 'objective' instead of 'flag'", prefix, i)
			}
		}
	}

	// Count checks against old source data
	if len(oldRI.Messages) > 0 {
		got := labelCount["message"]
		want := len(oldRI.Messages)
		if got != want {
			res.errorf("%s: gamelog message events = %d, want %d (from old messages)", prefix, got, want)
		}
	}

	if len(oldRI.Obituaries) > 0 {
		got := labelCount["kill"] + labelCount["suicide"] + labelCount["teamkill"]
		want := len(oldRI.Obituaries)
		if got != want {
			res.errorf("%s: gamelog kill+suicide+teamkill = %d, want %d (from old obituaries)", prefix, got, want)
		}
	}

	if len(oldRI.DamageStats) > 0 {
		got := labelCount["damage"]
		want := len(oldRI.DamageStats)
		if got != want {
			res.errorf("%s: gamelog damage events = %d, want %d (from old damageStats)", prefix, got, want)
		}
	}

	// Unixtime consistency check (spot-check first 20 events with unixtime)
	var roundStart int64
	var roundStartUnix *int64
	if outRI != nil {
		if v, ok := outRI["round_start"]; ok {
			json.Unmarshal(v, &roundStart) //nolint: errcheck
		}
		if v, ok := outRI["round_start_unix"]; ok {
			var u int64
			if json.Unmarshal(v, &u) == nil {
				roundStartUnix = &u
			}
		}
	}

	if roundStartUnix != nil {
		checked := 0
		for i, e := range events {
			if checked >= 20 {
				break
			}
			unixtimeRaw, hasUnix := e["unixtime"]
			leveltimeRaw, hasLT := e["leveltime"]
			if !hasUnix || !hasLT {
				continue
			}
			var unixtime, leveltime int64
			json.Unmarshal(unixtimeRaw, &unixtime) //nolint: errcheck
			json.Unmarshal(leveltimeRaw, &leveltime) //nolint: errcheck

			// Expected: unixtime_ms = roundStartUnix*1000 + (leveltime - roundStart)
			expected := *roundStartUnix*1000 + (leveltime - roundStart)
			delta := unixtime - expected
			if math.Abs(float64(delta)) > 5000 { // allow 5 second drift
				res.errorf("%s: gamelog[%d] unixtime inconsistency: got=%d expected=%d delta=%dms",
					prefix, i, unixtime, expected, delta)
			}
			checked++
		}
	}
}

// ── player_stats checks ───────────────────────────────────────────────────────

func validatePlayerStats(res *ValidationResult, prefix string,
	outPS, oldPS map[string]json.RawMessage,
	outRI map[string]json.RawMessage) {

	if outPS == nil {
		return
	}

	// All player GUIDs from old must be present in out
	if oldPS != nil {
		for guid := range oldPS {
			if _, ok := outPS[guid]; !ok {
				res.errorf("%s: player_stats missing GUID %s (present in old)", prefix, guid)
			}
		}
		if len(outPS) != len(oldPS) {
			res.warnf("%s: player_stats GUID count changed: old=%d out=%d", prefix, len(oldPS), len(outPS))
		}
	}

	// Parse round_start info for timestamp validation
	var roundStart int64
	var roundStartUnix *int64
	if outRI != nil {
		if v, ok := outRI["round_start"]; ok {
			json.Unmarshal(v, &roundStart) //nolint: errcheck
		}
		if v, ok := outRI["round_start_unix"]; ok {
			var u int64
			if json.Unmarshal(v, &u) == nil {
				roundStartUnix = &u
			}
		}
	}

	for guid, raw := range outPS {
		var pm map[string]json.RawMessage
		if err := json.Unmarshal(raw, &pm); err != nil {
			res.errorf("%s: player %s: not a valid object", prefix, guid[:min(8, len(guid))])
			continue
		}
		playerPrefix := fmt.Sprintf("%s player %s", prefix, guid[:min(8, len(guid))])

		// class_switches must be absent — moved exclusively to gamelog as class_change events
		if _, ok := pm["class_switches"]; ok {
			res.errorf("%s: class_switches still present in player_stats (must be removed; data belongs in gamelog)", playerPrefix)
		}

		// shoves format check
		for _, key := range []string{"shoves_given", "shoves_received"} {
			if v, ok := pm[key]; ok {
				checkShovesFormat(res, playerPrefix, key, v, roundStart, roundStartUnix)
			}
		}

		// obj_* format check: all obj_ values must be upgraded to {objective, timestamp_unix}
		// obj_flagcaptured retains its name in player_stats (only gamelog label differs).
		for _, label := range objLabels {
			if v, ok := pm[label]; ok {
				checkObjFormat(res, playerPrefix, label, v, roundStart, roundStartUnix)
			}
		}
	}
}


func checkShovesFormat(res *ValidationResult, prefix, key string, raw json.RawMessage,
	roundStart int64, roundStartUnix *int64) {

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	for lt, val := range m {
		if len(val) == 0 {
			continue
		}
		if val[0] == '"' {
			res.errorf("%s: %s[%s] value is still a plain string (not upgraded to {objective, timestamp_unix})", prefix, key, lt)
			continue
		}
		if val[0] == '{' {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(val, &obj); err != nil {
				res.errorf("%s: %s[%s] object is invalid JSON", prefix, key, lt)
				continue
			}
			if _, ok := obj["objective"]; !ok {
				res.errorf("%s: %s[%s] object missing 'objective' field", prefix, key, lt)
			}
			if _, ok := obj["timestamp_unix"]; !ok {
				res.errorf("%s: %s[%s] object missing 'timestamp_unix' field", prefix, key, lt)
			} else if roundStartUnix != nil {
				// Verify timestamp_unix is in reasonable range
				var ts int64
				json.Unmarshal(obj["timestamp_unix"], &ts) //nolint: errcheck
				lti, _ := parseInt64(lt)
				expected := *roundStartUnix + (lti-roundStart)/1000
				delta := ts - expected
				if math.Abs(float64(delta)) > 60 {
					res.warnf("%s: %s[%s].timestamp_unix=%d expected~%d (delta=%ds)",
						prefix, key, lt, ts, expected, delta)
				}
			}
		}
	}
}

func checkObjFormat(res *ValidationResult, prefix, label string, raw json.RawMessage,
	roundStart int64, roundStartUnix *int64) {

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	for lt, val := range m {
		if len(val) == 0 {
			continue
		}
		if val[0] == '"' {
			res.errorf("%s: %s[%s] value is still a plain string (not upgraded to {objective, timestamp_unix})", prefix, label, lt)
			continue
		}
		if val[0] == '{' {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(val, &obj); err != nil {
				res.errorf("%s: %s[%s] object is invalid JSON", prefix, label, lt)
				continue
			}
			if _, ok := obj["objective"]; !ok {
				res.errorf("%s: %s[%s] object missing 'objective' field", prefix, label, lt)
			}
			if _, ok := obj["timestamp_unix"]; !ok {
				res.errorf("%s: %s[%s] object missing 'timestamp_unix' field", prefix, label, lt)
			}
		}
	}
}

// ── File mtime check ──────────────────────────────────────────────────────────

func checkFileMtime(res *ValidationResult, outPath string, matchMap map[string]json.RawMessage) {
	var endTime, startTime int64
	if v, ok := matchMap["end_time"]; ok {
		json.Unmarshal(v, &endTime) //nolint: errcheck
	}
	if v, ok := matchMap["start_time"]; ok {
		json.Unmarshal(v, &startTime) //nolint: errcheck
	}

	expected := endTime
	if expected == 0 {
		expected = startTime
	}
	if expected == 0 {
		res.warnf("no end_time or start_time in match — cannot verify file mtime")
		return
	}

	info, err := os.Stat(outPath)
	if err != nil {
		res.warnf("cannot stat out file for mtime check: %v", err)
		return
	}

	gotUnix := info.ModTime().Unix()
	delta := gotUnix - expected
	if math.Abs(float64(delta)) > 2 {
		res.errorf("file mtime %s (%d) does not match match end_time/start_time %s (%d) — delta=%ds",
			info.ModTime().Format(time.RFC3339), gotUnix,
			time.Unix(expected, 0).Format(time.RFC3339), expected, delta)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseInt64(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
