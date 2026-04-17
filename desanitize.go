package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// luaEscapeRe matches Lua \u{hex} escape sequences as they appear in
// parsed Go strings (single backslash prefix).
var luaEscapeRe = regexp.MustCompile(`\\u\{([0-9a-fA-F]+)\}`)

// desanitizeLuaString decodes Lua \u{hex} escape sequences back to UTF-8.
//
// The Lua sanitizer encoded characters using string.byte(c), producing
// individual byte values for each byte of a multi-byte UTF-8 sequence.
// For example, 'ö' (UTF-8: 0xC3 0xB6) was encoded as \u{c3}\u{b6}.
//
// Decoding rules:
//   - Values 0x00–0x7F: treated as direct Unicode codepoints (ASCII).
//   - Values 0x80–0xFF: treated as raw UTF-8 bytes. Adjacent sequences in
//     this range are collected and decoded together as a UTF-8 byte sequence.
//   - Values > 0xFF: treated as direct Unicode codepoints.
func desanitizeLuaString(s string) string {
	matches := luaEscapeRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	pos := 0

	for i := 0; i < len(matches); {
		m := matches[i]
		b.WriteString(s[pos:m[0]])

		val, _ := strconv.ParseUint(s[m[2]:m[3]], 16, 32)

		if val >= 0x80 && val <= 0xFF {
			// Collect all adjacent byte-range escapes into a raw byte slice.
			raw := []byte{byte(val)}
			j := i + 1
			for j < len(matches) && matches[j][0] == matches[j-1][1] {
				nv, err := strconv.ParseUint(s[matches[j][2]:matches[j][3]], 16, 32)
				if err != nil || nv < 0x80 || nv > 0xFF {
					break
				}
				raw = append(raw, byte(nv))
				j++
			}
			// Write as raw UTF-8 bytes. If the sequence is invalid UTF-8,
			// DecodeRune inserts utf8.RuneError for each bad byte.
			if utf8.Valid(raw) {
				b.Write(raw)
			} else {
				for len(raw) > 0 {
					r, size := utf8.DecodeRune(raw)
					b.WriteRune(r)
					raw = raw[size:]
				}
			}
			pos = matches[j-1][1]
			i = j
		} else {
			// Direct Unicode codepoint.
			b.WriteRune(rune(val))
			pos = m[1]
			i++
		}
	}

	b.WriteString(s[pos:])
	return b.String()
}

// desanitizeAny recursively walks a JSON-decoded value and desanitizes all
// string leaves.
func desanitizeAny(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return desanitizeLuaString(t)
	case map[string]interface{}:
		for k, val := range t {
			t[k] = desanitizeAny(val)
		}
		return t
	case []interface{}:
		for i, val := range t {
			t[i] = desanitizeAny(val)
		}
		return t
	}
	return v
}

// desanitizeFile reads a JSON file, decodes all Lua escape sequences in
// string values, and writes the result to outPath (creating parent dirs as
// needed). File mtime is preserved.
func desanitizeFile(inPath, outPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	// Preserve mtime before any write.
	info, err := os.Stat(inPath)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	mtime := info.ModTime()

	var root interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	root = desanitizeAny(root)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	os.Chtimes(outPath, mtime, mtime) //nolint: errcheck
	return nil
}

func runDesanitizeCmd(args []string) {
	fs := flag.NewFlagSet("desanitize", flag.ExitOnError)
	inputDir := fs.String("in", "", "Input directory to walk recursively (required)")
	outputDir := fs.String("out", "", "Output directory (optional; omit to overwrite in place)")
	workers := fs.Int("workers", runtime.NumCPU(), "Number of parallel worker goroutines")
	fs.Parse(args) //nolint: errcheck

	if *inputDir == "" {
		fmt.Fprintln(os.Stderr, "Usage: etl-statsmig desanitize --in <dir> [--out <dir>] [--workers N]")
		fs.PrintDefaults()
		os.Exit(1)
	}

	inputRoot, err := filepath.Abs(*inputDir)
	if err != nil {
		log.Fatalf("bad input path: %v", err)
	}

	inPlace := *outputDir == ""
	outputRoot := inputRoot
	if !inPlace {
		outputRoot, err = filepath.Abs(*outputDir)
		if err != nil {
			log.Fatalf("bad output path: %v", err)
		}
		if err := os.MkdirAll(outputRoot, 0o755); err != nil {
			log.Fatalf("create output dir: %v", err)
		}
	}

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
			for inPath := range jobs {
				rel, _ := filepath.Rel(inputRoot, inPath)
				outPath := filepath.Join(outputRoot, rel)
				if err := desanitizeFile(inPath, outPath); err != nil {
					log.Printf("ERROR %s: %v", inPath, err)
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
			return nil
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
