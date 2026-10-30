package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"slices"
	"time"

	"hasd/lab3/internal/schema"
	"hasd/lab3/internal/textval"
	"hasd/lab3/internal/tsv"
)

type hashEntry struct {
	hash  uint64
	multi bool
}

type hashCheck struct {
	seen      map[string]hashEntry
	rows      uint64
	conflicts uint64
}

func (h *hashCheck) add(key, hash []byte) {
	v, _ := textval.ParseUint(hash)
	h.rows++
	e, ok := h.seen[string(key)]
	switch {
	case !ok:
		h.seen[string(key)] = hashEntry{hash: v}
	case e.hash != v:
		h.conflicts++
		if !e.multi {
			h.seen[string(key)] = hashEntry{hash: e.hash, multi: true}
		}
	}
}

func (h *hashCheck) print(w io.Writer, key, hash string) {
	multi := 0
	for _, e := range h.seen {
		if e.multi {
			multi++
		}
	}
	fmt.Fprintf(w, "%s по %s\n", hash, key)
	fmt.Fprintf(w, "  различных %s: %d\n", key, len(h.seen))
	fmt.Fprintf(w, "  %s, у которых больше одного %s: %d (%s)\n", key, hash, multi, pct(uint64(multi), uint64(len(h.seen))))
	fmt.Fprintf(w, "  строк, где %s не равен первому для этого %s: %d (%s)\n\n", hash, key, h.conflicts, pct(h.conflicts, h.rows))
}

type domainCheck struct {
	rows, same, wwwKept, wwwStripped, other uint64
	samples                                 [][2]string
}

var www = []byte("www.")

func host(url []byte) []byte {
	if i := bytes.Index(url, []byte("://")); i >= 0 {
		url = url[i+3:]
	}
	if i := bytes.IndexAny(url, "/?#"); i >= 0 {
		url = url[:i]
	}
	return url
}

func (d *domainCheck) add(url, domain []byte) {
	d.rows++
	h := host(url)
	switch {
	case !bytes.HasPrefix(h, www) && bytes.Equal(h, domain):
		d.same++
	case bytes.HasPrefix(h, www) && bytes.Equal(h, domain):
		d.wwwKept++
	case bytes.HasPrefix(h, www) && bytes.Equal(h[len(www):], domain):
		d.wwwStripped++
	default:
		d.other++
		if len(d.samples) < 5 {
			d.samples = append(d.samples, [2]string{string(url), string(domain)})
		}
	}
}

func (d *domainCheck) print(w io.Writer, domain, url string) {
	fmt.Fprintf(w, "%s из %s (хост – между :// и первым / ? #)\n", domain, url)
	fmt.Fprintf(w, "  хост без www., %s = хост: %s\n", domain, pct(d.same, d.rows))
	fmt.Fprintf(w, "  хост с www., %s = хост: %s\n", domain, pct(d.wwwKept, d.rows))
	fmt.Fprintf(w, "  хост с www., %s = хост без www.: %s\n", domain, pct(d.wwwStripped, d.rows))
	fmt.Fprintf(w, "  не совпало: %d (%s)\n", d.other, pct(d.other, d.rows))
	for _, s := range d.samples {
		fmt.Fprintf(w, "    %.80q -> %q\n", s[0], s[1])
	}
	fmt.Fprintln(w)
}

type offsetCheck struct {
	hist                 map[int64]uint64
	last                 map[uint64]int64
	rows, invalid        uint64
	userSeen, sameAsUser uint64
	sameAsPrevRow        uint64
	prevRow              int64
	hasPrevRow           bool
}

func (o *offsetCheck) add(user uint64, a, b []byte) {
	o.rows++
	ta, okA := textval.ParseDateTime(a)
	tb, okB := textval.ParseDateTime(b)
	if !okA || !okB {
		o.invalid++
		o.hasPrevRow = false
		return
	}
	d := ta - tb
	o.hist[d]++
	if p, ok := o.last[user]; ok {
		o.userSeen++
		if p == d {
			o.sameAsUser++
		}
	}
	o.last[user] = d
	if o.hasPrevRow && o.prevRow == d {
		o.sameAsPrevRow++
	}
	o.prevRow, o.hasPrevRow = d, true
}

func (o *offsetCheck) print(w io.Writer, title string) {
	fmt.Fprintln(w, title)
	keys := slices.SortedFunc(maps.Keys(o.hist), func(a, b int64) int {
		return cmp.Compare(o.hist[b], o.hist[a])
	})
	fmt.Fprintf(w, "  различных значений: %d, не разобрано: %d\n", len(keys), o.invalid)
	for _, k := range keys[:min(5, len(keys))] {
		fmt.Fprintf(w, "    %8s  %s\n", offset(k), pct(o.hist[k], o.rows))
	}
	fmt.Fprintf(w, "  как у прошлого хита того же пользователя: %s\n", pct(o.sameAsUser, o.userSeen))
	fmt.Fprintf(w, "  как в предыдущей строке файла: %s\n\n", pct(o.sameAsPrevRow, o.rows))
}

type refererCheck struct {
	lastURL                                             map[uint64]string
	prevURL                                             []byte
	rows, nonEmpty, userSeen, sameAsUser, sameAsPrevRow uint64
}

func (r *refererCheck) add(user uint64, url, ref []byte) {
	r.rows++
	if len(ref) > 0 {
		r.nonEmpty++
		if last, ok := r.lastURL[user]; ok {
			r.userSeen++
			if last == string(ref) {
				r.sameAsUser++
			}
		}
		if bytes.Equal(ref, r.prevURL) {
			r.sameAsPrevRow++
		}
	}
	if last, ok := r.lastURL[user]; !ok || last != string(url) {
		r.lastURL[user] = string(url)
	}
	r.prevURL = append(r.prevURL[:0], url...)
}

func (r *refererCheck) print(w io.Writer) {
	fmt.Fprintln(w, "Referer = URL прошлого хита")
	fmt.Fprintf(w, "  строк с непустым Referer: %d (%s)\n", r.nonEmpty, pct(r.nonEmpty, r.rows))
	fmt.Fprintf(w, "  из них у пользователя уже был хит: %d\n", r.userSeen)
	fmt.Fprintf(w, "    Referer = URL прошлого хита того же пользователя: %s\n", pct(r.sameAsUser, r.userSeen))
	fmt.Fprintf(w, "  Referer = URL предыдущей строки файла: %s от непустых\n\n", pct(r.sameAsPrevRow, r.nonEmpty))
}

func pct(a, b uint64) string {
	if b == 0 {
		return "–"
	}
	return fmt.Sprintf("%.2f%%", float64(a)/float64(b)*100)
}

func offset(d int64) string {
	switch {
	case d%3600 == 0:
		return fmt.Sprintf("%+dч", d/3600)
	case d%60 == 0:
		return fmt.Sprintf("%+dмин", d/60)
	default:
		return fmt.Sprintf("%+dс", d)
	}
}

func main() {
	schemaPath := flag.String("schema", "schema.txt", "column list, one \"<name> <ClickHouse type>\" per line")
	chunkMB := flag.Int("chunk", 32, "read chunk size in MiB")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: hypotheses [flags] <file.tsv>")
	}

	cols, err := schema.Load(*schemaPath)
	if err != nil {
		log.Fatal(err)
	}
	idx := make(map[string]int, len(cols))
	for _, c := range cols {
		idx[c.Name] = c.Index
	}
	col := func(name string) int {
		i, ok := idx[name]
		if !ok {
			log.Fatalf("no column %s in %s", name, *schemaPath)
		}
		return i
	}
	var (
		cURL, cURLHash, cURLDomain             = col("URL"), col("URLHash"), col("URLDomain")
		cReferer, cRefererHash, cRefererDomain = col("Referer"), col("RefererHash"), col("RefererDomain")
		cEventTime, cEventDate                 = col("EventTime"), col("EventDate")
		cUTCEventTime, cClientEventTime        = col("UTCEventTime"), col("ClientEventTime")
		cUserID                                = col("UserID")
	)

	f, err := os.Open(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	urlHash := hashCheck{seen: map[string]hashEntry{}}
	refererHash := hashCheck{seen: map[string]hashEntry{}}
	var urlDomain, refererDomain domainCheck
	var dateRows, dateMismatch uint64
	utc := offsetCheck{hist: map[int64]uint64{}, last: map[uint64]int64{}}
	client := offsetCheck{hist: map[int64]uint64{}, last: map[uint64]int64{}}
	referer := refererCheck{lastURL: map[uint64]string{}}

	start := time.Now()
	rd := tsv.NewReader(f, len(cols), *chunkMB<<20)
	var b tsv.Batch
	var rows, size uint64
	for {
		ok, err := rd.Next(&b)
		if err != nil {
			log.Fatal(err)
		}
		if !ok {
			break
		}
		rows += uint64(b.Rows)
		size += uint64(len(b.Buf))
		for r := range b.Rows {
			url, referrer := b.Field(r, cURL), b.Field(r, cReferer)
			user, _ := textval.ParseUint(b.Field(r, cUserID))
			eventTime := b.Field(r, cEventTime)

			urlHash.add(url, b.Field(r, cURLHash))
			refererHash.add(referrer, b.Field(r, cRefererHash))
			urlDomain.add(url, b.Field(r, cURLDomain))
			refererDomain.add(referrer, b.Field(r, cRefererDomain))

			dateRows++
			if len(eventTime) < 10 || !bytes.Equal(eventTime[:10], b.Field(r, cEventDate)) {
				dateMismatch++
			}

			utc.add(user, eventTime, b.Field(r, cUTCEventTime))
			client.add(user, b.Field(r, cClientEventTime), eventTime)
			referer.add(user, url, referrer)
		}
	}

	w := os.Stdout
	fmt.Fprintf(w, "%d строк, %.2f ГБ, %v\n\n", rows, float64(size)/1e9, time.Since(start).Round(time.Second))
	urlHash.print(w, "URL", "URLHash")
	refererHash.print(w, "Referer", "RefererHash")
	urlDomain.print(w, "URLDomain", "URL")
	refererDomain.print(w, "RefererDomain", "Referer")
	fmt.Fprintf(w, "EventDate = дата из EventTime\n  не совпало: %d (%s)\n\n", dateMismatch, pct(dateMismatch, dateRows))
	utc.print(w, "EventTime − UTCEventTime")
	client.print(w, "ClientEventTime − EventTime")
	referer.print(w)
}
