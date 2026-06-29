package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Match is one piece of data flowing through the results channel.
// For -name mode, LineNum is 0 and Line is empty (we just print the path).
// For content mode, both are populated so we can print grep-style output.
type Match struct {
	Path    string
	LineNum int
	Line    string
}

// matcher hides whether we're doing substring or regex matching from the
// hot loop. Building it once up-front (instead of branching on every line)
// is both faster and cleaner.
type matcher func(s string) bool

func buildMatcher(pattern string, useRegex, caseInsensitive bool) (matcher, error) {
	if useRegex {
		if caseInsensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}
	if caseInsensitive {
		lower := strings.ToLower(pattern)
		return func(s string) bool {
			return strings.Contains(strings.ToLower(s), lower)
		}, nil
	}
	return func(s string) bool {
		return strings.Contains(s, pattern)
	}, nil
}

// Directories we skip by default. Override with -all.
var defaultIgnoredDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

func main() {
	pattern := flag.String("p", "", "search pattern (required)")
	root := flag.String("dir", ".", "root directory to search")
	useRegex := flag.Bool("regex", false, "treat pattern as a regular expression")
	caseInsensitive := flag.Bool("i", false, "case-insensitive match")
	nameOnly := flag.Bool("name", false, "match filenames only (skip file contents)")
	workers := flag.Int("workers", runtime.NumCPU(), "number of concurrent workers")
	showAll := flag.Bool("all", false, "do not skip default-ignored dirs (.git, node_modules, vendor)")
	flag.Parse()

	if *pattern == "" {
		fmt.Fprintln(os.Stderr, "error: -p (search pattern) is required")
		flag.Usage()
		os.Exit(2)
	}

	match, err := buildMatcher(*pattern, *useRegex, *caseInsensitive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid pattern: %v\n", err)
		os.Exit(2)
	}

	// Buffered channels let the producer and consumers run ahead of each
	// other a bit, smoothing over short stalls (e.g. one slow file).
	jobs := make(chan string, 64)
	results := make(chan Match, 64)

	// ─── Stage 1: walker (producer) ────────────────────────────────────────
	// One goroutine walks the directory tree and pushes file paths into jobs.
	// When the walk finishes, we close(jobs) — this is the signal that tells
	// workers "no more files coming, you can exit your for-range loops."
	go func() {
		defer close(jobs)
		err := filepath.WalkDir(*root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: %s: %v\n", path, err)
				return nil
			}
			if d.IsDir() {
				// SkipDir tells WalkDir not to descend into this directory.
				// We only skip *nested* ignored dirs, not the root itself, so
				// you can still run `file-searcher -dir .git -p ...` if you want.
				if !*showAll && defaultIgnoredDirs[d.Name()] && path != *root {
					return filepath.SkipDir
				}
				return nil
			}
			jobs <- path
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk error: %v\n", err)
		}
	}()

	// ─── Stage 2: worker pool (consumers/producers) ────────────────────────
	// N goroutines pull file paths off `jobs`, search them, push matches into
	// `results`. WaitGroup tracks how many workers are still running so we
	// know when it's safe to close(results).
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				searchFile(path, match, *nameOnly, results)
			}
		}()
	}

	// ─── Stage 3: closer ───────────────────────────────────────────────────
	// We need *something* to close `results` once every worker is done — but
	// that must happen in a separate goroutine, because main is busy reading
	// from `results` below and would deadlock otherwise.
	go func() {
		wg.Wait()
		close(results)
	}()

	// ─── Stage 4: printer (main goroutine) ─────────────────────────────────
	// Drain results until the closer above closes the channel. Doing this on
	// the main goroutine means we don't need a second WaitGroup for printing.
	count := 0
	for m := range results {
		if m.LineNum == 0 {
			fmt.Printf("%s\n", m.Path)
		} else {
			fmt.Printf("%s:%d: %s\n", m.Path, m.LineNum, m.Line)
		}
		count++
	}
	fmt.Fprintf(os.Stderr, "\n%d match(es)\n", count)
}

func searchFile(path string, match matcher, nameOnly bool, results chan<- Match) {
	if nameOnly {
		if match(filepath.Base(path)) {
			results <- Match{Path: path}
		}
		return
	}

	f, err := os.Open(path)
	if err != nil {
		// Permission denied, broken symlink, etc. — skip silently. Logging
		// every one of these on a big tree would drown out real matches.
		return
	}
	defer f.Close()

	// Skip binary files. Heuristic: if the first 512 bytes contain a NUL
	// byte, it's almost certainly binary (same trick grep uses). Then we
	// rewind so the scanner below sees the full file from byte 0.
	sniff := make([]byte, 512)
	n, _ := f.Read(sniff)
	if bytes.IndexByte(sniff[:n], 0) != -1 {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}

	scanner := bufio.NewScanner(f)
	// Default bufio.Scanner caps lines at 64 KB; bump to 1 MB so we don't
	// choke on minified JS or long log lines.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if match(line) {
			results <- Match{Path: path, LineNum: lineNum, Line: line}
		}
	}
}
