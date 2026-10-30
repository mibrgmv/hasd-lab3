// Package textval converts between ClickHouse TabSeparated text and typed
// values. Parse functions accept only canonical text, i.e. text that the
// matching Append function reproduces byte for byte; anything else has to be
// stored verbatim by the caller.
package textval

import "math"

// ParseUint parses a decimal without sign or leading zeros.
func ParseUint(s []byte) (uint64, bool) {
	if len(s) == 0 || len(s) > 20 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	var v uint64
	for _, c := range s {
		d := uint64(c - '0')
		if d > 9 || v > (math.MaxUint64-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	return v, true
}

// ParseInt parses an optionally negative decimal; "-0" is not canonical.
func ParseInt(s []byte) (int64, bool) {
	neg := len(s) > 1 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	u, ok := ParseUint(s)
	switch {
	case !ok:
		return 0, false
	case !neg:
		return int64(u), u <= math.MaxInt64
	case u == 0:
		return 0, false
	default:
		return -int64(u), u <= 1<<63
	}
}

// ParseDate parses YYYY-MM-DD into days since 1970-01-01.
func ParseDate(s []byte) (int64, bool) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return 0, false
	}
	return parseYMD(s)
}

// ParseDateTime parses "YYYY-MM-DD hh:mm:ss" into seconds since
// 1970-01-01 00:00:00 of the same wall clock. No time zone is applied, so the
// mapping is a bijection and survives DST changes.
func ParseDateTime(s []byte) (int64, bool) {
	if len(s) != 19 || s[4] != '-' || s[7] != '-' || s[10] != ' ' || s[13] != ':' || s[16] != ':' {
		return 0, false
	}
	days, ok := parseYMD(s[:10])
	h, ok1 := digits(s[11:13])
	m, ok2 := digits(s[14:16])
	sec, ok3 := digits(s[17:19])
	if !ok || !ok1 || !ok2 || !ok3 || h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	return days*86400 + h*3600 + m*60 + sec, true
}

func AppendDate(dst []byte, days int64) []byte {
	y, m, d := civilFromDays(days)
	dst = append4(dst, y)
	dst = append(dst, '-')
	dst = append2(dst, m)
	dst = append(dst, '-')
	return append2(dst, d)
}

func AppendDateTime(dst []byte, secs int64) []byte {
	dst = AppendDate(dst, secs/86400)
	rem := secs % 86400
	dst = append(dst, ' ')
	dst = append2(dst, rem/3600)
	dst = append(dst, ':')
	dst = append2(dst, rem/60%60)
	dst = append(dst, ':')
	return append2(dst, rem%60)
}

// parseYMD accepts years 1970..9999, so day numbers are never negative.
func parseYMD(s []byte) (int64, bool) {
	y, ok1 := digits(s[0:4])
	m, ok2 := digits(s[5:7])
	d, ok3 := digits(s[8:10])
	if !ok1 || !ok2 || !ok3 || y < 1970 || m < 1 || m > 12 || d < 1 || d > daysIn(y, m) {
		return 0, false
	}
	return daysFromCivil(y, m, d), true
}

func digits(s []byte) (int64, bool) {
	var v int64
	for _, c := range s {
		d := c - '0'
		if d > 9 {
			return 0, false
		}
		v = v*10 + int64(d)
	}
	return v, true
}

func daysIn(y, m int64) int64 {
	switch m {
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// daysFromCivil and civilFromDays are Howard Hinnant's algorithms,
// restricted to non-negative years.
func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	yoe := y - era*400
	doy := (153*((m+9)%12)+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func civilFromDays(z int64) (y, m, d int64) {
	z += 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if m > 12 {
		m -= 12
	}
	y = yoe + era*400
	if m <= 2 {
		y++
	}
	return y, m, d
}

func append2(dst []byte, v int64) []byte {
	return append(dst, byte('0'+v/10), byte('0'+v%10))
}

func append4(dst []byte, v int64) []byte {
	return append(dst, byte('0'+v/1000), byte('0'+v/100%10), byte('0'+v/10%10), byte('0'+v%10))
}
