// Package codec implements the columnar binary format for the dataset.
//
// File layout (integers little-endian):
//
//	"HBC1" rows:u64 ncols:u32
//	ncols × { name:u16-prefixed, type:u16-prefixed, blobLen:u64 }
//	ncols × blob
//
// Every blob is one column encoded independently of the others (see
// intcol.go and bytescol.go). A value is decoded from its own code, the
// previous value of the same column and the column header (dictionary,
// code widths).
package codec

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/tsv"
)

const magic = "HBC1"

type collector interface {
	observe(v []byte)
	plan() encoder
	rawBytes() uint64
}

type encoder interface {
	encode(v []byte)
	finish() []byte
	describe() string
}

type decoder interface {
	next(dst []byte) []byte
}

type Options struct {
	Workers   int
	ChunkSize int // bytes read per batch
}

type ColumnReport struct {
	Column   schema.Column
	Codec    string
	RawBytes uint64 // bytes in the TSV, separator included
	EncBytes uint64
}

// Encode reads the TSV twice: pass 1 collects statistics and dictionaries,
// then every column picks its encoding, pass 2 writes the row codes.
func Encode(in io.ReadSeeker, cols []schema.Column, out io.Writer, opt Options) (uint64, []ColumnReport, error) {
	stats := make([]collector, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case schema.Int, schema.Date, schema.DateTime:
			stats[i] = newIntColumn(c)
		default:
			stats[i] = newBytesColumn(c)
		}
	}
	rows, _, err := tsv.ScanColumns(in, len(cols), opt.Workers, opt.ChunkSize, func(c int, v []byte) {
		stats[c].observe(v)
	})
	if err != nil {
		return 0, nil, err
	}

	reps := make([]ColumnReport, len(cols))
	encs := make([]encoder, len(cols))
	parallel(len(cols), opt.Workers, func(i int) {
		reps[i] = ColumnReport{Column: cols[i], RawBytes: stats[i].rawBytes()}
		encs[i] = stats[i].plan()
		stats[i] = nil
	})

	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return 0, nil, err
	}
	rows2, _, err := tsv.ScanColumns(in, len(cols), opt.Workers, opt.ChunkSize, func(c int, v []byte) {
		encs[c].encode(v)
	})
	if err != nil {
		return 0, nil, err
	}
	if rows2 != rows {
		return 0, nil, fmt.Errorf("codec: input changed between passes (%d rows, then %d)", rows, rows2)
	}

	blobs := make([][]byte, len(cols))
	parallel(len(cols), opt.Workers, func(i int) {
		blobs[i] = encs[i].finish()
		reps[i].Codec = encs[i].describe()
		reps[i].EncBytes = uint64(len(blobs[i]))
		encs[i] = nil
	})

	bw := bufio.NewWriterSize(out, 1<<20)
	h := []byte(magic)
	h = binary.LittleEndian.AppendUint64(h, rows)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(cols)))
	for i, c := range cols {
		h = appendStr(h, c.Name)
		h = appendStr(h, c.Type)
		h = binary.LittleEndian.AppendUint64(h, uint64(len(blobs[i])))
	}
	bw.Write(h)
	for _, b := range blobs {
		bw.Write(b)
	}
	return rows, reps, bw.Flush()
}

// Decode writes the TSV reconstructed from an encoded file. Columns are
// decoded concurrently in chunks of rows and stitched back into lines.
func Decode(data []byte, out io.Writer) (uint64, error) {
	if len(data) < 16 || string(data[:4]) != magic {
		return 0, errors.New("codec: not an HBC1 file")
	}
	rows := binary.LittleEndian.Uint64(data[4:])
	ncols := int(binary.LittleEndian.Uint32(data[12:]))
	p := 16
	cols := make([]schema.Column, ncols)
	sizes := make([]uint64, ncols)
	for i := range cols {
		var name, typ string
		var err error
		if name, p, err = readStr(data, p); err != nil {
			return 0, err
		}
		if typ, p, err = readStr(data, p); err != nil {
			return 0, err
		}
		if cols[i], err = schema.ParseColumn(i, name, typ); err != nil {
			return 0, err
		}
		if p+8 > len(data) {
			return 0, errors.New("codec: truncated header")
		}
		sizes[i] = binary.LittleEndian.Uint64(data[p:])
		p += 8
	}
	decs := make([]decoder, ncols)
	for i, c := range cols {
		if uint64(len(data)-p) < sizes[i] {
			return 0, errors.New("codec: truncated column data")
		}
		r := &bitReader{buf: data[p : p+int(sizes[i])]}
		p += int(sizes[i])
		var err error
		switch r.read(8) {
		case kindInt:
			decs[i], err = newIntDecoder(c, r)
		case kindBytes:
			decs[i], err = newBytesDecoder(c, r)
		default:
			err = fmt.Errorf("codec: column %s: unknown blob kind", c.Name)
		}
		if err != nil {
			return 0, err
		}
	}

	const chunkRows = 1 << 16
	type chunk struct {
		buf  []byte
		ends []uint32
	}
	outs := make([]chan *chunk, ncols)
	frees := make([]chan *chunk, ncols)
	for c := range ncols {
		outs[c] = make(chan *chunk, 2)
		frees[c] = make(chan *chunk, 3)
		for range 3 {
			frees[c] <- new(chunk)
		}
		go func(d decoder, out, free chan *chunk) {
			for start := uint64(0); start < rows; start += chunkRows {
				ch := <-free
				ch.buf, ch.ends = ch.buf[:0], ch.ends[:0]
				for range min(chunkRows, rows-start) {
					ch.buf = d.next(ch.buf)
					ch.ends = append(ch.ends, uint32(len(ch.buf)))
				}
				out <- ch
			}
		}(decs[c], outs[c], frees[c])
	}

	chs := make([]*chunk, ncols)
	var buf []byte
	for start := uint64(0); start < rows; start += chunkRows {
		for c := range chs {
			chs[c] = <-outs[c]
		}
		buf = buf[:0]
		for r := range int(min(chunkRows, rows-start)) {
			for c, ch := range chs {
				s := uint32(0)
				if r > 0 {
					s = ch.ends[r-1]
				}
				buf = append(buf, ch.buf[s:ch.ends[r]]...)
				if c == ncols-1 {
					buf = append(buf, '\n')
				} else {
					buf = append(buf, '\t')
				}
			}
		}
		if _, err := out.Write(buf); err != nil {
			return 0, err
		}
		for c, ch := range chs {
			frees[c] <- ch
		}
	}
	return rows, nil
}

func parallel(n, workers int, fn func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

func appendStr(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func readStr(data []byte, p int) (string, int, error) {
	if p+2 > len(data) {
		return "", 0, errors.New("codec: truncated header")
	}
	n := int(binary.LittleEndian.Uint16(data[p:]))
	p += 2
	if p+n > len(data) {
		return "", 0, errors.New("codec: truncated header")
	}
	return string(data[p : p+n]), p + n, nil
}
