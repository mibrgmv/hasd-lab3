// Package schema describes the columns of the dataset: one "<name> <type>"
// line per column, types written as in ClickHouse.
package schema

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Kind int

const (
	Int Kind = iota
	DateTime
	Date
	String
	FixedString
	Array
)

func (k Kind) String() string {
	return [...]string{"Int", "DateTime", "Date", "String", "FixedString", "Array"}[k]
}

type Column struct {
	Index    int
	Name     string
	Type     string // ClickHouse type as written in the schema file
	Kind     Kind
	Signed   bool   // Int only
	Bits     int    // Int only: 8, 16, 32 or 64
	FixedLen int    // FixedString only
	Elem     string // Array only: element type
}

func Load(path string) ([]Column, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cols []Column
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, typ, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("schema: line %q: want \"<name> <type>\"", line)
		}
		c, err := ParseColumn(len(cols), name, strings.TrimSpace(typ))
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, sc.Err()
}

func ParseColumn(index int, name, typ string) (Column, error) {
	c, err := parseType(typ)
	if err != nil {
		return Column{}, fmt.Errorf("schema: column %s: %w", name, err)
	}
	c.Index, c.Name, c.Type = index, name, typ
	return c, nil
}

func parseType(t string) (Column, error) {
	switch {
	case t == "DateTime":
		return Column{Kind: DateTime}, nil
	case t == "Date":
		return Column{Kind: Date}, nil
	case t == "String":
		return Column{Kind: String}, nil
	case strings.HasPrefix(t, "FixedString(") && strings.HasSuffix(t, ")"):
		n, err := strconv.Atoi(t[len("FixedString(") : len(t)-1])
		if err != nil || n <= 0 {
			return Column{}, fmt.Errorf("bad type %q", t)
		}
		return Column{Kind: FixedString, FixedLen: n}, nil
	case strings.HasPrefix(t, "Array(") && strings.HasSuffix(t, ")"):
		return Column{Kind: Array, Elem: t[len("Array(") : len(t)-1]}, nil
	case strings.HasPrefix(t, "UInt"), strings.HasPrefix(t, "Int"):
		signed := strings.HasPrefix(t, "Int")
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(t, "U"), "Int"))
		if err != nil || (n != 8 && n != 16 && n != 32 && n != 64) {
			return Column{}, fmt.Errorf("bad type %q", t)
		}
		return Column{Kind: Int, Signed: signed, Bits: n}, nil
	}
	return Column{}, fmt.Errorf("unsupported type %q", t)
}
