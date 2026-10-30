package codec

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"hasd/lab3/internal/schema"
)

func TestBitsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	type item struct {
		v uint64
		n uint
	}
	var items []item
	var w bitWriter
	for range 100000 {
		n := uint(rng.IntN(65))
		v := rng.Uint64() & (1<<n - 1)
		if n == 64 {
			v = rng.Uint64()
		}
		items = append(items, item{v, n})
		w.write(v, n)
	}
	r := bitReader{buf: w.bytes()}
	for i, it := range items {
		if got := r.read(it.n); got != it.v {
			t.Fatalf("item %d: read %d bits = %x, want %x", i, it.n, got, it.v)
		}
	}
}

func TestBestCode(t *testing.T) {
	var h [65]uint64
	h[0], h[6], h[20] = 1000, 1000, 1
	code, cost := bestCode(&h)
	// two classes {6, 20}: 1 selector bit each, 6 or 20 value bits
	if want := uint64(2000*7 + 21); cost > want {
		t.Fatalf("cost %d with %v, want <= %d", cost, code, want)
	}
	var w bitWriter
	for _, v := range []uint64{0, 63, 1 << 19} {
		code.write(&w, v)
	}
	r := bitReader{buf: w.bytes()}
	for _, v := range []uint64{0, 63, 1 << 19} {
		if got := code.read(&r); got != v {
			t.Fatalf("read %d, want %d", got, v)
		}
	}
}

func TestEncodeDecode(t *testing.T) {
	cols := mustCols(t,
		"I UInt64", "S Int16", "D Date", "T DateTime", "C UInt8",
		"Str String", "F FixedString(2)", "A Array(UInt16)", "Bad UInt32", "K String")
	rng := rand.New(rand.NewPCG(3, 4))
	var in strings.Builder
	for i := range 5000 {
		bad := fmt.Sprint(rng.IntN(100))
		if i == 17 {
			bad = "007" // non-canonical: column falls back to raw bytes
		}
		fields := []string{
			fmt.Sprint(rng.Uint64()),
			fmt.Sprint(rng.IntN(200) - 100),
			fmt.Sprintf("2014-03-%02d", 17+i/1000),
			fmt.Sprintf("2014-03-%02d %02d:%02d:%02d", 17+i/1000, i/200%24, i%60, rng.IntN(60)),
			fmt.Sprint(i / 700),
			[]string{"", "http://a.ru/x", `it\'s \\ \t`, "http://a.ru/y"}[rng.IntN(4)],
			[]string{"nD", `\0\n`}[i/100%2],
			[]string{"[]", "[1,2]", "[33]"}[i/30%3],
			bad,
			"same",
		}
		in.WriteString(strings.Join(fields, "\t") + "\n")
	}

	var enc bytes.Buffer
	rows, reps, err := Encode(strings.NewReader(in.String()), cols, &enc, Options{Workers: 3, ChunkSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 5000 {
		t.Fatalf("rows = %d", rows)
	}
	for _, r := range reps {
		t.Logf("%-4s %-40s %6d -> %6d", r.Column.Name, r.Codec, r.RawBytes, r.EncBytes)
	}
	var dec bytes.Buffer
	if _, err := Decode(enc.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.String() != in.String() {
		a, b := strings.Split(in.String(), "\n"), strings.Split(dec.String(), "\n")
		for i := range min(len(a), len(b)) {
			if a[i] != b[i] {
				t.Fatalf("line %d:\n got %q\nwant %q", i, b[i], a[i])
			}
		}
		t.Fatalf("decoded %d lines, want %d", len(b), len(a))
	}
}

func mustCols(t *testing.T, defs ...string) []schema.Column {
	var cols []schema.Column
	for i, d := range defs {
		name, typ, _ := strings.Cut(d, " ")
		c, err := schema.ParseColumn(i, name, typ)
		if err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	return cols
}
