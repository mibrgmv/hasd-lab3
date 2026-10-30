package codec

import (
	"bytes"
	"fmt"
	"math/bits"

	"hasd/lab3/internal/schema"
)

type bytesColumn struct {
	col schema.Column

	rows, runs, raw uint64
	started         bool
	prev            []byte
	runLen          uint64
	runHist         [65]uint64
	sumAll, sumH    uint64
	keyBytes        uint64
	index           map[string]uint32
	overflow        bool

	method    byte
	rle       bool
	h         header
	dict      stream
	vals      stream
	runStream stream
	fresh     uint32
	runVal    []byte
	runN      uint64
}

func newBytesColumn(c schema.Column) *bytesColumn {
	return &bytesColumn{col: c, index: make(map[string]uint32)}
}

func newRawBytesColumn(c schema.Column) *bytesColumn {
	b := &bytesColumn{col: c, method: methodPlain}
	b.writeHeader()
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
		c.sumH += uint64(len(v)) + 1
	}
	c.runLen++
	c.sumAll += uint64(len(v)) + 1

	if c.overflow {
		return
	}
	if _, ok := c.index[string(v)]; !ok {
		if len(c.index) == dictCap {
			c.overflow = true
			c.index = nil
			return
		}
		c.index[string(v)] = uint32(len(c.index))
		c.keyBytes += uint64(len(v)) + 1
	}
}

func (c *bytesColumn) plan() encoder {
	c.runHist[bits.Len64(c.runLen-1)]++
	runW := widthOf(maxBits(&c.runHist))
	d := uint64(len(c.index))

	var cands []candidate
	if !c.overflow && d == 1 {
		cands = append(cands, candidate{method: methodConst})
	}
	for _, rle := range []bool{false, true} {
		n, extra, sum := c.rows, uint64(0), c.sumAll
		if rle {
			n, extra, sum = c.runs, c.runs*uint64(runW), c.sumH
		}
		if !c.overflow {
			w := widthFor(d)
			cands = append(cands, candidate{methodDict, rle, w, n*uint64(w) + c.keyBytes + extra})
		}
		cands = append(cands, candidate{methodPlain, rle, 0, sum + extra})
	}
	best := cheapest(cands)

	c.method, c.rle = best.method, best.rle
	c.vals.width, c.runStream.width = best.width, runW
	c.writeHeader()
	c.prev = nil
	if c.method != methodDict {
		c.index = nil
	}
	return c
}

func (c *bytesColumn) writeHeader() {
	c.h.put(kindBytes, 1)
	c.h.put(uint64(c.method), 1)
	c.h.put(b2u(c.rle), 1)
	switch c.method {
	case methodConst:
		for k := range c.index {
			c.h.putBytes([]byte(k))
		}
	case methodDict:
		keys := make([]string, len(c.index))
		for k, i := range c.index {
			keys[i] = k
		}
		for _, k := range keys {
			c.dict.putLine([]byte(k))
		}
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
		c.runStream.put(c.runN - 1)
	}
}

func (c *bytesColumn) emit(v []byte) {
	if c.method == methodDict {
		c.vals.put(dictCode(c.index[string(v)], &c.fresh))
		return
	}
	c.vals.putLine(v)
}

func (c *bytesColumn) finish() ([]byte, []*stream) {
	if c.rle {
		c.flushRun()
	}
	c.index = nil
	return c.h.buf, columnStreams(c.method, c.rle, &c.dict, &c.vals, &c.runStream)
}

func (c *bytesColumn) describe() string {
	return describe(c.method, c.rle, c.vals.width, c.runStream.width)
}

type bytesDecoder struct {
	method byte
	rle    bool
	vals   *reader
	runs   *reader
	dict   [][]byte
	cur    []byte
	fresh  uint32
	left   uint64
}

func newBytesDecoder(col schema.Column, h *header, segs []*reader) (*bytesDecoder, error) {
	d := &bytesDecoder{method: byte(h.get(1)), rle: h.get(1) == 1}
	switch d.method {
	case methodConst:
		d.cur = h.getBytes()
	case methodPlain, methodDict:
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
		for r.pos < len(r.buf) {
			d.dict = append(d.dict, r.line())
		}
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
			d.left = d.runs.get() + 1
		}
		d.left--
	}
	return append(dst, d.cur...)
}

func (d *bytesDecoder) value() {
	if d.method == methodDict {
		d.cur = d.dict[dictID(d.vals.get(), &d.fresh)]
		return
	}
	d.cur = d.vals.line()
}
