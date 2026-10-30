package textval

import (
	"strconv"
	"testing"
)

func TestDateRoundTrip(t *testing.T) {
	last := daysFromCivil(9999, 12, 31)
	for d := int64(0); d <= last; d++ {
		s := AppendDate(nil, d)
		got, ok := ParseDate(s)
		if !ok || got != d {
			t.Fatalf("day %d: %q parsed as %d, %v", d, s, got, ok)
		}
	}
}

func TestDateTime(t *testing.T) {
	const s = "2014-03-23 16:23:07"
	v, ok := ParseDateTime([]byte(s))
	if !ok || v != 1395591787 {
		t.Fatalf("ParseDateTime(%q) = %d, %v", s, v, ok)
	}
	if got := string(AppendDateTime(nil, v)); got != s {
		t.Fatalf("AppendDateTime(%d) = %q", v, got)
	}
	for _, bad := range []string{
		"0000-00-00 00:00:00", "2014-02-29 00:00:00", "2014-13-01 00:00:00",
		"2014-03-23 24:00:00", "2014-03-23T16:23:07", "2014-03-23 16:23:7",
	} {
		if _, ok := ParseDateTime([]byte(bad)); ok {
			t.Errorf("ParseDateTime(%q) accepted", bad)
		}
	}
}

func TestInts(t *testing.T) {
	for _, s := range []string{"0", "7", "18446744073709551615"} {
		v, ok := ParseUint([]byte(s))
		if !ok || strconv.FormatUint(v, 10) != s {
			t.Errorf("ParseUint(%q) = %d, %v", s, v, ok)
		}
	}
	for _, s := range []string{"", "-", "+1", "007", "-1", "18446744073709551616", "1a"} {
		if _, ok := ParseUint([]byte(s)); ok {
			t.Errorf("ParseUint(%q) accepted", s)
		}
	}
	for _, s := range []string{"0", "-1", "127", "-9223372036854775808", "9223372036854775807"} {
		v, ok := ParseInt([]byte(s))
		if !ok || strconv.FormatInt(v, 10) != s {
			t.Errorf("ParseInt(%q) = %d, %v", s, v, ok)
		}
	}
	for _, s := range []string{"-0", "-01", "9223372036854775808", "-9223372036854775809", "--1"} {
		if _, ok := ParseInt([]byte(s)); ok {
			t.Errorf("ParseInt(%q) accepted", s)
		}
	}
}
