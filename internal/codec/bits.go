package codec

import (
	"encoding/binary"
	"math/bits"
	"strconv"
)

// bitWriter packs values LSB-first into a byte slice.
type bitWriter struct {
	buf []byte
	acc uint64
	n   uint // pending bits in acc, always < 8 between calls
}

func (w *bitWriter) write(v uint64, n uint) {
	if n > 32 {
		w.write(v&(1<<32-1), 32)
		v >>= 32
		n -= 32
	}
	w.acc |= (v & (1<<n - 1)) << w.n
	w.n += n
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

func (w *bitWriter) writeBytes(b []byte) {
	if w.n == 0 {
		w.buf = append(w.buf, b...)
		return
	}
	for ; len(b) >= 4; b = b[4:] {
		w.write(uint64(binary.LittleEndian.Uint32(b)), 32)
	}
	for _, c := range b {
		w.write(uint64(c), 8)
	}
}

// align pads with zero bits up to a byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.n = 0, 0
	}
}

func (w *bitWriter) bytes() []byte {
	w.align()
	return w.buf
}

type bitReader struct {
	buf []byte
	pos int // next byte to load into acc
	acc uint64
	n   uint // valid bits in acc
}

func (r *bitReader) refill() {
	if r.pos+8 <= len(r.buf) {
		// Bits above the n+8k loaded ones are the following stream bytes,
		// so OR-ing them in again on the next refill changes nothing.
		r.acc |= binary.LittleEndian.Uint64(r.buf[r.pos:]) << r.n
		k := (63 - r.n) / 8
		r.pos += int(k)
		r.n += k * 8
		return
	}
	for r.n <= 56 && r.pos < len(r.buf) {
		r.acc |= uint64(r.buf[r.pos]) << r.n
		r.pos++
		r.n += 8
	}
}

func (r *bitReader) read(n uint) uint64 {
	if n > 32 {
		lo := r.read(32)
		return lo | r.read(n-32)<<32
	}
	if r.n < n {
		r.refill()
		if r.n < n {
			panic("codec: bit stream truncated")
		}
	}
	v := r.acc & (1<<n - 1)
	r.acc >>= n
	r.n -= n
	return v
}

func (r *bitReader) appendBytes(dst []byte, n int) []byte {
	for ; n >= 4; n -= 4 {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(r.read(32)))
	}
	for ; n > 0; n-- {
		dst = append(dst, byte(r.read(8)))
	}
	return dst
}

// alignedBytes skips to the next byte boundary and returns the next k bytes
// without copying them.
func (r *bitReader) alignedBytes(k int) []byte {
	p := (r.pos*8 - int(r.n) + 7) / 8
	b := r.buf[p : p+k]
	r.pos, r.acc, r.n = p+k, 0, 0
	return b
}

// uintCode writes an unsigned integer as a class selector followed by the
// value in that class's fixed width. The widths are fitted to the column's
// histogram of value widths; with a single class this is plain fixed-width
// bit packing. Code lengths depend only on the magnitude of a value, never on
// how often that particular value occurs, so this is not entropy coding.
type uintCode struct {
	widths []uint8
	sel    uint
}

func newUintCode(widths ...uint8) uintCode {
	return uintCode{widths: widths, sel: uint(bits.Len(uint(len(widths) - 1)))}
}

func (c *uintCode) write(w *bitWriter, v uint64) {
	x := uint8(bits.Len64(v))
	for i, cw := range c.widths {
		if x <= cw {
			w.write(uint64(i), c.sel)
			w.write(v, uint(cw))
			return
		}
	}
	panic("codec: value does not fit the planned widths")
}

func (c *uintCode) read(r *bitReader) uint64 {
	return r.read(uint(c.widths[r.read(c.sel)]))
}

func (c *uintCode) save(w *bitWriter) {
	w.write(uint64(len(c.widths)), 8)
	for _, cw := range c.widths {
		w.write(uint64(cw), 8)
	}
}

func loadUintCode(r *bitReader) uintCode {
	widths := make([]uint8, r.read(8))
	for i := range widths {
		widths[i] = uint8(r.read(8))
	}
	return newUintCode(widths...)
}

func (c uintCode) String() string {
	s := "["
	for i, w := range c.widths {
		if i > 0 {
			s += " "
		}
		s += strconv.Itoa(int(w))
	}
	return s + "]"
}

// bestCode picks up to four class widths that minimise the total size of
// values whose bit widths follow histogram h, and returns the code with its
// cost in bits. Dynamic programming over the 65 possible widths: dp[j][b] is
// the cheapest cover of widths 0..b by j classes, the largest of width b.
func bestCode(h *[65]uint64) (uintCode, uint64) {
	var total uint64
	maxW := 0
	var p [66]uint64 // p[b+1] = number of values of width <= b
	for w, c := range h {
		total += c
		if c > 0 {
			maxW = w
		}
		p[w+1] = p[w] + c
	}

	const maxClasses = 4
	const inf = ^uint64(0)
	var dp, from [maxClasses + 1][65]uint64
	for j := range dp {
		for b := range dp[j] {
			dp[j][b] = inf
		}
	}
	for b := 0; b <= maxW; b++ {
		dp[1][b] = uint64(b) * p[b+1]
	}
	for j := 2; j <= maxClasses; j++ {
		for b := 0; b <= maxW; b++ {
			for a := 0; a < b; a++ {
				if dp[j-1][a] == inf {
					continue
				}
				if c := dp[j-1][a] + uint64(b)*(p[b+1]-p[a+1]); c < dp[j][b] {
					dp[j][b], from[j][b] = c, uint64(a)
				}
			}
		}
	}

	bestJ, bestCost := 1, dp[1][maxW]
	for j := 2; j <= maxClasses; j++ {
		if dp[j][maxW] == inf {
			continue
		}
		if c := dp[j][maxW] + uint64(bits.Len(uint(j-1)))*total; c < bestCost {
			bestJ, bestCost = j, c
		}
	}
	widths := make([]uint8, bestJ)
	b := maxW
	for j := bestJ; j >= 1; j-- {
		widths[j-1] = uint8(b)
		b = int(from[j][b])
	}
	return newUintCode(widths...), bestCost
}

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }
