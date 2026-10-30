// Command profile reads the dataset once and prints per-column statistics
// that drive the choice of encodings: cardinality, repeats, value and delta
// widths, string lengths, escapes and rough size estimates.
//
//	go run ./cmd/profile data/hits_v1.tsv > reports/profile.txt
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"time"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/tsv"
)

func main() {
	schemaPath := flag.String("schema", "schema.txt", "column list, one \"<name> <ClickHouse type>\" per line")
	chunkMB := flag.Int("chunk", 32, "read chunk size in MiB")
	workers := flag.Int("workers", max(runtime.NumCPU()-1, 1), "worker goroutines")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: profile [flags] <file.tsv | ->")
	}
	if *workers < 1 || *chunkMB < 1 || *chunkMB > 1024 {
		log.Fatal("-workers must be at least 1, -chunk between 1 and 1024 MiB")
	}

	cols, err := schema.Load(*schemaPath)
	if err != nil {
		log.Fatal(err)
	}
	var in io.Reader = os.Stdin
	if flag.Arg(0) != "-" {
		f, err := os.Open(flag.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		in = f
	}

	stats := make([]*colStats, len(cols))
	for i, c := range cols {
		stats[i] = newColStats(c)
	}

	start := time.Now()
	rows, size, err := tsv.ScanColumns(in, len(cols), *workers, *chunkMB<<20, func(c int, v []byte) {
		stats[c].add(v)
	})
	if err != nil {
		log.Fatal(err)
	}
	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "profiled %d rows, %.2f GB in %v (%.0f MB/s)\n",
		rows, float64(size)/1e9, elapsed.Round(time.Millisecond), float64(size)/1e6/elapsed.Seconds())

	report(os.Stdout, stats, rows, size, elapsed)
}
