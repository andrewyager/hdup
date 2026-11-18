package run

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// testProgress returns a progress whose clock is at t0 for the previous
// frame and at t0+step for each next frame.
func testProgress(out *bytes.Buffer, live bool, step time.Duration, decks ...*deckStats) *progress {
	now := t0
	return &progress{out: out, decks: decks, live: live, last: t0, now: func() time.Time {
		now = now.Add(step)
		return now
	}}
}

func TestPrintShowsEachDeckTheTotalAndTheConnections(t *testing.T) {
	out := &bytes.Buffer{}
	hd1 := &deckStats{name: "HD1", total: 10_000_000}
	hd1.read.Store(5_000_000)
	hd1.sent.Store(2_500_000)
	hd1.done.Store(2_500_000)
	hd1.file.Store("a.mov")
	iso := &deckStats{name: "ISO1", total: 10_000_000}
	p := testProgress(out, false, 2*time.Second, hd1, iso)
	p.conns = func() map[string]int { return map[string]int{"10.0.0.1": 2, "10.0.0.2": 3} }
	p.print()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	// 2 500 000 bytes in the 2 s since the previous frame is 10 Mbit/s.
	want := []string{
		"HD1         25.0%  read    20 Mbit/s  sent    10 Mbit/s  a.mov",
		"ISO1         0.0%  read     0 Mbit/s  sent     0 Mbit/s  ",
		"TOTAL       12.5%  read    20 Mbit/s  sent    10 Mbit/s  2.4 MiB of 19.1 MiB",
		"connections: .1=2 .2=3",
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d:\ngot  %q\nwant %q", i, lines[i], w)
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("plain mode has escape codes")
	}
}

func TestPrintInLiveModeRewritesTheLinesFromTheSecondFrame(t *testing.T) {
	out := &bytes.Buffer{}
	p := testProgress(out, true, time.Second, &deckStats{name: "HD1", total: 10})
	p.print()
	first := out.String()
	out.Reset()
	p.print()
	second := out.String()
	if strings.Contains(first, "\x1b") {
		t.Fatalf("the first frame has escape codes: %q", first)
	}
	if !strings.HasPrefix(second, "\x1b[2A") || strings.Count(second, "\x1b[2K") != 2 {
		t.Fatalf("the second frame: %q", second)
	}
}

func TestPrintShowsZeroForANegativeDeltaAndKeepsThePercentInRange(t *testing.T) {
	out := &bytes.Buffer{}
	d := &deckStats{name: "HD1", total: 100}
	d.sent.Store(-50) // a give-back after the previous frame
	d.done.Store(-50)
	p := testProgress(out, false, time.Second, d)
	p.print()
	if !strings.Contains(out.String(), "HD1          0.0%  read     0 Mbit/s  sent     0 Mbit/s") {
		t.Fatalf("output:\n%s", out.String())
	}
	d.done.Store(150)
	out.Reset()
	p.print()
	if !strings.Contains(out.String(), "HD1        100.0%") {
		t.Fatalf("output:\n%s", out.String())
	}
}
