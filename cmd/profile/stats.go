package main

import (
	"bytes"
	"encoding/binary"
	"hash/maphash"
	"math/bits"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/textval"
)

// distinctCap bounds the per-column distinct-value tables to keep memory in
// check; columns that exceed it are reported as ">=cap".
const distinctCap = 1 << 22

var seed = maphash.MakeSeed()

type strEntry struct {
	off   uint64
	n     uint32
	count uint32
}

// colStats accumulates everything we want to know about one column. Integer
// kinds (Int, Date, DateTime) are parsed; all other kinds are profiled as raw
// escaped bytes.
type colStats struct {
	col    schema.Column
	isInt  bool
	signed bool

	rows, rawBytes uint64
	repeats        uint64 // value equal to the previous row's value
	exceptions     uint64 // integer text that is not canonical
	excSamples     []string

	overflow bool
	intCount map[uint64]uint32 // integer kinds: value -> count
	strIndex map[uint64]uint32 // other kinds: hash -> index into strs
	strs     []strEntry
	arena    []byte // bytes of distinct values in first-seen order

	hasPrev   bool
	prev      uint64
	prevBytes []byte

	min, max  uint64     // compared as int64 when signed
	valHist   [65]uint64 // bit width of the value (zigzag when signed)
	deltaHist [65]uint64 // bit width of zigzag(value - previous)

	sumLen, sumLenVarint uint64
	minLen, maxLen       int
	empty, escaped, lcp  uint64
	escChars             [256]uint64 // byte following a backslash
	badFixedLen          uint64      // FixedString(N) whose unescaped length != N
}

func newColStats(c schema.Column) *colStats {
	s := &colStats{col: c}
	switch c.Kind {
	case schema.Int, schema.Date, schema.DateTime:
		s.isInt = true
		s.signed = c.Kind != schema.Int || c.Signed
		s.intCount = make(map[uint64]uint32)
	default:
		s.strIndex = make(map[uint64]uint32)
	}
	return s
}

func (s *colStats) add(v []byte) {
	s.rows++
	s.rawBytes += uint64(len(v)) + 1 // + separator
	if !s.isInt {
		s.addBytes(v)
		return
	}
	x, ok := s.parse(v)
	if !ok {
		s.exceptions++
		if len(s.excSamples) < 5 {
			s.excSamples = append(s.excSamples, string(v))
		}
		return
	}
	s.addInt(x)
}

func (s *colStats) parse(v []byte) (uint64, bool) {
	switch {
	case s.col.Kind == schema.Date:
		d, ok := textval.ParseDate(v)
		return uint64(d), ok
	case s.col.Kind == schema.DateTime:
		t, ok := textval.ParseDateTime(v)
		return uint64(t), ok
	case s.signed:
		x, ok := textval.ParseInt(v)
		return uint64(x), ok
	}
	return textval.ParseUint(v)
}

func (s *colStats) addInt(x uint64) {
	if !s.hasPrev {
		s.min, s.max = x, x
	} else {
		if x == s.prev {
			s.repeats++
		}
		s.deltaHist[bits.Len64(zigzag(int64(x-s.prev)))]++
		if s.less(x, s.min) {
			s.min = x
		}
		if s.less(s.max, x) {
			s.max = x
		}
	}
	s.hasPrev, s.prev = true, x
	if s.signed {
		s.valHist[bits.Len64(zigzag(int64(x)))]++
	} else {
		s.valHist[bits.Len64(x)]++
	}

	if len(s.intCount) < distinctCap {
		s.intCount[x]++
	} else if _, ok := s.intCount[x]; ok {
		s.intCount[x]++
	} else {
		s.overflow = true
	}
}

func (s *colStats) addBytes(v []byte) {
	n := len(v)
	if !s.hasPrev {
		s.minLen, s.maxLen = n, n
	} else {
		if bytes.Equal(v, s.prevBytes) {
			s.repeats++
		}
		s.lcp += uint64(commonPrefix(v, s.prevBytes))
		s.minLen, s.maxLen = min(s.minLen, n), max(s.maxLen, n)
	}
	s.hasPrev = true
	s.prevBytes = append(s.prevBytes[:0], v...)
	s.sumLen += uint64(n)
	s.sumLenVarint += uint64(uvarintLen(uint64(n)))
	if n == 0 {
		s.empty++
	}

	escapes := 0
	if i := bytes.IndexByte(v, '\\'); i >= 0 {
		s.escaped++
		for ; i < n; i++ {
			if v[i] == '\\' && i+1 < n {
				s.escChars[v[i+1]]++
				escapes++
				i++
			}
		}
	}
	if s.col.Kind == schema.FixedString && n-escapes != s.col.FixedLen {
		s.badFixedLen++
	}

	h := maphash.Bytes(seed, v)
	if i, ok := s.strIndex[h]; ok {
		s.strs[i].count++
	} else if len(s.strs) < distinctCap {
		s.strIndex[h] = uint32(len(s.strs))
		s.strs = append(s.strs, strEntry{off: uint64(len(s.arena)), n: uint32(n), count: 1})
		s.arena = append(s.arena, v...)
	} else {
		s.overflow = true
	}
}

func (s *colStats) less(a, b uint64) bool {
	if s.signed {
		return int64(a) < int64(b)
	}
	return a < b
}

func (s *colStats) distinct() int {
	if s.isInt {
		return len(s.intCount)
	}
	return len(s.strs)
}

func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

func uvarintLen(v uint64) int {
	var buf [binary.MaxVarintLen64]byte
	return binary.PutUvarint(buf[:], v)
}

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
