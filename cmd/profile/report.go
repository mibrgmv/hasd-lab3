package main

import (
	"fmt"
	"io"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/textval"
)

type estimate struct {
	name  string
	bytes float64
}

// estimates gives rough sizes of the simplest encodings, to see where the
// bytes are before designing anything clever:
//
//	plain  native binary width (strings: bytes + varint length)
//	for    frame of reference: value-min in fixed width
//	delta  zigzag(value-previous) in fixed width
//	dict   global dictionary + fixed-width index per row
//	const  a single distinct value
func (s *colStats) estimates() []estimate {
	rows := float64(s.rows)
	d := s.distinct()
	dictBits := float64(bits.Len64(uint64(max(d-1, 0))))
	var out []estimate
	if d == 1 && s.exceptions == 0 {
		out = append(out, estimate{"const", 0})
	}
	if s.isInt {
		native := s.col.Bits
		switch s.col.Kind {
		case schema.Date:
			native = 16
		case schema.DateTime:
			native = 32
		}
		rangeBits := float64(bits.Len64(s.max - s.min))
		out = append(out,
			estimate{"plain", rows * float64(native) / 8},
			estimate{"for", rows * rangeBits / 8},
			estimate{"delta", rows * float64(pctWidth(&s.deltaHist, 1)) / 8})
		if !s.overflow {
			out = append(out, estimate{"dict", rows*dictBits/8 + float64(d)*rangeBits/8})
		}
		return out
	}
	plain := float64(s.sumLen + s.sumLenVarint)
	if s.col.Kind == schema.FixedString {
		plain = rows * float64(s.col.FixedLen)
	}
	out = append(out, estimate{"plain", plain})
	if !s.overflow {
		dict := rows*dictBits/8 + float64(len(s.arena))
		for _, e := range s.strs {
			dict += float64(uvarintLen(uint64(e.n)))
		}
		out = append(out, estimate{"dict", dict})
	}
	return out
}

func best(es []estimate) estimate {
	b := es[0]
	for _, e := range es[1:] {
		if e.bytes < b.bytes {
			b = e
		}
	}
	return b
}

// counts returns the frequency of every distinct value, most frequent first.
func (s *colStats) counts() []uint32 {
	var cs []uint32
	if s.isInt {
		for _, c := range s.intCount {
			cs = append(cs, c)
		}
	} else {
		for _, e := range s.strs {
			cs = append(cs, e.count)
		}
	}
	slices.SortFunc(cs, func(a, b uint32) int { return int(b) - int(a) })
	return cs
}

// entropy is the order-0 entropy in bits per value: a reference point only,
// since entropy coding itself is not allowed.
func entropy(cs []uint32) float64 {
	var n float64
	for _, c := range cs {
		n += float64(c)
	}
	var h float64
	for _, c := range cs {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func report(w io.Writer, stats []*colStats, rows, size uint64, elapsed time.Duration) {
	fmt.Fprintf(w, "rows %d, TSV %.1f MB, %d columns, profiled in %v\n\n", rows, float64(size)/1e6, len(stats), elapsed.Round(time.Second))

	type line struct {
		s      *colStats
		cs     []uint32
		h0     float64
		plain  float64
		best   estimate
		allEst []estimate
	}
	lines := make([]line, len(stats))
	for i, s := range stats {
		es := s.estimates()
		l := line{s: s, allEst: es, best: best(es), h0: math.NaN()}
		for _, e := range es {
			if e.name == "plain" {
				l.plain = e.bytes
			}
		}
		l.cs = s.counts()
		if !s.overflow {
			l.h0 = entropy(l.cs)
		}
		lines[i] = l
	}

	byWeight := slices.Clone(lines)
	slices.SortStableFunc(byWeight, func(a, b line) int { return int(b.s.rawBytes) - int(a.s.rawBytes) })

	fmt.Fprintf(w, "%3s  %-27s %-16s %9s %6s %10s %7s %6s %8s %9s  %-6s %9s\n",
		"#", "column", "type", "TSV MB", "share", "distinct", "H0 b/v", "rep%", "exc", "plain MB", "best", "MB")
	var totTSV, totPlain, totBest, totH0 float64
	h0Complete := true
	for _, l := range byWeight {
		s := l.s
		totTSV += float64(s.rawBytes)
		totPlain += l.plain
		totBest += l.best.bytes
		if math.IsNaN(l.h0) {
			h0Complete = false
		} else {
			totH0 += l.h0 * float64(s.rows-s.exceptions) / 8
		}
		fmt.Fprintf(w, "%3d  %-27s %-16s %9.1f %5.1f%% %10s %7s %5.1f%% %8d %9.1f  %-6s %9.1f\n",
			s.col.Index+1, s.col.Name, s.col.Type, float64(s.rawBytes)/1e6, 100*float64(s.rawBytes)/float64(size),
			distinctStr(s), h0Str(l.h0), pct(s.repeats, s.rows), s.exceptions, l.plain/1e6, l.best.name, l.best.bytes/1e6)
	}
	fmt.Fprintf(w, "\ntotal TSV %.1f MB; plain binary %.1f MB (%.1f%%); best simple scheme per column %.1f MB (%.1f%%)\n",
		totTSV/1e6, totPlain/1e6, 100*totPlain/totTSV, totBest/1e6, 100*totBest/totTSV)
	note := ""
	if !h0Complete {
		note = " (columns over the distinct cap excluded)"
	}
	fmt.Fprintf(w, "order-0 entropy of values %.1f MB%s: reference only\n", totH0/1e6, note)

	fmt.Fprintf(w, "\n\n==== details (schema order) ====\n")
	for _, l := range lines {
		details(w, l.s, l.cs, l.h0, l.allEst)
	}
}

func details(w io.Writer, s *colStats, cs []uint32, h0 float64, es []estimate) {
	parsed := s.rows - s.exceptions
	fmt.Fprintf(w, "\n[%d] %s  %s\n", s.col.Index+1, s.col.Name, s.col.Type)
	fmt.Fprintf(w, "  TSV %.1f MB, distinct %s, H0 %s bit/value, repeat-prev %.2f%%, exceptions %d\n",
		float64(s.rawBytes)/1e6, distinctStr(s), h0Str(h0), pct(s.repeats, s.rows), s.exceptions)
	if len(s.excSamples) > 0 {
		fmt.Fprintf(w, "  exception samples: %q\n", s.excSamples)
	}
	fmt.Fprintf(w, "  top:%s\n", s.topValues(5, parsed))

	var cov []string
	var cum uint64
	next := 0
	for _, k := range []int{1, 3, 15, 255, 4095, 65535} {
		for ; next < k && next < len(cs); next++ {
			cum += uint64(cs[next])
		}
		cov = append(cov, fmt.Sprintf("top%d %.2f%%", k, pct(cum, parsed)))
	}
	fmt.Fprintf(w, "  coverage: %s\n", strings.Join(cov, ", "))

	if s.isInt {
		fmt.Fprintf(w, "  min %s, max %s, range %d bit; value width p50/p90/p99/max %s; delta width %s\n",
			s.fmtInt(s.min), s.fmtInt(s.max), bits.Len64(s.max-s.min), widths(&s.valHist), widths(&s.deltaHist))
	} else {
		var esc []string
		for c, n := range s.escChars {
			if n > 0 {
				esc = append(esc, fmt.Sprintf("\\%c:%d", c, n))
			}
		}
		fmt.Fprintf(w, "  len min/avg/max %d/%.1f/%d, empty %.2f%%, escaped %.2f%% %v, avg LCP with prev %.1f, distinct bytes %.1f MB\n",
			s.minLen, float64(s.sumLen)/float64(s.rows), s.maxLen, pct(s.empty, s.rows), pct(s.escaped, s.rows), esc,
			float64(s.lcp)/float64(max(s.rows-1, 1)), float64(len(s.arena))/1e6)
		if s.col.Kind == schema.FixedString {
			fmt.Fprintf(w, "  unescaped length != %d: %d\n", s.col.FixedLen, s.badFixedLen)
		}
	}
	var parts []string
	for _, e := range es {
		parts = append(parts, fmt.Sprintf("%s %.1f", e.name, e.bytes/1e6))
	}
	fmt.Fprintf(w, "  estimates MB: %s\n", strings.Join(parts, ", "))
}

func (s *colStats) topValues(k int, total uint64) string {
	type kv struct {
		v string
		c uint32
	}
	var top []kv
	add := func(v func() string, c uint32) {
		if len(top) == k && c <= top[k-1].c {
			return
		}
		i, _ := slices.BinarySearchFunc(top, c, func(e kv, c uint32) int { return int(c) - int(e.c) })
		top = slices.Insert(top, i, kv{v(), c})
		if len(top) > k {
			top = top[:k]
		}
	}
	if s.isInt {
		for x, c := range s.intCount {
			add(func() string { return s.fmtInt(x) }, c)
		}
	} else {
		for _, e := range s.strs {
			add(func() string { return quote(s.arena[e.off : e.off+uint64(e.n)]) }, e.count)
		}
	}
	var b strings.Builder
	for _, e := range top {
		fmt.Fprintf(&b, " %.2f%% %s |", pct(uint64(e.c), total), e.v)
	}
	return strings.TrimSuffix(b.String(), " |")
}

func (s *colStats) fmtInt(x uint64) string {
	switch {
	case s.col.Kind == schema.Date:
		return string(textval.AppendDate(nil, int64(x)))
	case s.col.Kind == schema.DateTime:
		return string(textval.AppendDateTime(nil, int64(x)))
	case s.signed:
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatUint(x, 10)
}

func quote(b []byte) string {
	if len(b) > 60 {
		return strconv.Quote(string(b[:60])) + "..."
	}
	return strconv.Quote(string(b))
}

func distinctStr(s *colStats) string {
	if s.overflow {
		return ">=" + strconv.Itoa(distinctCap)
	}
	return strconv.Itoa(s.distinct())
}

func h0Str(h float64) string {
	if math.IsNaN(h) {
		return "n/a"
	}
	return strconv.FormatFloat(h, 'f', 2, 64)
}

func widths(h *[65]uint64) string {
	return fmt.Sprintf("%d/%d/%d/%d", pctWidth(h, 0.5), pctWidth(h, 0.9), pctWidth(h, 0.99), pctWidth(h, 1))
}

// pctWidth returns the smallest bit width that covers fraction p of values.
func pctWidth(h *[65]uint64, p float64) int {
	var total uint64
	for _, c := range h {
		total += c
	}
	need := uint64(math.Ceil(p * float64(total)))
	var cum uint64
	for w, c := range h {
		cum += c
		if cum >= need && cum > 0 {
			return w
		}
	}
	return 0
}

func pct(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
