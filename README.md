# hdup

`hdup` uploads video files from Blackmagic HyperDeck recorders to S3 storage.
It reads the files directly from the FTP server of each deck, with no local
copy, and it sends many parts in parallel. It continues an upload that was
interrupted.

We wrote it to move multi-terabyte recordings from four HyperDecks to Wasabi
after an event. It replaces a shell script that started one `rclone copy` for
each deck.

## Why it exists

An S3 endpoint usually has many IP addresses. The Wasabi endpoint that we use
has eight. A client that opens its connections across all of them shares the
load between the hosts of the storage service.

On macOS, this does not occur with rclone. rclone uses the system resolver,
and the macOS resolver gives the same address for the endpoint name until its
cache entry expires. All connections of a run then go to one host, and the
other addresses get no traffic. We tested this on macOS and confirmed it.

`hdup` does not use the order from the resolver. It resolves the endpoint
itself, and it gives each new connection the next address in the list. During
a run it shows the number of connections on each address, so you can see the
distribution.

The other reasons are resume of interrupted multipart uploads, which rclone
does not do, and one command for all decks.

## What it does

- **Parallel parts.** Each part of a file has its own FTP connection to the
  deck and its own HTTPS connection to the S3 endpoint.
- **Connections on all endpoint addresses.** `hdup` resolves the S3 endpoint
  and opens its connections across all the addresses in turn.
- **Resume.** A second run of the same command skips the files that are
  complete and sends only the missing parts of the other files. The storage
  service holds the state. There is no local state file.
- **Checks.** Each part is sent with its MD5. A file that changes on the deck
  during the upload fails. Stored parts are verified before a resume
  continues.
- **One job file.** It names the bucket, a date window and the decks.
  Endpoints, keys and passwords come from your rclone config.

## What to expect

We measured the upload rate to Wasabi with generated data, on a link that
carries approximately 2.25 Gbit/s. The only difference between the two
columns is the number of endpoint addresses in use:

| Connections | 1 address | 8 addresses |
|---|---|---|
| 8 | 620 Mbit/s | 1,840 Mbit/s |
| 32 | 1,600 to 2,100 Mbit/s | 2,250 Mbit/s |
| 64 | 2,200 Mbit/s | 2,250 Mbit/s |

- With a small number of connections, the distribution across the addresses
  gave approximately three times the rate.
- One address can get near the limit of the link, but it needs many more
  connections to do so.
- The rate also increases with the number of connections, until the link is
  full.

In the real upload, `hdup` moved 2.3 TiB from four decks at up to
2.2 Gbit/s with 32 connections.

rclone with a large `--s3-upload-concurrency` also opens many connections,
but on macOS they all go to one address. `hdup` also gives resume of
multipart uploads and a direct FTP to S3 path for more than one deck at a
time.

Your link and your storage service can give different numbers. `hdup probe`
measures some of these cases on your link.

`hdup` is tested with HyperDeck FTP servers and with Wasabi. Other S3
services and other FTP servers can work, but they are not tested.

## Requirements

- Go 1.27 or later to build.
- [rclone](https://rclone.org/) on the machine that runs `hdup`, with one S3
  remote and one FTP remote for each deck. `hdup` does not use rclone to move
  data. It uses `rclone config dump` to read the remotes.
- An FTP server that supports `REST` (restart at an offset) and `MDTM`. The
  HyperDecks do.

## Install

```bash
go install github.com/andrewyager/hdup/cmd/hdup@latest
```

Or build from a clone:

```bash
go build -o hdup ./cmd/hdup
```

`hdup` looks for rclone in `PATH`, then in `/opt/homebrew/bin` and
`/usr/local/bin`.

## Job file

```toml
remote = "my-s3"        # rclone S3 remote
bucket = "my-event"
from   = "2026-01-15"   # files modified at or after 00:00 on this date
to     = "2026-01-16"   # and before 00:00 on this date

[[deck]]
remote = "deck-1"       # rclone FTP remote
path   = "internal/"

[[deck]]
remote  = "deck-2"
path    = "internal/"
include = ["CH*/**"]    # optional
```

- The dates use the time zone of the machine.
- An object gets the key `<deck remote>/<path below the deck path>`.
- `include` patterns use `*` for one path segment and `**` for all segments.
  A file is selected if it matches one of the patterns.
- Each deck remote can be in the job file only one time.
- The job file contains no secrets.

## Use

```bash
hdup upload --dry-run job.toml   # show the files only
hdup upload job.toml             # show the files, ask, then upload
hdup probe job.toml              # measure the upload rate
```

`hdup probe` uploads generated data for approximately 80 seconds. Then it
aborts the upload and leaves nothing in the bucket.

Flags for `upload` go before the job file:

| Flag | Default | Function |
|---|---|---|
| `--parts-per-deck N` | 8 | Parallel parts for each deck. |
| `--part-size N` | 64 | Part size in MiB (minimum 5). |
| `--yes` | off | Do not ask for confirmation. |
| `--dry-run` | off | Show the files and stop. |

Decks run in parallel. The files of one deck go in sequence. Memory use is
approximately decks × parts for each deck × part size.

During a run, `hdup` shows one line for each deck with the read rate from the
deck and the send rate to the storage service, a total line, and the number
of connections on each endpoint address.

### How many parallel parts

The upload rate depends on the number of parallel connections. One connection
can be slow on a long or busy internet path.

`hdup probe` shows the rate for 1, 8 and 32 connections. It also shows the
effect of 1 or 8 endpoint addresses. Use it to find a good value.

The total number of connections is decks × parts for each deck. If the job
has only one deck, use a larger `--parts-per-deck` (for example 32). More
connections than the uplink can carry give no more speed. Too many
connections make each connection slow. A send that moves no data for
3 minutes stops, and `hdup` tries it again.

If the read rate and the send rate are equal and the rate is lower than you
expect, the decks or the storage service are the limit, not the number of
connections.

## Resume

To continue after an interruption or a failed file, run the same command
again. `hdup` skips files that are complete and sends only the missing parts
of a file that is not complete. Do not change `--part-size` between the two
runs: a different part size starts the file again.

Before it continues a file, `hdup` reads the first and the last stored part
from the deck again and compares them with the stored parts. If the file
changed on the deck after the earlier run, the file starts again.

Ctrl-C stops the run. The summary shows the files that were interrupted. The
same command continues them. A second Ctrl-C stops `hdup` immediately.

An upload that is never completed keeps its parts in the bucket, and the
storage service can charge for them. Remove them with rclone, for example:

```bash
rclone cleanup "my-s3:my-event"
```

By default, rclone removes only uploads that are older than 24 hours.

## Unattended runs

The exit code of `hdup upload` is:

| Code | Meaning |
|---|---|
| 0 | All files are done. |
| 130 | The run was interrupted. |
| 1 | One or more files failed, or a different error occurred. |

`scripts/run-loop.sh` runs `hdup` again each 60 seconds until all files are
done, and stops on Ctrl-C. Put it in the same directory as `hdup` and
`job.toml`:

```bash
./run-loop.sh        # 8 parts for each deck
./run-loop.sh 16     # 16 parts for each deck
```

Warning: do not run rclone or a second `hdup` on the same bucket keys at the
same time. Resume aborts or continues multipart uploads that it did not
start.

### macOS: detached sessions lose access to the decks

On macOS, Local Network privacy can block `hdup` from the decks when you
start it from an SSH session in `screen` or `tmux` and then close the SSH
session. New connections to the decks then fail with
`connect: no route to host`. Connections to the internet continue.

Two procedures prevent this:

- Start `hdup` from Terminal on the desktop of the Mac, and approve the
  Local Network prompt.
- Or start it through an SSH session to the same machine, inside `screen`:

  ```bash
  screen -dmS hdup ssh -tt localhost "./run-loop.sh"
  ```

## Limits

- The storage endpoint must use HTTPS.
- `hdup` copies files to the bucket. It does not delete, sync or download.
- A file that is recording when the run starts fails with a clear message.
  Select completed files with the date window.
- A resume verifies the first and the last stored part, not all parts.
- Objects have no stored modified time of the source file.

## Design

See [docs/design.md](docs/design.md).

## Tests

```bash
go test -race ./...
```

The tests use an FTP server and an S3 substitute that run in the test
process. They do not use the network.

## Security

Report a vulnerability through
[GitHub security advisories](https://github.com/andrewyager/hdup/security/advisories/new).
Do not open a public issue for it.

## Licence

MIT. See [LICENSE](LICENSE).
