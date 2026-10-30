package codec

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"

	"hasd/lab3/internal/cm"
	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/tsv"
)

const magic = "HBC2"

type collector interface {
	observe(v []byte)
	plan() encoder
	rawBytes() uint64
}

type encoder interface {
	encode(v []byte)
	finish() ([]byte, []*stream)
	describe() string
}

type decoder interface {
	next(dst []byte) []byte
}

type Options struct {
	Workers   int
	ChunkSize int
}

type ColumnReport struct {
	Column      schema.Column
	Codec       string
	RawBytes    uint64
	StreamBytes uint64
	EncBytes    uint64
}

type segment struct {
	col   int
	width int
	raw   []byte
	comp  []byte
}

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

	hdrs := make([][]byte, len(cols))
	segs := make([][]*segment, len(cols))
	var all []*segment
	for i, e := range encs {
		var streams []*stream
		hdrs[i], streams = e.finish()
		reps[i].Codec = e.describe()
		reps[i].StreamBytes = uint64(len(hdrs[i]))
		for _, s := range streams {
			sg := &segment{col: i, width: s.width, raw: s.buf}
			segs[i] = append(segs[i], sg)
			all = append(all, sg)
			reps[i].StreamBytes += uint64(len(s.buf))
		}
		encs[i] = nil
	}
	slices.SortFunc(all, func(a, b *segment) int { return len(b.raw) - len(a.raw) })
	parallel(len(all), opt.Workers, func(i int) {
		all[i].comp = cm.Compress(all[i].raw, all[i].width)
	})

	blobs := make([][]byte, len(cols))
	for i := range cols {
		b := binary.AppendUvarint(nil, uint64(len(hdrs[i])))
		b = append(b, hdrs[i]...)
		b = append(b, byte(len(segs[i])))
		for _, sg := range segs[i] {
			b = binary.AppendUvarint(b, uint64(len(sg.raw)))
			b = append(b, byte(sg.width))
			b = binary.AppendUvarint(b, uint64(len(sg.comp)))
			b = append(b, sg.comp...)
		}
		blobs[i] = b
		reps[i].EncBytes = uint64(len(b))
	}

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

type parsedColumn struct {
	hdr  header
	segs []*segment
}

func parseBlob(col int, b []byte) (*parsedColumn, error) {
	bad := fmt.Errorf("codec: column %d: corrupt blob", col)
	n, k := binary.Uvarint(b)
	if k <= 0 || uint64(len(b)-k) < n+1 {
		return nil, bad
	}
	pc := &parsedColumn{hdr: header{buf: b[k : k+int(n)]}}
	b = b[k+int(n):]
	nseg := int(b[0])
	b = b[1:]
	for range nseg {
		raw, k := binary.Uvarint(b)
		if k <= 0 || len(b) < k+1 {
			return nil, bad
		}
		width := int(b[k])
		b = b[k+1:]
		size, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < size {
			return nil, bad
		}
		pc.segs = append(pc.segs, &segment{col: col, width: width, raw: make([]byte, 0, raw), comp: b[k : k+int(size)]})
		b = b[k+int(size):]
	}
	return pc, nil
}

func newDecoder(c schema.Column, pc *parsedColumn) (dec decoder, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("codec: column %s: %v", c.Name, r)
		}
	}()
	readers := make([]*reader, len(pc.segs))
	for i, sg := range pc.segs {
		readers[i] = &reader{buf: sg.raw, width: sg.width}
	}
	switch pc.hdr.get(1) {
	case kindInt:
		return newIntDecoder(c, &pc.hdr, readers)
	case kindBytes:
		return newBytesDecoder(c, &pc.hdr, readers)
	}
	return nil, errors.New("unknown blob kind")
}

func Decode(data []byte, out io.Writer, workers int) (uint64, error) {
	if len(data) < 16 || string(data[:4]) != magic {
		return 0, errors.New("codec: not an " + magic + " file")
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

	parsed := make([]*parsedColumn, ncols)
	var all []*segment
	for i := range cols {
		if uint64(len(data)-p) < sizes[i] {
			return 0, errors.New("codec: truncated column data")
		}
		pc, err := parseBlob(i, data[p:p+int(sizes[i])])
		if err != nil {
			return 0, err
		}
		p += int(sizes[i])
		parsed[i] = pc
		all = append(all, pc.segs...)
	}
	slices.SortFunc(all, func(a, b *segment) int { return cap(b.raw) - cap(a.raw) })
	errs := make([]error, len(all))
	parallel(len(all), workers, func(i int) {
		all[i].raw, errs[i] = cm.Decompress(all[i].comp, cap(all[i].raw), all[i].width)
	})
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}

	decs := make([]decoder, ncols)
	for i, c := range cols {
		var err error
		if decs[i], err = newDecoder(c, parsed[i]); err != nil {
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
