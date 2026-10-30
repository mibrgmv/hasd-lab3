package codec

import (
	"bytes"
	"errors"
	"math/bits"
)

type stream struct {
	buf   []byte
	width int
}

func (s *stream) put(v uint64) {
	for i := s.width - 1; i >= 0; i-- {
		s.buf = append(s.buf, byte(v>>(8*i)))
	}
}

func (s *stream) putLine(v []byte) {
	s.buf = append(append(s.buf, v...), '\n')
}

type reader struct {
	buf   []byte
	pos   int
	width int
}

func (r *reader) get() uint64 {
	if r.pos+r.width > len(r.buf) {
		panic(errTruncated)
	}
	var v uint64
	for _, c := range r.buf[r.pos : r.pos+r.width] {
		v = v<<8 | uint64(c)
	}
	r.pos += r.width
	return v
}

func (r *reader) line() []byte {
	i := bytes.IndexByte(r.buf[r.pos:], '\n')
	if i < 0 {
		panic(errTruncated)
	}
	v := r.buf[r.pos : r.pos+i : r.pos+i]
	r.pos += i + 1
	return v
}

var errTruncated = errors.New("codec: stream truncated")

type header struct {
	buf []byte
	pos int
}

func (h *header) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		h.buf = append(h.buf, byte(v>>(8*i)))
	}
}

func (h *header) get(n int) uint64 {
	if h.pos+n > len(h.buf) {
		panic(errTruncated)
	}
	var v uint64
	for _, c := range h.buf[h.pos : h.pos+n] {
		v = v<<8 | uint64(c)
	}
	h.pos += n
	return v
}

func (h *header) putBytes(b []byte) {
	h.put(uint64(len(b)), 4)
	h.buf = append(h.buf, b...)
}

func (h *header) getBytes() []byte {
	n := int(h.get(4))
	if h.pos+n > len(h.buf) {
		panic(errTruncated)
	}
	b := h.buf[h.pos : h.pos+n : h.pos+n]
	h.pos += n
	return b
}

func widthOf(maxBits int) int {
	return (maxBits + 7) / 8
}

func widthFor(v uint64) int {
	return widthOf(bits.Len64(v))
}

func maxBits(h *[65]uint64) int {
	for w := 64; w > 0; w-- {
		if h[w] > 0 {
			return w
		}
	}
	return 0
}

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }
