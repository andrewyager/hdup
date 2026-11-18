package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validJob = `
remote = "wasabi"
bucket = "rushes"
from   = "2026-11-12"
to     = "2026-11-13"

[[deck]]
remote = "HD1"
path   = "internal/"

[[deck]]
remote  = "ISO1"
path    = "internal/"
include = ["CH*/**"]
`

func load(t *testing.T, text string) (*Job, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "job.toml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p, time.UTC)
}

func TestLoadReadsAValidJobFile(t *testing.T) {
	j, err := load(t, validJob)
	if err != nil {
		t.Fatal(err)
	}
	if j.Remote != "wasabi" || j.Bucket != "rushes" || len(j.Decks) != 2 {
		t.Fatalf("got %+v", j)
	}
	if !j.FromTime.Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)) ||
		!j.ToTime.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window %v to %v", j.FromTime, j.ToTime)
	}
	if j.Decks[1].Include[0] != "CH*/**" {
		t.Fatalf("include %v", j.Decks[1].Include)
	}
}

func TestLoadRejectsIncorrectJobFiles(t *testing.T) {
	cases := map[string]struct{ old, new, want string }{
		"missing remote":    {`remote = "wasabi"`, ``, "remote"},
		"missing bucket":    {`bucket = "rushes"`, ``, "bucket"},
		"incorrect date":    {`"2026-11-12"`, `"12/11/2026"`, "from"},
		"to not after from": {`"2026-11-13"`, `"2026-11-12"`, "after"},
		"unknown key":       {`bucket = "rushes"`, "bucket = \"rushes\"\nbuckett = 1", "buckett"},
		"deck with no path": {`path   = "internal/"`, ``, "path"},
		"incorrect pattern": {`"CH*/**"`, `"CH[/**"`, "include"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, strings.Replace(validJob, c.old, c.new, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got error %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestLoadRejectsTwoDecksWithTheSameRemote(t *testing.T) {
	_, err := load(t, strings.Replace(validJob, `remote  = "ISO1"`, `remote  = "HD1"`, 1))
	if err == nil || !strings.Contains(err.Error(), "HD1") || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("got error %v", err)
	}
}

func TestLoadRejectsAJobWithNoDecks(t *testing.T) {
	_, err := load(t, "remote = \"w\"\nbucket = \"b\"\nfrom = \"2026-11-12\"\nto = \"2026-11-13\"\n")
	if err == nil || !strings.Contains(err.Error(), "deck") {
		t.Fatalf("got error %v", err)
	}
}

const fileJob = `
remote = "wasabi"
bucket = "rushes"

[[file]]
deck = "deck-1"
path = "internal/CH1/a.mov"
key  = "show/day1/cam1/a.mov"

[[file]]
deck = "deck-2"
path = "internal/b.mov"
key  = "show/day1/cam2/b.mov"

[[file]]
deck = "deck-1"
path = "internal/CH1/c.mov"
key  = "show/day1/cam1/c.mov"
`

func TestLoadReadsAJobWithAFileListAndNoDateWindow(t *testing.T) {
	j, err := load(t, fileJob)
	if err != nil {
		t.Fatal(err)
	}
	want := File{Deck: "deck-1", Path: "internal/CH1/c.mov", Key: "show/day1/cam1/c.mov"}
	if len(j.Files) != 3 || j.Files[2] != want || len(j.Decks) != 0 {
		t.Fatalf("got %+v", j)
	}
}

func TestLoadRejectsIncorrectFileLists(t *testing.T) {
	cases := map[string]struct{ old, new, want string }{
		"file with no deck":        {`deck = "deck-2"`, ``, "deck"},
		"file with no path":        {`path = "internal/b.mov"`, ``, "path"},
		"file with no key":         {`key  = "show/day1/cam2/b.mov"`, ``, "key"},
		"same key two times":       {`cam1/c.mov`, `cam1/a.mov`, "more than one"},
		"key with a leading slash": {`"show/day1/cam2/b.mov"`, `"/show/b.mov"`, "key"},
		"file list with a date":    {`bucket = "rushes"`, "bucket = \"rushes\"\nfrom = \"2026-11-12\"", "from"},
		"file list with a deck":    {`[[file]]`, "[[deck]]\nremote = \"HD1\"\npath = \"internal/\"\n\n[[file]]", "[[deck]]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, strings.Replace(fileJob, c.old, c.new, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got error %v, want it to contain %q", err, c.want)
			}
		})
	}
}
