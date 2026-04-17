package main

import (
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

const usage = `etl-statsmig — ET:Legacy stats JSON format upgrader (v1.2.4 → v2.x)

Commands:
  upgrade     Upgrade old-format JSON files to new format
  validate    Validate upgraded files against old originals (and optionally new-format references)
  rescore     Inject/update metadata.scores in already-upgraded files (in-place)
  scorecheck  Dry-run scoring algorithm against reference files and report pass/fail
  desanitize  Decode Lua \\u{hex} escape sequences in JSON files back to UTF-8

Run 'etl-statsmig <command> --help' for command-specific flags.

Examples:
  etl-statsmig upgrade --in ./old --out ./out
  etl-statsmig upgrade --in ./old --out ./out --workers 32
  etl-statsmig validate --in ./old --out ./out
  etl-statsmig validate --in ./old --out ./out --ref ./new --verbose
  etl-statsmig rescore --in ./out
  etl-statsmig scorecheck --ref ./ref
  etl-statsmig desanitize --in ./ref --out ./ref-clean
  etl-statsmig desanitize --in ./ref
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "upgrade":
		runUpgradeCmd(os.Args[2:])
	case "validate":
		runValidateCmd(os.Args[2:])
	case "rescore":
		runRescoreCmd(os.Args[2:])
	case "scorecheck":
		runScorecheckCmd(os.Args[2:])
	case "desanitize":
		runDesanitizeCmd(os.Args[2:])
	case "--help", "-h", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
}

func runUpgradeCmd(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	inputDir := fs.String("in", "", "Input directory to walk recursively (required)")
	outputDir := fs.String("out", "", "Output directory mirroring input structure (required)")
	workers := fs.Int("workers", runtime.NumCPU(), "Number of parallel worker goroutines")
	apiToken := fs.String("api-token", "GameStatsWebLuaToken", "Bearer token for ETL API (used to infer team names)")
	apiBase := fs.String("api-base", etlAPIBase, "ETL API base URL")
	fs.Parse(args) //nolint: errcheck

	if *inputDir == "" || *outputDir == "" {
		fmt.Fprintln(os.Stderr, "Usage: etl-statsmig upgrade --in <dir> --out <dir> [--workers N]")
		fs.PrintDefaults()
		os.Exit(1)
	}

	inputRoot, err := filepath.Abs(*inputDir)
	if err != nil {
		log.Fatalf("bad input path: %v", err)
	}
	outputRoot, err := filepath.Abs(*outputDir)
	if err != nil {
		log.Fatalf("bad output path: %v", err)
	}

	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	cfg := upgradeConfig{apiToken: *apiToken, apiBase: *apiBase}

	jobs := make(chan string, *workers*8)

	var (
		wg        sync.WaitGroup
		processed uint64
		errCount  uint64
	)

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				if err := upgradeFile(path, inputRoot, outputRoot, cfg); err != nil {
					log.Printf("ERROR %s: %v", path, err)
					atomic.AddUint64(&errCount, 1)
				}
				n := atomic.AddUint64(&processed, 1)
				if n%1000 == 0 {
					log.Printf("processed %d files...", n)
				}
			}
		}()
	}

	walkErr := filepath.WalkDir(inputRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			log.Printf("walk error at %s: %v", path, err)
			return nil // continue walking
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".json") {
			jobs <- path
		}
		return nil
	})

	close(jobs)
	wg.Wait()

	if walkErr != nil {
		log.Printf("walk completed with error: %v", walkErr)
	}

	n := atomic.LoadUint64(&processed)
	e := atomic.LoadUint64(&errCount)
	fmt.Printf("Done. Files: %d  Errors: %d\n", n, e)
	if e > 0 {
		os.Exit(1)
	}
}
