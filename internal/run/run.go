// Package run shows the files, asks for confirmation, uploads all decks in
// parallel and prints a summary.
package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewyager/hdup/internal/source"
	"github.com/andrewyager/hdup/internal/upload"
)

// Target is one file of an explicit file list.
type Target struct {
	Path string // full path on the deck
	Key  string // full object key
}

type Deck struct {
	Name    string
	Src     source.Source
	Include []string
	// Files is an explicit file list. If it is not nil, the date window and
	// Include do not apply, and each file goes to the key of its Target.
	Files []Target
}

type Config struct {
	Decks    []Deck
	From, To time.Time
	Uploader upload.Uploader // Run sets the progress hooks on a copy for each deck
	Yes      bool
	DryRun   bool
	JSON     bool // write JSON lines to Out, and no text
	In       io.Reader
	Out      io.Writer
	Conns    func() map[string]int // optional: open connections for each address
	Interval time.Duration         // time between progress reports
	Live     bool                  // Out is a terminal
}

type Summary struct {
	Uploaded    int
	Skipped     int
	Failed      []string
	Interrupted int   // files that a cancel stopped
	Bytes       int64 // bytes of the files that were uploaded
	Sent        int64 // bytes that this run sent; resumed parts are not included
	Elapsed     time.Duration
}

var ErrAborted = errors.New("aborted")

// FailedError is the result of a run in which files failed. It holds the
// number of files.
type FailedError int

func (e FailedError) Error() string {
	return fmt.Sprintf("%d files failed; run the same command again to continue", int(e))
}

// item is a file to upload and its object key.
type item struct {
	file source.File
	key  string
}

// fileState is the progress of the file that a deck uploads.
type fileState struct {
	item
	done atomic.Int64 // bytes sent or resumed
}

type deckWork struct {
	deck   Deck
	items  []item
	failed []fileEvent // files of an explicit list that the deck could not give
	err    error
	stats  *deckStats
}

// plan finds the files of a deck and their keys.
func (w *deckWork) plan(ctx context.Context, from, to time.Time) {
	d := w.deck
	if d.Files == nil {
		all, err := d.Src.List(ctx)
		if err != nil {
			w.err = fmt.Errorf("%s: list: %w", d.Name, err)
			return
		}
		for _, f := range source.Select(all, from, to, d.Include) {
			w.items = append(w.items, item{file: f, key: d.Name + "/" + f.Path})
		}
		return
	}
	paths := make([]string, len(d.Files))
	for i, t := range d.Files {
		paths[i] = t.Path
	}
	res, err := d.Src.Stat(ctx, paths)
	if w.err = ctx.Err(); w.err != nil {
		return
	}
	for i, t := range d.Files {
		switch {
		case err != nil:
			w.fail(t, fmt.Errorf("deck %s does not answer: %w", d.Name, err))
		case res[i].Err != nil:
			w.fail(t, res[i].Err)
		default:
			w.items = append(w.items, item{file: res[i].File, key: t.Key})
		}
	}
}

func (w *deckWork) fail(t Target, err error) {
	w.failed = append(w.failed, fileEvent{Event: "file_failed", Deck: w.deck.Name, Path: t.Path, Key: t.Key,
		Error: fmt.Sprintf("%s: %s", t.Key, err)})
}

func Run(ctx context.Context, cfg Config) (Summary, error) {
	// In JSON mode, the text for a person goes nowhere.
	out := cfg.Out
	var ev *events
	if cfg.JSON {
		ev, out = &events{w: cfg.Out}, io.Discard
	}

	work := make([]*deckWork, len(cfg.Decks))
	var planning sync.WaitGroup
	for i, d := range cfg.Decks {
		work[i] = &deckWork{deck: d, stats: &deckStats{name: d.Name}}
		planning.Go(func() { work[i].plan(ctx, cfg.From, cfg.To) })
	}
	planning.Wait()

	var sum Summary
	var count int
	var total int64
	for _, w := range work {
		if w.err != nil {
			return Summary{}, w.err
		}
		for _, it := range w.items {
			name := w.deck.Name + "/" + it.file.Path
			if w.deck.Files != nil {
				name += " -> " + it.key
			}
			fmt.Fprintf(out, "%s  %10s  %s\n", it.file.ModTime.Local().Format(time.DateTime), human(it.file.Size), Printable(name))
			w.stats.total += it.file.Size
		}
		count += len(w.items)
		total += w.stats.total
	}
	fmt.Fprintf(out, "%d files, %s\n", count, human(total))
	ev.emit(planEvent{Event: "plan", Files: count, Bytes: total})
	if cfg.DryRun {
		return Summary{}, nil
	}
	for _, w := range work {
		for _, f := range w.failed {
			sum.Failed = append(sum.Failed, f.Error)
			ev.emit(f)
		}
	}
	if count == 0 {
		if len(sum.Failed) == 0 && ev == nil {
			return Summary{}, nil
		}
		return finish(ctx, out, ev, sum)
	}
	if !cfg.Yes {
		fmt.Fprint(out, "Proceed with the upload? [y/N] ")
		lineCh := make(chan string, 1)
		go func() {
			answer, _ := bufio.NewReader(cfg.In).ReadString('\n')
			lineCh <- answer
		}()
		select {
		case <-ctx.Done():
			return Summary{}, ctx.Err()
		case answer := <-lineCh:
			if a := strings.TrimSpace(answer); a != "y" && a != "Y" {
				return Summary{}, ErrAborted
			}
		}
	}

	p := &progress{out: out, conns: cfg.Conns, live: cfg.Live, now: time.Now, last: time.Now()}
	for _, w := range work {
		p.decks = append(p.decks, w.stats)
	}
	tick := p.print
	if ev != nil {
		tick = func() {
			for _, d := range p.decks {
				if fs := d.cur.Load(); fs != nil {
					ev.emit(fs.event(d.name, "file_progress", true))
				}
			}
		}
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		loop(cfg.Interval, stop, tick)
		close(stopped)
	}()

	start := time.Now()
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, w := range work {
		wg.Go(func() {
			st := w.stats
			for i := range w.items {
				if ctx.Err() != nil {
					return
				}
				fs := &fileState{item: w.items[i]}
				f := fs.file
				up := cfg.Uploader
				up.Read = func(n int64) { st.read.Add(n) }
				up.Sent = func(n int64) { st.sent.Add(n); st.done.Add(n); fs.done.Add(n) }
				up.Resumed = func(n int64) { st.done.Add(n); fs.done.Add(n) }
				st.file.Store(f.Path)
				st.cur.Store(fs)
				ev.emit(fs.event(st.name, "file_start", false))
				res, err := up.Upload(ctx, w.deck.Src, f, fs.key)
				st.cur.Store(nil)
				end := fs.event(st.name, "file_done", false)
				mu.Lock()
				switch {
				case err != nil && ctx.Err() != nil:
					sum.Interrupted++
					end.Event = "file_interrupted"
				case err != nil:
					sum.Failed = append(sum.Failed, err.Error())
					end.Event, end.Error = "file_failed", err.Error()
				case res == upload.Skipped:
					sum.Skipped++
					st.done.Add(f.Size)
					end.Result = "skipped"
				default:
					sum.Uploaded++
					sum.Bytes += f.Size
					end.Result = "uploaded"
				}
				mu.Unlock()
				ev.emit(end)
			}
		})
	}
	wg.Wait()
	close(stop)
	<-stopped
	sum.Elapsed = time.Since(start)
	p.print()
	for _, w := range work {
		sum.Sent += w.stats.sent.Load()
	}
	return finish(ctx, out, ev, sum)
}

// event makes an event for the file. withDone adds the bytes that are done.
func (fs *fileState) event(deck, name string, withDone bool) fileEvent {
	e := fileEvent{Event: name, Deck: deck, Path: fs.file.Path, Key: fs.key, Size: &fs.file.Size}
	if withDone {
		// A try that fails gives back its bytes, so keep the value in range.
		done := min(max(fs.done.Load(), 0), fs.file.Size)
		e.Done = &done
	}
	return e
}

// finish writes the summary and gives the result of the run.
func finish(ctx context.Context, out io.Writer, ev *events, sum Summary) (Summary, error) {
	fmt.Fprintf(out, "\nUploaded %d files (%s), skipped %d, failed %d, in %s, average %.0f Mbit/s\n",
		sum.Uploaded, human(sum.Bytes), sum.Skipped, len(sum.Failed),
		sum.Elapsed.Round(time.Second), mbit(sum.Sent, sum.Elapsed))
	if sum.Interrupted > 0 {
		fmt.Fprintf(out, "Interrupted %d files; run the same command again to continue\n", sum.Interrupted)
	}
	for _, f := range sum.Failed {
		fmt.Fprintln(out, "FAILED:", Printable(f))
	}
	ev.emit(summaryEvent{Event: "summary", Uploaded: sum.Uploaded, Skipped: sum.Skipped, Failed: len(sum.Failed),
		Interrupted: sum.Interrupted, Bytes: sum.Bytes, Sent: sum.Sent, ElapsedS: sum.Elapsed.Seconds()})
	if sum.Interrupted > 0 || ctx.Err() != nil {
		return sum, ctx.Err()
	}
	if len(sum.Failed) > 0 {
		return sum, FailedError(len(sum.Failed))
	}
	return sum, nil
}
