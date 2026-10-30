package codec

import (
	"bytes"
	"fmt"
	"math/bits"
	"slices"

	"hasd/lab3/internal/schema"
)

type strEntry struct{ count, heads uint32 }

// bytesColumn handles String, FixedString and Array columns (and integer
// columns with non-canonical text). Values are kept exactly as they appear in
// the TSV, escapes included, so reproducing them needs no unescaping.
type bytesColumn struct {
	col schema.Column

	// pass 1 statistics
	rows, runs, raw uint64
	started         bool
	prev            []byte
	runLen          uint64
	runHist         [65]uint64 // width of run length - 1
	lenAll, lenH    [65]uint64 // width of value length, all rows / run heads
	sumAll, sumH    uint64     // total value bytes, all rows / run heads
	index           map[string]uint32
	entries         []strEntry
	overflow        bool

	// plan and pass 2
	method        byte
	rle           bool
	val, run, len uintCode // val: dictionary index; len: value or suffix length
	lcp           uintCode // dictionary: bytes shared with the previous entry
	w             bitWriter
	runVal        []byte
	runN          uint64
}

func newBytesColumn(c schema.Column) *bytesColumn {
	return &bytesColumn{col: c, index: make(map[string]uint32)}
}

// newRawBytesColumn is the fallback that needs no statistics: plain bytes
// with a generic length code.
func newRawBytesColumn(c schema.Column) *bytesColumn {
	b := &bytesColumn{col: c, method: methodPlain, len: newUintCode(4, 8, 16, 32)}
	b.writeHeader(nil)
	return b
}

func (c *bytesColumn) rawBytes() uint64 { return c.raw }

func (c *bytesColumn) observe(v []byte) {
	c.rows++
	c.raw += uint64(len(v)) + 1
	head := !c.started || !bytes.Equal(v, c.prev)
	if head {
		if c.started {
			c.runHist[bits.Len64(c.runLen-1)]++
		}
		c.started = true
		c.runs++
		c.runLen = 0
		c.prev = append(c.prev[:0], v...)
	}
	c.runLen++
	lw := bits.Len(uint(len(v)))
	c.lenAll[lw]++
	c.sumAll += uint64(len(v))
	if head {
		c.lenH[lw]++
		c.sumH += uint64(len(v))
	}

	if c.overflow {
		return
	}
	i, ok := c.index[string(v)]
	if !ok {
		if len(c.entries) == dictCap {
			c.overflow = true
			c.index, c.entries = nil, nil
			return
		}
		i = uint32(len(c.entries))
		c.index[string(v)] = i
		c.entries = append(c.entries, strEntry{})
	}
	c.entries[i].count++
	if head {
		c.entries[i].heads++
	}
}

type bytesCand struct {
	method   byte
	rle      bool
	val, len uintCode
	cost     uint64
}

func (c *bytesColumn) plan() encoder {
	c.runHist[bits.Len64(c.runLen-1)]++
	runCode, runCost := bestCode(&c.runHist)

	// The dictionary is sorted and front coded: every entry is stored as the
	// length of the prefix shared with the previous entry plus the suffix.
	d := len(c.entries)
	var keys []string
	var lcpHist, sufHist [65]uint64
	var sufBytes uint64
	if !c.overflow {
		keys = make([]string, 0, d)
		for k := range c.index {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		prev := ""
		for _, k := range keys {
			l := commonPrefix(prev, k)
			lcpHist[bits.Len(uint(l))]++
			sufHist[bits.Len(uint(len(k)-l))]++
			sufBytes += uint64(len(k) - l)
			prev = k
		}
	}
	lcpCode, lcpCost := bestCode(&lcpHist)
	sufCode, sufCost := bestCode(&sufHist)
	dictCost := lcpCost + sufCost + 8*sufBytes

	var cands []bytesCand
	if !c.overflow && d == 1 {
		cands = append(cands, bytesCand{method: methodConst})
	}
	for _, rle := range []bool{false, true} {
		n, extra, hLen, sum := c.rows, uint64(0), &c.lenAll, c.sumAll
		if rle {
			n, extra, hLen, sum = c.runs, runCost, &c.lenH, c.sumH
		}
		code, cost := bestCode(hLen)
		cands = append(cands, bytesCand{methodPlain, rle, uintCode{}, code, cost + 8*sum + extra})
		if !c.overflow {
			w := bits.Len(uint(d - 1))
			cost := n*uint64(w) + dictCost
			cands = append(cands, bytesCand{methodDict, rle, newUintCode(uint8(w)), sufCode, cost + extra})
		}
	}
	best := cands[0]
	for _, k := range cands[1:] {
		if k.cost < best.cost {
			best = k
		}
	}

	c.method, c.rle, c.val, c.len, c.lcp, c.run = best.method, best.rle, best.val, best.len, lcpCode, runCode
	c.writeHeader(keys)
	c.entries, c.prev = nil, nil
	if c.method != methodDict {
		c.index = nil
	}
	return c
}

// writeHeader writes the column parameters; for a dictionary, keys are the
// sorted distinct values and the index is rewritten to point into them.
func (c *bytesColumn) writeHeader(keys []string) {
	w := &c.w
	w.write(kindBytes, 8)
	w.write(uint64(c.method), 8)
	w.write(b2u(c.rle), 8)
	switch c.method {
	case methodConst:
		w.write(uint64(len(keys[0])), 32)
		w.writeBytes([]byte(keys[0]))
	case methodPlain:
		c.len.save(w)
	case methodDict:
		c.val.save(w)
		w.write(uint64(len(keys)), 32)
		c.lcp.save(w)
		c.len.save(w)
		prev := ""
		for _, k := range keys {
			l := commonPrefix(prev, k)
			c.lcp.write(w, uint64(l))
			c.len.write(w, uint64(len(k)-l))
			prev = k
		}
		w.align()
		prev = ""
		for i, k := range keys {
			w.writeBytes([]byte(k[commonPrefix(prev, k):]))
			c.index[k] = uint32(i)
			prev = k
		}
	}
	if c.rle {
		c.run.save(w)
	}
}

func (c *bytesColumn) encode(v []byte) {
	if c.method == methodConst {
		return
	}
	if !c.rle {
		c.emit(v)
		return
	}
	if c.runN > 0 && bytes.Equal(v, c.runVal) {
		c.runN++
		return
	}
	c.flushRun()
	c.runVal = append(c.runVal[:0], v...)
	c.runN = 1
}

func (c *bytesColumn) flushRun() {
	if c.runN > 0 {
		c.emit(c.runVal)
		c.run.write(&c.w, c.runN-1)
	}
}

func (c *bytesColumn) emit(v []byte) {
	if c.method == methodDict {
		c.val.write(&c.w, uint64(c.index[string(v)]))
		return
	}
	c.len.write(&c.w, uint64(len(v)))
	c.w.writeBytes(v)
}

func (c *bytesColumn) finish() []byte {
	if c.rle {
		c.flushRun()
	}
	c.index = nil
	return c.w.bytes()
}

func (c *bytesColumn) describe() string {
	code := c.val
	if c.method == methodPlain {
		code = c.len
	}
	return describe(c.method, c.rle, code, c.run)
}

type bytesDecoder struct {
	r             *bitReader
	method        byte
	rle           bool
	val, run, len uintCode
	dict          [][]byte
	cur           []byte // current value: a dictionary entry or buf
	buf           []byte
	left          uint64
}

func newBytesDecoder(col schema.Column, r *bitReader) (*bytesDecoder, error) {
	d := &bytesDecoder{r: r, method: byte(r.read(8)), rle: r.read(8) == 1}
	switch d.method {
	case methodConst:
		d.cur = r.appendBytes(nil, int(r.read(32)))
		return d, nil
	case methodPlain:
		d.len = loadUintCode(r)
	case methodDict:
		d.val = loadUintCode(r)
		d.dict = make([][]byte, r.read(32))
		lcpCode, sufCode := loadUintCode(r), loadUintCode(r)
		lcps, sufs := make([]int, len(d.dict)), make([]int, len(d.dict))
		total, full := 0, 0
		for i := range lcps {
			lcps[i], sufs[i] = int(lcpCode.read(r)), int(sufCode.read(r))
			total += sufs[i]
			full += lcps[i] + sufs[i]
		}
		suffixes := r.alignedBytes(total)
		arena := make([]byte, 0, full)
		var prev []byte
		for i := range d.dict {
			start := len(arena)
			arena = append(arena, prev[:lcps[i]]...)
			arena = append(arena, suffixes[:sufs[i]]...)
			suffixes = suffixes[sufs[i]:]
			d.dict[i] = arena[start:len(arena):len(arena)]
			prev = d.dict[i]
		}
	default:
		return nil, fmt.Errorf("codec: column %s: unknown method %d", col.Name, d.method)
	}
	if d.rle {
		d.run = loadUintCode(r)
	}
	return d, nil
}

func (d *bytesDecoder) next(dst []byte) []byte {
	switch {
	case d.method == methodConst:
	case !d.rle:
		d.value()
	default:
		if d.left == 0 {
			d.value()
			d.left = d.run.read(d.r) + 1
		}
		d.left--
	}
	return append(dst, d.cur...)
}

func (d *bytesDecoder) value() {
	if d.method == methodDict {
		d.cur = d.dict[d.val.read(d.r)]
		return
	}
	d.buf = d.r.appendBytes(d.buf[:0], int(d.len.read(d.r)))
	d.cur = d.buf
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
