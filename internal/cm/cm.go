package cm

import (
	"errors"
	"math/bits"
)

const (
	nHashed = 6
	nInputs = nHashed + 4
	lr      = 12
	minLen  = 6
	maxLen  = 32
)

var (
	squashTab  = [33]int32{1, 2, 3, 6, 10, 16, 27, 45, 73, 120, 194, 310, 488, 747, 1101, 1546, 2047, 2549, 2994, 3348, 3607, 3785, 3901, 3975, 4022, 4050, 4068, 4079, 4085, 4089, 4092, 4093, 4094}
	stretchTab [4096]int32
	recip      [1024]int64
)

func init() {
	next := int32(0)
	for x := int32(-2047); x <= 2047; x++ {
		v := squash(x)
		for j := next; j <= v; j++ {
			stretchTab[j] = x
		}
		next = v + 1
	}
	for j := next; j < 4096; j++ {
		stretchTab[j] = 2047
	}
	for i := range recip {
		recip[i] = int64(16384 / (i + i + 3))
	}
}

func squash(d int32) int32 {
	if d > 2047 {
		return 4095
	}
	if d < -2047 {
		return 1
	}
	w := d & 127
	i := d>>7 + 16
	return (squashTab[i]*(128-w) + squashTab[i+1]*w + 64) >> 7
}

type counters []uint32

func newCounters(n int) counters {
	t := make(counters, n)
	for i := range t {
		t[i] = 1 << 31
	}
	return t
}

func (t counters) p(i uint32) int32 {
	return int32(t[i] >> 20)
}

func (t counters) update(i uint32, bit int, limit uint32) {
	e := t[i]
	n := e & 1023
	p := int64(e >> 10)
	if n < limit {
		e++
	} else {
		e = e&^1023 | limit
	}
	d := ((int64(bit)<<22 - p) >> 3) * recip[n]
	t[i] = uint32(int64(e) + d&^1023)
}

type predictor struct {
	buf    []byte
	pos    int
	width  int
	decode bool

	c0     uint32
	bitPos int
	c4, c8 uint32
	word   uint64
	prevW  uint64

	o0     counters
	o1     counters
	hashed [nHashed]counters
	ctx    [nHashed]uint64
	shift  uint
	idx    [nInputs]uint32

	mt     []uint32
	mshift uint
	mptr   int
	mlen   int
	mm     counters
	mmIdx  uint32

	st  [nInputs]int32
	w   []int32
	sel int
	pr  int32
}

func newPredictor(buf []byte, n, width int, decode bool) *predictor {
	tb := min(max(bits.Len(uint(n))+1, 12), 22)
	mb := min(max(bits.Len(uint(n)), 12), 22)
	m := &predictor{
		buf:    buf,
		width:  width,
		decode: decode,
		c0:     1,
		o0:     newCounters(256),
		o1:     newCounters(1 << 16),
		shift:  uint(64 - tb),
		mt:     make([]uint32, 1<<mb),
		mshift: uint(64 - mb),
		mm:     newCounters(64),
		w:      make([]int32, 512*nInputs),
	}
	for i := range m.hashed {
		m.hashed[i] = newCounters(1 << tb)
	}
	for i := range m.w {
		m.w[i] = 1 << 14
	}
	m.byteContexts()
	m.predict()
	return m
}

func hash(a, b, c uint64) uint64 {
	h := a*0x9E3779B97F4A7C15 ^ b*0xC2B2AE3D27D4EB4F ^ c*0x165667B19E3779F9
	return h ^ h>>29
}

func (m *predictor) byteContexts() {
	c1 := uint64(m.c4 & 0xff)
	m.ctx[0] = hash(2, uint64(m.c4&0xffff), 0)
	m.ctx[1] = hash(3, uint64(m.c4&0xffffff), 0)
	m.ctx[2] = hash(4, uint64(m.c4), 0)
	m.ctx[3] = hash(6, uint64(m.c4), uint64(m.c8&0xffff))
	if m.width > 1 {
		col := uint64(m.pos % m.width)
		var above, above2 uint64
		if m.pos >= m.width {
			above = uint64(m.buf[m.pos-m.width])
		}
		if m.pos >= 2*m.width {
			above2 = uint64(m.buf[m.pos-2*m.width])
		}
		m.ctx[4] = hash(7, col<<8|above, above2)
		m.ctx[5] = hash(8, col<<8|above, c1)
	} else {
		m.ctx[4] = hash(9, m.word, 0)
		m.ctx[5] = hash(10, m.word, m.prevW)
	}
}

func (m *predictor) predict() {
	c0 := uint64(m.c0)
	m.idx[0] = m.c0
	m.idx[1] = (m.c4&0xff)<<8 | m.c0
	m.st[0] = stretchTab[m.o0.p(m.idx[0])]
	m.st[1] = stretchTab[m.o1.p(m.idx[1])]
	for i := range m.hashed {
		j := uint32((m.ctx[i] ^ c0*0xD6E8FEB86659FD93) * 0x9E3779B97F4A7C15 >> m.shift)
		m.idx[i+2] = j
		m.st[i+2] = stretchTab[m.hashed[i].p(j)]
	}

	m.mmIdx = 0
	if m.mlen > 0 {
		exp := uint32(m.buf[m.mptr]) | 256
		if exp>>(8-m.bitPos) == m.c0 {
			m.mmIdx = uint32(2+min(m.mlen, 15)*2) + exp>>(7-m.bitPos)&1
		} else {
			m.mlen = 0
		}
	}
	m.st[nInputs-2] = stretchTab[m.mm.p(m.mmIdx)]
	m.st[nInputs-1] = 256

	m.sel = int(m.c0) * nInputs
	if m.mlen > 0 {
		m.sel += 256 * nInputs
	}
	w := m.w[m.sel : m.sel+nInputs]
	var dot int64
	for i, s := range m.st {
		dot += int64(w[i]) * int64(s)
	}
	dot >>= 16
	m.pr = min(max(squash(int32(max(min(dot, 2047), -2047))), 1), 4095)
}

func (m *predictor) update(bit int) {
	err := (int32(bit)<<12 - m.pr) * lr
	w := m.w[m.sel : m.sel+nInputs]
	for i, s := range m.st {
		w[i] += (s * err) >> 14
	}

	m.o0.update(m.idx[0], bit, 60)
	m.o1.update(m.idx[1], bit, 250)
	for i := range m.hashed {
		m.hashed[i].update(m.idx[i+2], bit, 1023)
	}
	m.mm.update(m.mmIdx, bit, 1023)

	m.c0 = m.c0<<1 | uint32(bit)
	m.bitPos++
	if m.bitPos == 8 {
		m.endByte(byte(m.c0))
		m.c0, m.bitPos = 1, 0
	}
	m.predict()
}

func (m *predictor) endByte(c byte) {
	if m.decode {
		m.buf[m.pos] = c
	}
	m.pos++
	m.c8 = m.c8<<8 | m.c4>>24
	m.c4 = m.c4<<8 | uint32(c)
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80 {
		m.word = (m.word + uint64(c) + 1) * 0x2F0F3C5A1B3E4D27
	} else if m.word != 0 {
		m.prevW, m.word = m.word, 0
	}

	if m.mlen > 0 && m.buf[m.mptr] == c {
		m.mlen++
		m.mptr++
	} else {
		m.mlen = 0
	}
	if m.pos >= minLen {
		h := hash(11, uint64(m.c4), uint64(m.c8&0xffff)) >> m.mshift
		if m.mlen == 0 {
			if p := int(m.mt[h]); p > 0 {
				l := 0
				for l < maxLen && p-1-l >= 0 && m.buf[p-1-l] == m.buf[m.pos-1-l] {
					l++
				}
				if l >= minLen {
					m.mptr, m.mlen = p, l
				}
			}
		}
		m.mt[h] = uint32(m.pos)
	}
	m.byteContexts()
}

func Compress(src []byte, width int) []byte {
	if len(src) == 0 {
		return nil
	}
	m := newPredictor(src, len(src), width, false)
	x1, x2 := uint32(0), uint32(0xffffffff)
	out := make([]byte, 0, len(src)/3+16)
	for _, c := range src {
		for i := 7; i >= 0; i-- {
			bit := int(c>>uint(i)) & 1
			xmid := x1 + uint32(uint64(x2-x1)*uint64(m.pr)>>12)
			if bit == 1 {
				x2 = xmid
			} else {
				x1 = xmid + 1
			}
			m.update(bit)
			for (x1^x2)&0xff000000 == 0 {
				out = append(out, byte(x2>>24))
				x1 <<= 8
				x2 = x2<<8 | 255
			}
		}
	}
	return append(out, byte(x1>>24), byte(x1>>16), byte(x1>>8), byte(x1))
}

func Decompress(src []byte, n, width int) (out []byte, err error) {
	out = make([]byte, n)
	if n == 0 {
		return out, nil
	}
	defer func() {
		if recover() != nil {
			out, err = nil, errors.New("cm: corrupt stream")
		}
	}()
	m := newPredictor(out, n, width, true)
	pos := 0
	next := func() uint32 {
		if pos < len(src) {
			pos++
			return uint32(src[pos-1])
		}
		return 0
	}
	x1, x2, x := uint32(0), uint32(0xffffffff), uint32(0)
	for range 4 {
		x = x<<8 | next()
	}
	for range n * 8 {
		xmid := x1 + uint32(uint64(x2-x1)*uint64(m.pr)>>12)
		bit := 0
		if x <= xmid {
			bit = 1
			x2 = xmid
		} else {
			x1 = xmid + 1
		}
		m.update(bit)
		for (x1^x2)&0xff000000 == 0 {
			x1 <<= 8
			x2 = x2<<8 | 255
			x = x<<8 | next()
		}
	}
	return out, nil
}
