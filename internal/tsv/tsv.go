// Package tsv cuts a TabSeparated stream into batches of whole rows and
// records field offsets. Field contents are neither copied nor unescaped:
// ClickHouse escapes tabs and newlines inside values, so every raw '\t' and
// '\n' is a separator.
package tsv

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ScanColumns reads all rows and calls fn(col, value) for every field. Each
// column is owned by one worker, so calls for one column are sequential and
// in row order, while different columns are processed concurrently.
func ScanColumns(r io.Reader, ncols, workers, chunkSize int, fn func(col int, v []byte)) (rows, size uint64, err error) {
	type job struct {
		b    Batch
		refs atomic.Int32
	}
	const inFlight = 4
	free := make(chan *job, inFlight)
	for range inFlight {
		free <- new(job)
	}

	var wg sync.WaitGroup
	chans := make([]chan *job, workers)
	for w := range chans {
		var mine []int
		for c := w; c < ncols; c += workers {
			mine = append(mine, c)
		}
		chans[w] = make(chan *job, inFlight)
		wg.Add(1)
		go func(ch chan *job) {
			defer wg.Done()
			for j := range ch {
				b := &j.b
				for r := 0; r < b.Rows; r++ {
					for _, c := range mine {
						fn(c, b.Field(r, c))
					}
				}
				if j.refs.Add(-1) == 0 {
					free <- j
				}
			}
		}(chans[w])
	}
	defer func() {
		for _, ch := range chans {
			close(ch)
		}
		wg.Wait()
	}()

	rd := NewReader(r, ncols, chunkSize)
	for {
		j := <-free
		ok, err := rd.Next(&j.b)
		if err != nil || !ok {
			return rows, size, err
		}
		rows += uint64(j.b.Rows)
		size += uint64(len(j.b.Buf))
		j.refs.Store(int32(workers))
		for _, ch := range chans {
			ch <- j
		}
	}
}

// Batch holds whole rows of the input. For every row, Off stores NCols+1
// offsets into Buf: the start of each field, then the end of the row + 1.
type Batch struct {
	Buf   []byte
	Off   []uint32
	Rows  int
	NCols int
}

// Field returns the raw (still escaped) bytes of a field.
func (b *Batch) Field(row, col int) []byte {
	k := row*(b.NCols+1) + col
	return b.Buf[b.Off[k] : b.Off[k+1]-1]
}

type Reader struct {
	r     io.Reader
	ncols int
	size  int
	carry []byte // incomplete row left over from the previous chunk
	eof   bool
	rows  int // rows returned so far, for error messages
}

// NewReader reads rows of ncols fields in chunks of chunkSize bytes; no row
// may be longer than a chunk.
func NewReader(r io.Reader, ncols, chunkSize int) *Reader {
	return &Reader{r: r, ncols: ncols, size: chunkSize}
}

// Next refills b with the next batch of rows. It returns false once the input
// is exhausted.
func (rd *Reader) Next(b *Batch) (bool, error) {
	if cap(b.Buf) < rd.size {
		b.Buf = make([]byte, 0, rd.size)
	}
	buf := append(b.Buf[:0], rd.carry...)
	for !rd.eof && len(buf) < cap(buf) {
		n, err := rd.r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			rd.eof = true
		} else if err != nil {
			return false, err
		}
	}
	end := bytes.LastIndexByte(buf, '\n') + 1
	switch {
	case len(buf) == 0:
		return false, nil
	case rd.eof && end < len(buf):
		return false, fmt.Errorf("tsv: missing newline at end of input")
	case end == 0:
		return false, fmt.Errorf("tsv: row %d is longer than the %d-byte chunk", rd.rows+1, rd.size)
	}
	rd.carry = append(rd.carry[:0], buf[end:]...)
	b.Buf = buf[:end]
	if err := b.split(rd.ncols, rd.rows); err != nil {
		return false, err
	}
	rd.rows += b.Rows
	return true, nil
}

func (b *Batch) split(ncols, firstRow int) error {
	b.NCols = ncols
	off := append(b.Off[:0], 0)
	rows, col := 0, 0
	for i, c := range b.Buf {
		switch c {
		case '\t':
			col++
			off = append(off, uint32(i+1))
		case '\n':
			if col != ncols-1 {
				return fmt.Errorf("tsv: row %d has %d fields, want %d", firstRow+rows+1, col+1, ncols)
			}
			off = append(off, uint32(i+1))
			rows++
			col = 0
			if i+1 < len(b.Buf) {
				off = append(off, uint32(i+1))
			}
		}
	}
	b.Off, b.Rows = off, rows
	return nil
}
