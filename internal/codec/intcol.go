package codec

import (
	"fmt"
	"math/bits"
	"strconv"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/textval"
)

const (
	kindInt   = 1
	kindBytes = 2
)

const (
	methodConst = iota
	methodPlain
	methodDelta
	methodDict
)

var methodNames = [...]string{"const", "plain", "delta", "dict"}

const dictCap = 1 << 22

type intColumn struct {
	col    schema.Column
	signed bool

	rows, runs, raw  uint64
	bad              bool
	started          bool
	first, prev      uint64
	min, max         uint64
	runLen           uint64
	runHist          [65]uint64
	deltaAll, deltaH [65]uint64
	index            map[uint64]uint32
	entries          []uint64
	overflow         bool

	method    byte
	rle       bool
	base      uint64
	h         header
	dict      stream
	vals      stream
	runStream stream
	encPrev   uint64
	fresh     uint32
	runVal    uint64
	runN      uint64
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
	if _, ok := c.index[x]; !ok {
		if len(c.entries) == dictCap {
			c.overflow = true
			c.index, c.entries = nil, nil
			return
		}
		c.index[x] = uint32(len(c.entries))
		c.entries = append(c.entries, x)
	}
}

func (c *intColumn) less(a, b uint64) bool {
	if c.signed {
		return int64(a) < int64(b)
	}
	return a < b
}

type candidate struct {
	method byte
	rle    bool
	width  int
	cost   uint64
}

func cheapest(cands []candidate) candidate {
	best := cands[0]
	for _, k := range cands[1:] {
		if k.cost < best.cost {
			best = k
		}
	}
	return best
}

func (c *intColumn) plan() encoder {
	if c.bad {
		return newRawBytesColumn(c.col)
	}
	c.runHist[bits.Len64(c.runLen-1)]++
	runW := widthOf(maxBits(&c.runHist))
	rangeW := widthFor(c.max - c.min)

	var cands []candidate
	if !c.overflow && len(c.entries) == 1 {
		cands = append(cands, candidate{method: methodConst})
	}
	for _, rle := range []bool{false, true} {
		n, extra, hDelta := c.rows, uint64(0), &c.deltaAll
		if rle {
			n, extra, hDelta = c.runs, c.runs*uint64(runW), &c.deltaH
		}
		dw := widthOf(maxBits(hDelta))
		cands = append(cands, candidate{methodDelta, rle, dw, n*uint64(dw) + extra})
		if !c.overflow {
			d := uint64(len(c.entries))
			w := widthFor(d)
			cands = append(cands, candidate{methodDict, rle, w, n*uint64(w) + d*uint64(rangeW) + extra})
		}
		cands = append(cands, candidate{methodPlain, rle, rangeW, n*uint64(rangeW) + extra})
	}
	best := cheapest(cands)

	c.method, c.rle, c.base, c.encPrev = best.method, best.rle, c.min, c.first
	c.vals.width, c.runStream.width = best.width, runW
	c.h.put(kindInt, 1)
	c.h.put(uint64(c.method), 1)
	c.h.put(b2u(c.rle), 1)
	switch c.method {
	case methodConst:
		c.h.put(c.first, 8)
	case methodPlain, methodDelta:
		c.h.put(c.base, 8)
		c.h.put(c.first, 8)
	case methodDict:
		c.h.put(c.base, 8)
		c.dict.width = rangeW
		for _, v := range c.entries {
			c.dict.put(v - c.base)
		}
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
		c.runStream.put(c.runN - 1)
	}
}

func (c *intColumn) emit(x uint64) {
	switch c.method {
	case methodPlain:
		c.vals.put(x - c.base)
	case methodDelta:
		c.vals.put(zigzag(int64(x - c.encPrev)))
		c.encPrev = x
	case methodDict:
		c.vals.put(dictCode(c.index[x], &c.fresh))
	}
}

func dictCode(id uint32, fresh *uint32) uint64 {
	if id == *fresh {
		*fresh++
		return 0
	}
	return uint64(*fresh - id)
}

func dictID(code uint64, fresh *uint32) uint32 {
	if code == 0 {
		*fresh++
		return *fresh - 1
	}
	return *fresh - uint32(code)
}

func (c *intColumn) finish() ([]byte, []*stream) {
	if c.rle {
		c.flushRun()
	}
	c.index = nil
	return c.h.buf, columnStreams(c.method, c.rle, &c.dict, &c.vals, &c.runStream)
}

func columnStreams(method byte, rle bool, dict, vals, runs *stream) []*stream {
	var s []*stream
	if method == methodDict {
		s = append(s, dict)
	}
	if method != methodConst {
		s = append(s, vals)
	}
	if rle {
		s = append(s, runs)
	}
	return s
}

func (c *intColumn) describe() string {
	return describe(c.method, c.rle, c.vals.width, c.runStream.width)
}

func describe(method byte, rle bool, width, runWidth int) string {
	s := methodNames[method]
	if method != methodConst {
		s += "[" + strconv.Itoa(width) + "]"
	}
	if rle {
		s += " rle[" + strconv.Itoa(runWidth) + "]"
	}
	return s
}

type intDecoder struct {
	method byte
	rle    bool
	base   uint64
	prev   uint64
	cur    uint64
	left   uint64
	fresh  uint32
	vals   *reader
	runs   *reader
	dict   []uint64
	format func([]byte, uint64) []byte
}

func newIntDecoder(col schema.Column, h *header, segs []*reader) (*intDecoder, error) {
	d := &intDecoder{method: byte(h.get(1)), rle: h.get(1) == 1}
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
		d.cur = h.get(8)
	case methodPlain, methodDelta:
		d.base = h.get(8)
		d.prev = h.get(8)
	case methodDict:
		d.base = h.get(8)
	default:
		return nil, fmt.Errorf("codec: column %s: unknown method %d", col.Name, d.method)
	}
	var err error
	d.vals, d.runs, segs, err = splitSegs(col, d.method, d.rle, segs)
	if err != nil {
		return nil, err
	}
	if d.method == methodDict {
		r := segs[0]
		d.dict = make([]uint64, 0, len(r.buf)/max(r.width, 1))
		for r.pos < len(r.buf) {
			d.dict = append(d.dict, d.base+r.get())
		}
	}
	return d, nil
}

func splitSegs(col schema.Column, method byte, rle bool, segs []*reader) (vals, runs *reader, rest []*reader, err error) {
	want := len(columnStreams(method, rle, &stream{}, &stream{}, &stream{}))
	if len(segs) != want {
		return nil, nil, nil, fmt.Errorf("codec: column %s: %d streams, want %d", col.Name, len(segs), want)
	}
	if rle {
		runs, segs = segs[len(segs)-1], segs[:len(segs)-1]
	}
	if method != methodConst {
		vals, segs = segs[len(segs)-1], segs[:len(segs)-1]
	}
	return vals, runs, segs, nil
}

func (d *intDecoder) next(dst []byte) []byte {
	switch {
	case d.method == methodConst:
	case !d.rle:
		d.cur = d.value()
	default:
		if d.left == 0 {
			d.cur = d.value()
			d.left = d.runs.get() + 1
		}
		d.left--
	}
	return d.format(dst, d.cur)
}

func (d *intDecoder) value() uint64 {
	v := d.vals.get()
	switch d.method {
	case methodPlain:
		return d.base + v
	case methodDelta:
		d.prev += uint64(unzigzag(v))
		return d.prev
	}
	return d.dict[dictID(v, &d.fresh)]
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}
