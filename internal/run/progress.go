package run

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type deckStats struct {
	name  string
	total int64
	read  atomic.Int64 // bytes read from the deck
	sent  atomic.Int64 // bytes sent to S3
	done  atomic.Int64 // bytes sent, resumed or skipped
	file  atomic.Value // string: the current file
	cur   atomic.Pointer[fileState]

	lastRead, lastSent int64
}

type progress struct {
	out   io.Writer
	decks []*deckStats
	conns func() map[string]int
	live  bool // rewrite the lines in place with ANSI codes
	lines int
	now   func() time.Time
	last  time.Time // the time of the previous frame
}

// mbit is 0 for a negative byte count, which a give-back can cause.
func mbit(bytes int64, d time.Duration) float64 {
	if bytes <= 0 || d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / 1e6 / d.Seconds()
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// print writes one line for each deck, a total line and the connections line.
// The rates are for the time since the previous frame.
func (p *progress) print() {
	now := p.now()
	interval := now.Sub(p.last)
	p.last = now
	var lines []string
	var total, done, readDelta, sentDelta int64
	for _, d := range p.decks {
		read, sent := d.read.Load(), d.sent.Load()
		rd, sd := read-d.lastRead, sent-d.lastSent
		d.lastRead, d.lastSent = read, sent
		total += d.total
		done += d.done.Load()
		readDelta += rd
		sentDelta += sd
		file, _ := d.file.Load().(string)
		lines = append(lines, fmt.Sprintf("%-10s %5.1f%%  read %5.0f Mbit/s  sent %5.0f Mbit/s  %s",
			d.name, percent(d.done.Load(), d.total), mbit(rd, interval), mbit(sd, interval), Printable(file)))
	}
	lines = append(lines, fmt.Sprintf("%-10s %5.1f%%  read %5.0f Mbit/s  sent %5.0f Mbit/s  %s of %s",
		"TOTAL", percent(done, total), mbit(readDelta, interval), mbit(sentDelta, interval), human(done), human(total)))
	if p.conns != nil {
		lines = append(lines, "connections: "+connsLine(p.conns()))
	}
	rewrite := p.live && p.lines > 0
	if rewrite {
		fmt.Fprintf(p.out, "\x1b[%dA", p.lines) // cursor up
	}
	for _, l := range lines {
		if rewrite {
			fmt.Fprint(p.out, "\x1b[2K") // erase the line
		}
		fmt.Fprintln(p.out, l)
	}
	p.lines = len(lines)
}

// Printable quotes a string that has control or non-printable characters.
func Printable(s string) string {
	for _, r := range s {
		if !strconv.IsPrint(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}

func percent(done, total int64) float64 {
	if total == 0 {
		return 100
	}
	return min(max(float64(done)*100/float64(total), 0), 100)
}

// connsLine shows the last number of each address, for example ".100=4 .101=4".
func connsLine(counts map[string]int) string {
	addrs := make([]string, 0, len(counts))
	for a := range counts {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		short := a
		if i := strings.LastIndex(a, "."); i >= 0 {
			short = a[i:]
		}
		parts[i] = fmt.Sprintf("%s=%d", short, counts[a])
	}
	return strings.Join(parts, " ")
}

// loop calls tick at each interval until stop is closed.
func loop(interval time.Duration, stop <-chan struct{}, tick func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			tick()
		case <-stop:
			return
		}
	}
}
