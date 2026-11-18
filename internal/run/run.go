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
	"time"

	"github.com/andrewyager/hdup/internal/source"
	"github.com/andrewyager/hdup/internal/upload"
)

type Deck struct {
	Name    string
	Src     source.Source
	Include []string
}

type Config struct {
	Decks    []Deck
	From, To time.Time
	Uploader upload.Uploader // Run sets the progress hooks on a copy for each deck
	Yes      bool
	DryRun   bool
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

type deckWork struct {
	deck  Deck
	files []source.File
	stats *deckStats
}

func Run(ctx context.Context, cfg Config) (Summary, error) {
	var work []deckWork
	var count int
	var total int64
	for _, d := range cfg.Decks {
		all, err := d.Src.List(ctx)
		if err != nil {
			return Summary{}, fmt.Errorf("%s: list: %w", d.Name, err)
		}
		files := source.Select(all, cfg.From, cfg.To, d.Include)
		st := &deckStats{name: d.Name}
		for _, f := range files {
			fmt.Fprintf(cfg.Out, "%s  %10s  %s/%s\n", f.ModTime.Local().Format(time.DateTime), human(f.Size), d.Name, Printable(f.Path))
			st.total += f.Size
		}
		count += len(files)
		total += st.total
		work = append(work, deckWork{deck: d, files: files, stats: st})
	}
	fmt.Fprintf(cfg.Out, "%d files, %s\n", count, human(total))
	if count == 0 || cfg.DryRun {
		return Summary{}, nil
	}
	if !cfg.Yes {
		fmt.Fprint(cfg.Out, "Proceed with the upload? [y/N] ")
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

	p := &progress{out: cfg.Out, conns: cfg.Conns, live: cfg.Live, now: time.Now, last: time.Now()}
	for _, w := range work {
		p.decks = append(p.decks, w.stats)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		p.loop(cfg.Interval, stop)
		close(stopped)
	}()

	start := time.Now()
	var (
		mu  sync.Mutex
		sum Summary
		wg  sync.WaitGroup
	)
	for _, w := range work {
		wg.Go(func() {
			st := w.stats
			up := cfg.Uploader
			up.Read = func(n int64) { st.read.Add(n) }
			up.Sent = func(n int64) { st.sent.Add(n); st.done.Add(n) }
			up.Resumed = func(n int64) { st.done.Add(n) }
			for _, f := range w.files {
				if ctx.Err() != nil {
					return
				}
				st.file.Store(f.Path)
				key := w.deck.Name + "/" + f.Path
				res, err := up.Upload(ctx, w.deck.Src, f, key)
				mu.Lock()
				switch {
				case err != nil && ctx.Err() != nil:
					sum.Interrupted++
				case err != nil:
					sum.Failed = append(sum.Failed, err.Error())
				case res == upload.Skipped:
					sum.Skipped++
					st.done.Add(f.Size)
				default:
					sum.Uploaded++
					sum.Bytes += f.Size
				}
				mu.Unlock()
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

	fmt.Fprintf(cfg.Out, "\nUploaded %d files (%s), skipped %d, failed %d, in %s, average %.0f Mbit/s\n",
		sum.Uploaded, human(sum.Bytes), sum.Skipped, len(sum.Failed),
		sum.Elapsed.Round(time.Second), mbit(sum.Sent, sum.Elapsed))
	if sum.Interrupted > 0 {
		fmt.Fprintf(cfg.Out, "Interrupted %d files; run the same command again to continue\n", sum.Interrupted)
	}
	for _, f := range sum.Failed {
		fmt.Fprintln(cfg.Out, "FAILED:", Printable(f))
	}
	if sum.Interrupted > 0 || ctx.Err() != nil {
		return sum, ctx.Err()
	}
	if len(sum.Failed) > 0 {
		return sum, fmt.Errorf("%d files failed; run the same command again to continue", len(sum.Failed))
	}
	return sum, nil
}
