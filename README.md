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
- **One job file.** It names the bucket and the decks, with a date window
  or an explicit file list. Endpoints, keys and passwords come from an `hdup`
  config file or from your rclone config.
- **Use from a different program.** `--progress json` gives one JSON object
  on each line for each step of each file.

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

- Go 1.27 or later to build. The release binaries need nothing.
- A config with one S3 remote and one FTP remote for each deck. This is an
  `hdup` config file (see [Config file](#config-file)), or an
  [rclone](https://rclone.org/) config file. `hdup` reads the rclone config
  file itself. rclone does not have to be installed on the machine that runs
  `hdup`.
- An FTP server that supports `REST` (restart at an offset) and `MDTM`. The
  HyperDecks do.

## Install

Each release has binaries for macOS (arm64) and Windows (x64), and a
`SHA256SUMS` file. The binaries are not signed. Get them from the
[releases page](https://github.com/andrewyager/hdup/releases).

To build with Go:

```bash
go install github.com/andrewyager/hdup/cmd/hdup@latest
```

Or build from a clone:

```bash
go build -o hdup ./cmd/hdup
```

## Config file

`--config FILE` gives an `hdup` config file. With this flag, `hdup` does not
read an rclone config.

```toml
[s3.my-s3]
endpoint          = "s3.ap-southeast-2.wasabisys.com"
region            = "ap-southeast-2"   # optional
access_key_id     = "..."
secret_access_key = "..."

[ftp.deck-1]
host = "10.0.0.11"
port = 21             # optional, default 21
user = "anonymous"    # optional, default anonymous
pass = "..."          # optional, plain text
```

- The name after `s3.` or `ftp.` is the name that the job file uses.
- The file holds secrets as plain text. On macOS and Linux, `hdup` refuses
  the file if the group or other users have access to it. Use `chmod 600`.
  On Windows, `hdup` does not examine the access. Keep the file in a folder
  that only your user can read.

Without `--config`, `hdup` reads the rclone config file from the path in
`RCLONE_CONFIG`. If that variable is not set, it looks in
`%APPDATA%\rclone\rclone.conf` (Windows),
`$XDG_CONFIG_HOME/rclone/rclone.conf`, `~/.config/rclone/rclone.conf` and
`~/.rclone.conf`.

## Job file

```toml
remote = "my-s3"        # S3 remote
bucket = "my-event"
from   = "2026-01-15"   # files modified at or after 00:00 on this date
to     = "2026-01-16"   # and before 00:00 on this date

[[deck]]
remote = "deck-1"       # FTP remote
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

### Explicit file list

A job file can name each file and its object key. It then has no `from`, no
`to` and no `[[deck]]`.

```toml
remote = "my-s3"
bucket = "my-event"

[[file]]
deck = "deck-1"                        # FTP remote
path = "internal/CH1/clip0001.mov"     # full path on the deck
key  = "show/day1/cam1/clip0001.mov"   # full object key

[[file]]
deck = "deck-2"
path = "internal/clip0007.mov"
key  = "show/day1/cam2/clip0007.mov"
```

- Decks run in parallel. The files of one deck go in the sequence of the
  list.
- A key can be in the list only one time.
- Before the upload, `hdup` gets the size and the modified time of each file
  from its deck, with one try. A file that is not on the deck fails. If a
  deck does not answer in 10 seconds, all the files of that deck fail. The
  other files continue.

## Use

```bash
hdup upload --dry-run job.toml   # show the files only
hdup upload job.toml             # show the files, ask, then upload
hdup probe job.toml              # measure the upload rate
```

`hdup probe` uploads generated data for approximately 80 seconds. Then it
aborts the upload and leaves nothing in the bucket.

Flags go before the job file. `--config FILE` applies to `upload` and to
`probe`. The other flags apply to `upload`:

| Flag | Default | Function |
|---|---|---|
| `--config FILE` | none | Read this `hdup` config file and not the rclone config. |
| `--parts-per-deck N` | 8 | Parallel parts for each deck. |
| `--part-size N` | 64 | Part size in MiB (minimum 5). |
| `--retries N` | 10 | Tries for each read, send and storage call. |
| `--yes` | off | Do not ask for confirmation. |
| `--dry-run` | off | Show the files and stop. |
| `--progress MODE` | `text` | `text`, or `json` for JSON lines. |
| `--stdin-stop` | off | Stop when the standard input closes. |

`hdup version` shows the version. It reads no config.

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
of a file that is not complete. A file is complete if an object with its key
and its size is in the bucket. `hdup` does not compare the content or the
time of a complete object. If the object has a different size, `hdup`
uploads the file and replaces the object. Do not change `--part-size` between the two
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

## Use from a different program

A program that starts `hdup` as a child process uses these flags:

```bash
hdup upload --config hdup.toml --progress json --stdin-stop job.toml
```

`--progress json` and `--stdin-stop` each include `--yes`.

### JSON lines

With `--progress json`, the standard output has only JSON objects, one on
each line. Each line is written immediately. Text for a person goes to the
standard error.

| `event` | Other fields | When |
|---|---|---|
| `plan` | `files`, `bytes` | One time, after `hdup` has found the files. |
| `file_start` | `deck`, `path`, `key`, `size` | The upload of a file starts. |
| `file_progress` | `deck`, `path`, `key`, `size`, `done` | Each second for each file in progress. |
| `file_done` | `deck`, `path`, `key`, `size`, `result` | The file is in the bucket. `result` is `uploaded` or `skipped`. |
| `file_failed` | `deck`, `path`, `key`, `error`, and `size` if it is known | The file failed. |
| `file_interrupted` | `deck`, `path`, `key`, `size` | A stop ended the upload of the file. |
| `summary` | `uploaded`, `skipped`, `failed`, `interrupted`, `bytes`, `sent`, `elapsed_s` | One time, at the end. |
| `error` | `error` | An error stopped the full run. There is no `summary`. |

```json
{"event":"plan","files":2,"bytes":2147483648}
{"event":"file_start","deck":"deck-1","path":"internal/CH1/clip0001.mov","key":"show/day1/cam1/clip0001.mov","size":1073741824}
{"event":"file_progress","deck":"deck-1","path":"internal/CH1/clip0001.mov","key":"show/day1/cam1/clip0001.mov","size":1073741824,"done":268435456}
{"event":"file_done","deck":"deck-1","path":"internal/CH1/clip0001.mov","key":"show/day1/cam1/clip0001.mov","size":1073741824,"result":"uploaded"}
{"event":"file_failed","deck":"deck-2","path":"internal/clip0007.mov","key":"show/day1/cam2/clip0007.mov","error":"..."}
{"event":"summary","uploaded":1,"skipped":0,"failed":1,"interrupted":0,"bytes":1073741824,"sent":1073741824,"elapsed_s":12.5}
```

- Sizes and `done` are in bytes. `done` includes the bytes that an earlier
  run sent. `done` can decrease when a part is tried again.
- `plan` counts only the files that `hdup` found on the decks.
- `bytes` in `summary` is the size of the uploaded files. `sent` is the
  number of bytes that this run sent.
- After a stop, a file that did not start has no event.
- An error in the flags gives the usage text on the standard error and no
  JSON line.
- With a date window, `path` is the path below the deck path.

### Stop

On macOS and Linux, send `SIGINT` or `SIGTERM`. With `--stdin-stop`, `hdup`
also stops when its standard input closes. Use this on Windows, and to stop
`hdup` if the parent program ends. Keep the input open during the run:
`hdup` stops immediately if the input is closed at the start.

After a stop, `hdup` writes the summary and the exit code is 130. A process
that is killed writes no summary. In the two cases, the next run continues
the files, because the storage service holds the state.

### Time limits

- A connection to a deck must open in 10 seconds.
- A deck that is silent for 60 seconds ends the try.
- A send that moves no data for 3 minutes ends the try.
- A storage call that sends no file data has 2 minutes for each try.
- `--retries N` sets the number of tries. The delay between two tries starts
  at 1 second and doubles to a maximum of 30 seconds.

With a small `--retries` value, a file on a deck that stops fails quickly,
and the next run continues it.

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
- `hdup` cannot read an rclone config file that is encrypted with a
  password.
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
