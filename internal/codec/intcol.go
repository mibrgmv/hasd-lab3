package codec

import (
	"fmt"
	"math/bits"
	"slices"
	"strconv"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/textval"
)

// Column blob layout, one bit stream per column:
//
//	kind:8 method:8 rle:8 <method parameters> <row codes>
//
// With rle every run of equal values is written once as <value code><run
// length - 1>; otherwise each row gets its own value code.
const (
	kindInt   = 1
	kindBytes = 2
)

const (
	methodConst = iota // one value for the whole column, no row codes
	methodPlain        // ints: value - min; bytes: length + raw bytes
	methodDelta        // ints: zigzag(value - previous value)
	methodDict         // index into a sorted dictionary of distinct values
)

var methodNames = [...]string{"const", "plain", "delta", "dict"}

// dictCap bounds the distinct values tracked per column; beyond it a
// dictionary would not pay off and the tables would eat too much memory.
const dictCap = 1 << 22

type intEntry struct {
	v            uint64
	count, heads uint32
}

// intColumn handles Int, Date and DateTime columns. Values are parsed into
// uint64 (two's complement for signed kinds, days or seconds for dates).
type intColumn struct {
	col    schema.Column
	signed bool

	// pass 1 statistics; a "head" is a row whose value differs from the
	// previous row, i.e. the start of a run
	rows, runs, raw  uint64
	bad              bool // non-canonical text seen: the column is stored as bytes
	started          bool
	first, prev      uint64
	min, max         uint64
	runLen           uint64
	runHist          [65]uint64 // width of run length - 1
	deltaAll, deltaH [65]uint64 // width of zigzag(value - previous), all rows / heads
	index            map[uint64]uint32
	entries          []intEntry
	overflow         bool

	// plan and pass 2
	method   byte
	rle      bool
	base     uint64
	constant uint64
	val, run uintCode
	w        bitWriter
	encPrev  uint64
	runVal   uint64
	runN     uint64
}

func newIntColumn(c schema.Column) *intColumn {
	return &intColumn{col: c, signed: c.Kind != schema.Int || c.Signed, index: make(map[uint64]uint32)}
}

func parseInt(c schema.Column, v []byte) (uint64, bool) {
	switch {
	case c.Kind == schema.Date:
		d, ok := textval.ParseDate(v)
		return uint64(d), ok
	case c.Kind == schema.DateTime:
		t, ok := textval.ParseDateTime(v)
		return uint64(t), ok
	case c.Signed:
		x, ok := textval.ParseInt(v)
		return uint64(x), ok
	}
	return textval.ParseUint(v)
}

func (c *intColumn) rawBytes() uint64 { return c.raw }

func (c *intColumn) observe(v []byte) {
	c.rows++
	c.raw += uint64(len(v)) + 1
	if c.bad {
		return
	}
	x, ok := parseInt(c.col, v)
	if !ok {
		c.bad = true
		c.index, c.entries = nil, nil
		return
	}
	if !c.started {
		c.started = true
		c.first, c.prev, c.min, c.max = x, x, x, x
	}
	head := c.runs == 0 || x != c.prev
	d := bits.Len64(zigzag(int64(x - c.prev)))
	c.deltaAll[d]++
	if head {
		c.deltaH[d]++
		if c.runs > 0 {
			c.runHist[bits.Len64(c.runLen-1)]++
		}
		c.runs++
		c.runLen = 0
	}
	c.runLen++
	c.prev = x
	if c.less(x, c.min) {
		c.min = x
	}
	if c.less(c.max, x) {
		c.max = x
	}

	if c.overflow {
		return
	}
	i, ok := c.index[x]
	if !ok {
		if len(c.entries) == dictCap {
			c.overflow = true
			c.index, c.entries = nil, nil
			return
		}
		i = uint32(len(c.entries))
		c.index[x] = i
		c.entries = append(c.entries, intEntry{v: x})
	}
	c.entries[i].count++
	if head {
		c.entries[i].heads++
	}
}

func (c *intColumn) less(a, b uint64) bool {
	if c.signed {
		return int64(a) < int64(b)
	}
	return a < b
}

type intCand struct {
	method byte
	rle    bool
	val    uintCode
	cost   uint64
}

// plan picks the cheapest encoding from the exact sizes implied by the pass-1
// statistics and writes the column header.
func (c *intColumn) plan() encoder {
	if c.bad {
		return newRawBytesColumn(c.col)
	}
	c.runHist[bits.Len64(c.runLen-1)]++
	rangeBits := bits.Len64(c.max - c.min)

	// widths of value - min, for all rows and for run heads
	var forAll, forH [65]uint64
	if c.overflow {
		forAll[rangeBits], forH[rangeBits] = c.rows, c.runs
	} else {
		for _, e := range c.entries {
			w := bits.Len64(e.v - c.min)
			forAll[w] += uint64(e.count)
			forH[w] += uint64(e.heads)
		}
	}

	// The dictionary is sorted; entry i is stored as the gap to entry i-1
	// minus one (entry 0 is the minimum itself).
	var gapHist [65]uint64
	if !c.overflow {
		slices.SortFunc(c.entries, func(a, b intEntry) int {
			return cmpU64(a.v-c.min, b.v-c.min)
		})
		for i := 1; i < len(c.entries); i++ {
			gapHist[bits.Len64(c.entries[i].v-c.entries[i-1].v-1)]++
		}
	}
	gapCode, gapCost := bestCode(&gapHist)

	runCode, runCost := bestCode(&c.runHist)
	var cands []intCand
	if !c.overflow && len(c.entries) == 1 {
		cands = append(cands, intCand{method: methodConst})
	}
	for _, rle := range []bool{false, true} {
		n, extra, hFor, hDelta := c.rows, uint64(0), &forAll, &c.deltaAll
		if rle {
			n, extra, hFor, hDelta = c.runs, runCost, &forH, &c.deltaH
		}
		code, cost := bestCode(hFor)
		cands = append(cands, intCand{methodPlain, rle, code, cost + extra})
		code, cost = bestCode(hDelta)
		cands = append(cands, intCand{methodDelta, rle, code, cost + extra})
		if !c.overflow {
			d := len(c.entries)
			w := bits.Len(uint(d - 1))
			cost := n*uint64(w) + gapCost
			cands = append(cands, intCand{methodDict, rle, newUintCode(uint8(w)), cost + extra})
		}
	}
	best := cands[0]
	for _, k := range cands[1:] {
		if k.cost < best.cost {
			best = k
		}
	}

	c.method, c.rle, c.val, c.run = best.method, best.rle, best.val, runCode
	c.base, c.encPrev = c.min, c.first
	w := &c.w
	w.write(kindInt, 8)
	w.write(uint64(c.method), 8)
	w.write(b2u(c.rle), 8)
	switch c.method {
	case methodConst:
		c.constant = c.first
		w.write(c.constant, 64)
	case methodPlain, methodDelta:
		w.write(c.base, 64)
		w.write(c.first, 64)
		c.val.save(w)
	case methodDict:
		w.write(c.base, 64)
		w.write(uint64(len(c.entries)), 32)
		gapCode.save(w)
		for i, e := range c.entries {
			if i > 0 {
				gapCode.write(w, e.v-c.entries[i-1].v-1)
			}
			c.index[e.v] = uint32(i)
		}
		c.val.save(w)
	}
	if c.rle {
		c.run.save(w)
	}
	c.entries = nil
	if c.method != methodDict {
		c.index = nil
	}
	return c
}

func (c *intColumn) encode(v []byte) {
	if c.method == methodConst {
		return
	}
	x, _ := parseInt(c.col, v)
	if !c.rle {
		c.emit(x)
		return
	}
	if c.runN > 0 && x == c.runVal {
		c.runN++
		return
	}
	c.flushRun()
	c.runVal, c.runN = x, 1
}

func (c *intColumn) flushRun() {
	if c.runN > 0 {
		c.emit(c.runVal)
		c.run.write(&c.w, c.runN-1)
	}
}

func (c *intColumn) emit(x uint64) {
	switch c.method {
	case methodPlain:
		c.val.write(&c.w, x-c.base)
	case methodDelta:
		c.val.write(&c.w, zigzag(int64(x-c.encPrev)))
		c.encPrev = x
	case methodDict:
		c.val.write(&c.w, uint64(c.index[x]))
	}
}

func (c *intColumn) finish() []byte {
	if c.rle {
		c.flushRun()
	}
	c.index = nil
	return c.w.bytes()
}

func (c *intColumn) describe() string {
	return describe(c.method, c.rle, c.val, c.run)
}

func describe(method byte, rle bool, val, run uintCode) string {
	s := methodNames[method]
	if method != methodConst {
		s += val.String()
	}
	if rle {
		s += " rle" + run.String()
	}
	return s
}

type intDecoder struct {
	r        *bitReader
	method   byte
	rle      bool
	base     uint64
	prev     uint64
	cur      uint64
	left     uint64
	val, run uintCode
	dict     []uint64
	format   func([]byte, uint64) []byte
}

func newIntDecoder(col schema.Column, r *bitReader) (*intDecoder, error) {
	d := &intDecoder{r: r, method: byte(r.read(8)), rle: r.read(8) == 1}
	switch {
	case col.Kind == schema.Date:
		d.format = func(b []byte, x uint64) []byte { return textval.AppendDate(b, int64(x)) }
	case col.Kind == schema.DateTime:
		d.format = func(b []byte, x uint64) []byte { return textval.AppendDateTime(b, int64(x)) }
	case col.Kind == schema.Int && col.Signed:
		d.format = func(b []byte, x uint64) []byte { return strconv.AppendInt(b, int64(x), 10) }
	case col.Kind == schema.Int:
		d.format = func(b []byte, x uint64) []byte { return strconv.AppendUint(b, x, 10) }
	default:
		return nil, fmt.Errorf("codec: column %s: integer blob for type %s", col.Name, col.Type)
	}
	switch d.method {
	case methodConst:
		d.cur = r.read(64)
		return d, nil
	case methodPlain, methodDelta:
		d.base = r.read(64)
		d.prev = r.read(64)
		d.val = loadUintCode(r)
	case methodDict:
		d.base = r.read(64)
		d.dict = make([]uint64, r.read(32))
		gap := loadUintCode(r)
		d.dict[0] = d.base
		for i := 1; i < len(d.dict); i++ {
			d.dict[i] = d.dict[i-1] + gap.read(r) + 1
		}
		d.val = loadUintCode(r)
	default:
		return nil, fmt.Errorf("codec: column %s: unknown method %d", col.Name, d.method)
	}
	if d.rle {
		d.run = loadUintCode(r)
	}
	return d, nil
}

func (d *intDecoder) next(dst []byte) []byte {
	switch {
	case d.method == methodConst:
	case !d.rle:
		d.cur = d.value()
	default:
		if d.left == 0 {
			d.cur = d.value()
			d.left = d.run.read(d.r) + 1
		}
		d.left--
	}
	return d.format(dst, d.cur)
}

func (d *intDecoder) value() uint64 {
	v := d.val.read(d.r)
	switch d.method {
	case methodPlain:
		return d.base + v
	case methodDelta:
		d.prev += uint64(unzigzag(v))
		return d.prev
	}
	return d.dict[v]
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
