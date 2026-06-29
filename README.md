# file-searcher

A small concurrent file-search CLI written in Go — like a tiny `grep` + `find`
combined. It walks a directory tree, fans the file paths out to a pool of
worker goroutines, and prints matches as they're found.

Built as a learning exercise for Go's concurrency primitives:
**goroutines**, **channels**, and **`sync.WaitGroup`**.

## Build

```sh
go build -o file-searcher .
```

You can also skip the build step and use `go run .` while iterating.

## Usage

```sh
./file-searcher -p <pattern> [flags]
```

### Flags

| Flag       | Default       | Description                                                    |
| ---------- | ------------- | -------------------------------------------------------------- |
| `-p`       | *(required)*  | Search pattern.                                                |
| `-dir`     | `.`           | Root directory to search.                                      |
| `-regex`   | `false`       | Treat `-p` as a regular expression (Go's `regexp` syntax).     |
| `-i`       | `false`       | Case-insensitive match.                                        |
| `-name`    | `false`       | Match filenames only (don't open file contents).               |
| `-workers` | `NumCPU()`    | Number of concurrent worker goroutines.                        |
| `-all`     | `false`       | Don't skip `.git`, `node_modules`, `vendor`.                   |

### Examples

Search file contents (default mode):

```sh
./file-searcher -p "TODO" -dir ./src
```

Case-insensitive:

```sh
./file-searcher -p "fixme" -i -dir .
```

Regex — find Go function definitions:

```sh
./file-searcher -p "^func\s+\w+" -regex -dir .
```

Search filenames only:

```sh
./file-searcher -p "config" -name -dir .
```

Crank up the worker count for a huge tree:

```sh
./file-searcher -p "deprecated" -workers 16 -dir /path/to/big/repo
```

Include normally-ignored directories:

```sh
./file-searcher -p "commit" -all -dir .
```

## Output format

Content matches are printed in `grep -n` style:

```
path/to/file.go:42: the matching line text
```

Filename-only matches print just the path:

```
path/to/matching-file.go
```

A summary like `3 match(es)` is written to stderr at the end, so you can pipe
stdout (`./file-searcher ... > results.txt`) without it getting mixed in.

## How it works (the concurrency pipeline)

```
┌──────────────┐  jobs chan   ┌────────────────┐  results chan  ┌─────────┐
│  walker      │ ───────────> │  worker pool   │ ─────────────> │ printer │
│ (1 goroutine)│              │ (N goroutines) │                │  (main) │
└──────────────┘              └────────────────┘                └─────────┘
```

1. **Walker goroutine** uses `filepath.WalkDir` to traverse the tree, pushing
   each file path into the `jobs` channel. When the walk finishes it
   `close(jobs)`, signalling "no more work."
2. **Worker pool** — `N` goroutines (default = `runtime.NumCPU()`) read from
   `jobs`, open and scan each file, and push every `Match` they find into the
   `results` channel. A `sync.WaitGroup` tracks how many workers are still
   alive.
3. **Closer goroutine** does one job: `wg.Wait()`, then `close(results)`.
   This has to be on its own goroutine because main is busy printing.
4. **Printer (main goroutine)** ranges over `results` and prints. The
   `for ... range results` loop exits naturally once the closer above closes
   the channel.

### Why these primitives?

- **Channels** are typed pipes between goroutines — safer than sharing memory
  with locks. Here they form a producer → workers → consumer pipeline.
- **`sync.WaitGroup`** lets one goroutine wait for a group of others to
  finish. We use it to know when *all* workers have finished so we can close
  the results channel.
- **Closing channels** is how Go signals "no more data." A `for x := range ch`
  loop ends cleanly when `ch` is closed, draining whatever's still buffered.

### Why a worker *pool* instead of one goroutine per file?

Goroutines are cheap (~2 KB each), but file descriptors aren't — spawning one
goroutine per file on a million-file tree would exhaust the OS's FD limit.
A fixed pool bounds concurrency to something the OS can handle while still
keeping all CPU cores busy.

## Things skipped automatically

- **Binary files** — detected by sniffing the first 512 bytes for a NUL byte
  (the same trick `grep` uses). Avoids dumping garbage when you `grep` a
  directory that contains executables or images.
- **Permission-denied files** — silently skipped rather than spamming stderr.
- **`.git`, `node_modules`, `vendor`** — skipped by default; pass `-all` to
  include them.
