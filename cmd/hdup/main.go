// Command hdup uploads HyperDeck files to S3 storage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
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
  hdup probe [flags] job.toml    measure the upload rate to the bucket
  hdup version                   show the version

Flag for upload and probe:
  --config FILE        hdup config file; without it, hdup reads the rclone config

Flags for upload:
  --parts-per-deck N   parallel parts for each deck (default 8)
  --part-size N        part size in MiB (default 64)
  --retries N          tries for each read, send and storage call (default 10)
  --bwlimit RATE       limit the total send rate, for example 10M for 10 MiB/s;
                       suffix B, K, M or G, and no suffix means KiB/s
  --yes                do not ask for confirmation
  --dry-run            show the files and stop
  --progress MODE      text (default) or json: one JSON object on each line
  --stdin-stop         stop when the standard input closes`

type usageError string

func (e usageError) Error() string { return string(e) }

var errUsage error = usageError(usage)

// version is set at link time for a release build.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

type stdio struct {
	in       io.Reader
	out, err io.Writer
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second Ctrl-C has the default effect.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code, msg := exit(ctx, cli(ctx, os.Args[1:], stdio{in: os.Stdin, out: os.Stdout, err: os.Stderr}))
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

type uploadOpts struct {
	job, config  string
	partsPerDeck int
	partSize     int64
	retries      int
	bwlimit      int64 // bytes for each second; 0 is no limit
	yes, dryRun  bool
	json         bool
}

func cli(ctx context.Context, args []string, std stdio) error {
	if len(args) == 0 {
		return errUsage
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(std.err)
	fs.Usage = func() {}
	config := fs.String("config", "", "hdup config file")
	switch args[0] {
	case "version":
		fmt.Fprintln(std.out, "hdup", buildVersion())
		return nil
	case "probe":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			return errUsage
		}
		return runProbe(ctx, fs.Arg(0), *config, std)
	case "upload":
		partsPerDeck := fs.Int("parts-per-deck", 8, "parallel parts for each deck")
		partMiB := fs.Int64("part-size", 64, "part size in MiB")
		retries := fs.Int("retries", 10, "tries for each read, send and storage call")
		yes := fs.Bool("yes", false, "do not ask for confirmation")
		dryRun := fs.Bool("dry-run", false, "show the files and stop")
		progress := fs.String("progress", "text", "text or json")
		stdinStop := fs.Bool("stdin-stop", false, "stop when the standard input closes")
		bwlimit := fs.String("bwlimit", "", "limit the total send rate")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 || *partsPerDeck < 1 || *partMiB < 5 || *retries < 1 ||
			(*progress != "text" && *progress != "json") {
			return errUsage
		}
		rate, err := parseRate(*bwlimit)
		if err != nil {
			return errUsage
		}
		o := uploadOpts{job: fs.Arg(0), config: *config, partsPerDeck: *partsPerDeck, partSize: *partMiB << 20,
			retries: *retries, bwlimit: rate, dryRun: *dryRun, json: *progress == "json"}
		// The two modes that a program uses cannot ask a question.
		o.yes = *yes || o.json || *stdinStop
		if *stdinStop {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			go stopOnEOF(std.in, cancel)
		}
		err = runUpload(ctx, o, std)
		var failed run.FailedError
		if o.json && err != nil && ctx.Err() == nil && !errors.As(err, &failed) {
			// The run stopped before it could report on each file.
			run.EmitError(std.out, err)
		}
		return err
	default:
		return errUsage
	}
}

// parseRate reads a rate in the rclone format for one rate: a number and an
// optional suffix B, K, M or G, with binary multiples. A number with no
// suffix is in KiB. The result is in bytes for each second. "off", "0" and
// "" give 0, which is no limit.
func parseRate(s string) (int64, error) {
	if s == "" || s == "off" {
		return 0, nil
	}
	shift := 10
	if i := strings.IndexByte("BKMG", s[len(s)-1]&^0x20); i >= 0 {
		shift, s = 10*i, s[:len(s)-1]
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			return 0, fmt.Errorf("incorrect rate")
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	rate := n * float64(int64(1)<<shift)
	if rate != 0 && rate < 1 || rate > 1<<50 {
		return 0, fmt.Errorf("incorrect rate")
	}
	return int64(rate), nil
}

// stopOnEOF calls cancel when in closes. It ignores the data.
func stopOnEOF(in io.Reader, cancel func()) {
	io.Copy(io.Discard, in)
	cancel()
}

// remotes gives the remotes that a job file names.
type remotes interface {
	S3(name string) (job.S3Remote, error)
	FTP(name string) (job.FTPRemote, error)
}

// env is the data that each command gets from the job file and from the hdup
// config file or the rclone config file.
type env struct {
	job     *job.Job
	remotes remotes
	remote  job.S3Remote
	addrs   []string
}

// setup reads the job file and the config. If configPath is empty, it reads
// the rclone config file. It writes text for a person to info.
func setup(ctx context.Context, jobPath, configPath string, info io.Writer) (*env, error) {
	j, err := job.Load(jobPath, time.Local)
	if err != nil {
		return nil, err
	}
	var rs remotes
	if configPath != "" {
		rs, err = job.LoadConfig(configPath)
	} else {
		var conf string
		if conf, err = job.ConfigPath(); err == nil {
			rs, err = job.LoadRemotes(conf)
		}
	}
	if err != nil {
		return nil, err
	}
	remote, err := rs.S3(j.Remote)
	if err != nil {
		return nil, err
	}
	addrs, err := balance.Resolve(ctx, remote.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", remote.Endpoint, err)
	}
	fmt.Fprintf(info, "%s has %d addresses\n", remote.Endpoint, len(addrs))
	return &env{job: j, remotes: rs, remote: remote, addrs: addrs}, nil
}

func runProbe(ctx context.Context, jobPath, configPath string, std stdio) error {
	e, err := setup(ctx, jobPath, configPath, std.out)
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
		Out: std.out,
	})
}

// deckFiles is the part of an explicit file list that is on one deck.
type deckFiles struct {
	name  string
	files []run.Target
}

// groupFiles gives the files of each deck. The decks and the files keep the
// sequence of the list.
func groupFiles(files []job.File) []deckFiles {
	var out []deckFiles
	index := map[string]int{}
	for _, f := range files {
		i, ok := index[f.Deck]
		if !ok {
			i = len(out)
			index[f.Deck] = i
			out = append(out, deckFiles{name: f.Deck})
		}
		out[i].files = append(out[i].files, run.Target{Path: f.Path, Key: f.Key})
	}
	return out
}

func runUpload(ctx context.Context, o uploadOpts, std stdio) error {
	// In JSON mode, the standard output has only JSON lines.
	info := std.out
	if o.json {
		info = std.err
	}
	e, err := setup(ctx, o.job, o.config, info)
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
	for _, d := range groupFiles(e.job.Files) {
		r, err := e.remotes.FTP(d.name)
		if err != nil {
			return err
		}
		decks = append(decks, run.Deck{
			Name:  d.name,
			Files: d.files,
			Src:   &source.FTP{Addr: r.Addr, User: r.User, Pass: r.Pass},
		})
	}
	bal := balance.New(e.addrs)
	var limit *upload.Limiter
	if o.bwlimit > 0 {
		limit = upload.NewLimiter(o.bwlimit)
	}
	live := false
	if f, ok := std.out.(*os.File); ok && !o.json {
		stat, _ := f.Stat()
		live = stat != nil && stat.Mode()&os.ModeCharDevice != 0
	}
	interval := 10 * time.Second
	if live || o.json {
		interval = time.Second
	}
	_, err = run.Run(ctx, run.Config{
		Decks: decks,
		From:  e.job.FromTime,
		To:    e.job.ToTime,
		Uploader: upload.Uploader{
			API:         upload.NewClient(e.remote, bal.Client(len(decks)*o.partsPerDeck)),
			Bucket:      e.job.Bucket,
			PartSize:    o.partSize,
			Workers:     o.partsPerDeck,
			Retries:     o.retries,
			Backoff:     time.Second,
			IdleTimeout: 3 * time.Minute,
			Limit:       limit,
		},
		Yes:      o.yes,
		DryRun:   o.dryRun,
		JSON:     o.json,
		In:       std.in,
		Out:      std.out,
		Conns:    bal.Counts,
		Interval: interval,
		Live:     live,
	})
	return err
}
