// Command hdup uploads HyperDeck files to Wasabi.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andrewyager/hdup/internal/balance"
	"github.com/andrewyager/hdup/internal/job"
	"github.com/andrewyager/hdup/internal/probe"
	"github.com/andrewyager/hdup/internal/run"
	"github.com/andrewyager/hdup/internal/source"
	"github.com/andrewyager/hdup/internal/upload"
)

const usage = `Usage:
  hdup upload [flags] job.toml   upload the files that the job file selects
  hdup probe job.toml            measure the upload rate to the bucket

Flags for upload:
  --parts-per-deck N   parallel parts for each deck (default 8)
  --part-size N        part size in MiB (default 64)
  --yes                do not ask for confirmation
  --dry-run            show the files and stop`

type usageError string

func (e usageError) Error() string { return string(e) }

var errUsage error = usageError(usage)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second Ctrl-C has the default effect.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code, msg := exit(ctx, cli(ctx, os.Args[1:]))
	if code != 0 {
		fmt.Fprintln(os.Stderr, "hdup:", msg)
		os.Exit(code)
	}
}

// exit gives the exit code and the message for the result of cli. A context
// that has ended means an interrupt, whatever the type of the error.
func exit(ctx context.Context, err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return 130, "interrupted; run the same command again to continue"
	default:
		// An error text can contain a file name from a deck.
		return 1, run.Printable(err.Error())
	}
}

func cli(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "probe":
		if len(args) != 2 {
			return errUsage
		}
		return runProbe(ctx, args[1])
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ContinueOnError)
		partsPerDeck := fs.Int("parts-per-deck", 8, "parallel parts for each deck")
		partMiB := fs.Int64("part-size", 64, "part size in MiB")
		yes := fs.Bool("yes", false, "do not ask for confirmation")
		dryRun := fs.Bool("dry-run", false, "show the files and stop")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || *partsPerDeck < 1 || *partMiB < 5 {
			return errUsage
		}
		return runUpload(ctx, fs.Arg(0), *partsPerDeck, *partMiB<<20, *yes, *dryRun)
	default:
		return errUsage
	}
}

// env is the data that each command gets from the job file and the rclone
// config file.
type env struct {
	job     *job.Job
	remotes job.Remotes
	remote  job.S3Remote
	addrs   []string
}

func setup(ctx context.Context, jobPath string) (*env, error) {
	j, err := job.Load(jobPath, time.Local)
	if err != nil {
		return nil, err
	}
	conf, err := job.ConfigPath()
	if err != nil {
		return nil, err
	}
	remotes, err := job.LoadRemotes(conf)
	if err != nil {
		return nil, err
	}
	remote, err := remotes.S3(j.Remote)
	if err != nil {
		return nil, err
	}
	addrs, err := balance.Resolve(ctx, remote.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", remote.Endpoint, err)
	}
	fmt.Printf("%s has %d addresses\n", remote.Endpoint, len(addrs))
	return &env{job: j, remotes: remotes, remote: remote, addrs: addrs}, nil
}

func runProbe(ctx context.Context, jobPath string) error {
	e, err := setup(ctx, jobPath)
	if err != nil {
		return err
	}
	return probe.Run(ctx, probe.Config{
		Bucket:   e.job.Bucket,
		Addrs:    e.addrs,
		Duration: 20 * time.Second,
		PartSize: 4 << 20,
		NewAPI: func(addrs []string, conns int) upload.API {
			return upload.NewClient(e.remote, balance.New(addrs).Client(conns))
		},
		Out: os.Stdout,
	})
}

func runUpload(ctx context.Context, jobPath string, partsPerDeck int, partSize int64, yes, dryRun bool) error {
	e, err := setup(ctx, jobPath)
	if err != nil {
		return err
	}
	var decks []run.Deck
	for _, d := range e.job.Decks {
		r, err := e.remotes.FTP(d.Remote)
		if err != nil {
			return err
		}
		decks = append(decks, run.Deck{
			Name:    d.Remote,
			Include: d.Include,
			Src:     &source.FTP{Addr: r.Addr, User: r.User, Pass: r.Pass, Root: d.Path},
		})
	}
	bal := balance.New(e.addrs)
	info, _ := os.Stdout.Stat()
	live := info != nil && info.Mode()&os.ModeCharDevice != 0
	interval := 10 * time.Second
	if live {
		interval = time.Second
	}
	_, err = run.Run(ctx, run.Config{
		Decks: decks,
		From:  e.job.FromTime,
		To:    e.job.ToTime,
		Uploader: upload.Uploader{
			API:         upload.NewClient(e.remote, bal.Client(len(decks)*partsPerDeck)),
			Bucket:      e.job.Bucket,
			PartSize:    partSize,
			Workers:     partsPerDeck,
			Retries:     10,
			Backoff:     time.Second,
			IdleTimeout: 3 * time.Minute,
		},
		Yes:      yes,
		DryRun:   dryRun,
		In:       os.Stdin,
		Out:      os.Stdout,
		Conns:    bal.Counts,
		Interval: interval,
		Live:     live,
	})
	return err
}
