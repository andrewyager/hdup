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
