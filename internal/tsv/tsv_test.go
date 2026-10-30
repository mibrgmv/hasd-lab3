package tsv

import (
	"strings"
	"testing"
)

func TestReaderCarriesRowsAcrossChunks(t *testing.T) {
	rows := [][]string{{"a", "", "ccc"}, {"dd\\te", "f", ""}, {"", "", ""}, {"123", "4", "56789"}}
	var in strings.Builder
	for _, r := range rows {
		in.WriteString(strings.Join(r, "\t") + "\n")
	}

	rd := NewReader(strings.NewReader(in.String()), 3, 16)
	var b Batch
	var got [][]string
	for {
		ok, err := rd.Next(&b)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		for r := 0; r < b.Rows; r++ {
			var row []string
			for c := 0; c < 3; c++ {
				row = append(row, string(b.Field(r, c)))
			}
			got = append(got, row)
		}
	}
	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		if strings.Join(got[i], "|") != strings.Join(rows[i], "|") {
			t.Errorf("row %d = %q, want %q", i, got[i], rows[i])
		}
	}
}

func TestReaderErrors(t *testing.T) {
	for _, in := range []string{"a\tb\n", "a\tb\tc\td\n", "a\tb\tc"} {
		var b Batch
		if _, err := NewReader(strings.NewReader(in), 3, 64).Next(&b); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}
