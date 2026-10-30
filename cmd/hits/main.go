// Command hits encodes the TSV dataset into the HBC1 columnar format and
// decodes it back.
//
//	hits encode [-schema schema.txt] -o data/hits_v1.hbc data/hits_v1.tsv
//	hits decode data/hits_v1.hbc | shasum -a 256
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"slices"
	"time"

	"hasd/lab3/internal/codec"
	"hasd/lab3/internal/schema"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: hits encode|decode [flags] <input>")
	}
	switch os.Args[1] {
	case "encode":
		encode(os.Args[2:])
	case "decode":
		decode(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func encode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	schemaPath := fs.String("schema", "schema.txt", "column list, one \"<name> <ClickHouse type>\" per line")
	outPath := fs.String("o", "", "output file (required)")
	workers := fs.Int("workers", max(runtime.NumCPU()-1, 1), "worker goroutines")
	chunkMB := fs.Int("chunk", 32, "read chunk size in MiB")
	fs.Parse(args)
	if fs.NArg() != 1 || *outPath == "" {
		log.Fatal("usage: hits encode [-schema schema.txt] -o out.hbc in.tsv")
	}
	checkFlags(*workers, *chunkMB)

	cols, err := schema.Load(*schemaPath)
	if err != nil {
		log.Fatal(err)
	}
	in, err := os.Open(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer in.Close()
	if outInfo, err := os.Stat(*outPath); err == nil {
		if inInfo, err := in.Stat(); err == nil && os.SameFile(inInfo, outInfo) {
			log.Fatal("output file is the input file")
		}
	}
	out, err := os.Create(*outPath)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	rows, reps, err := codec.Encode(in, cols, out, codec.Options{Workers: *workers, ChunkSize: *chunkMB << 20})
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		out.Close()
		os.Remove(*outPath)
		log.Fatal(err)
	}
	elapsed := time.Since(start)
	inInfo, _ := in.Stat()
	outInfo, _ := os.Stat(*outPath)
	report(os.Stdout, rows, reps, inInfo.Size(), outInfo.Size(), elapsed)
}

// checkFlags rejects values that would hang the pipeline (no workers) or
// overflow the uint32 field offsets of a batch (chunks of 4 GiB and more).
func checkFlags(workers, chunkMB int) {
	if workers < 1 {
		log.Fatal("-workers must be at least 1")
	}
	if chunkMB < 1 || chunkMB > 1024 {
		log.Fatal("-chunk must be between 1 and 1024 MiB")
	}
}

func report(w io.Writer, rows uint64, reps []codec.ColumnReport, inSize, outSize int64, elapsed time.Duration) {
	slices.SortStableFunc(reps, func(a, b codec.ColumnReport) int { return int(b.EncBytes) - int(a.EncBytes) })
	fmt.Fprintf(w, "%3s  %-27s %-16s %9s %9s %7s %8s  %s\n", "#", "column", "type", "TSV MB", "enc MB", "enc %", "bit/row", "codec")
	for _, r := range reps {
		fmt.Fprintf(w, "%3d  %-27s %-16s %9.1f %9.3f %6.2f%% %8.3f  %s\n",
			r.Column.Index+1, r.Column.Name, r.Column.Type, float64(r.RawBytes)/1e6, float64(r.EncBytes)/1e6,
			100*float64(r.EncBytes)/float64(r.RawBytes), 8*float64(r.EncBytes)/float64(rows), r.Codec)
	}
	fmt.Fprintf(w, "\nrows %d: %.1f MB -> %.1f MB (%.2f%%, %.2fx) in %v, %.0f MB/s\n",
		rows, float64(inSize)/1e6, float64(outSize)/1e6, 100*float64(outSize)/float64(inSize),
		float64(inSize)/float64(outSize), elapsed.Round(time.Millisecond), float64(inSize)/1e6/elapsed.Seconds())
}

func decode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	outPath := fs.String("o", "-", "output file, - for stdout")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatal("usage: hits decode [-o out.tsv] in.hbc")
	}

	start := time.Now()
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	out := os.Stdout
	if *outPath != "-" {
		if out, err = os.Create(*outPath); err != nil {
			log.Fatal(err)
		}
	}
	bw := bufio.NewWriterSize(out, 4<<20)
	rows, err := codec.Decode(data, bw)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "decoded %d rows in %v\n", rows, time.Since(start).Round(time.Millisecond))
}
