package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andrewyager/hdup/internal/job"
	"github.com/andrewyager/hdup/internal/run"
)

func TestExitGivesTheCodeAndTheMessageForEachResult(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		ended bool // the root context has ended
		code  int
		msg   string
	}{
		{"done", nil, false, 0, ""},
		{"interrupted", fmt.Errorf("run: %w", context.Canceled), false, 130, "interrupted; run the same command again to continue"},
		{"network error after interrupt", &net.OpError{Op: "dial", Err: errors.New("use of closed connection")}, true, 130, "interrupted; run the same command again to continue"},
		{"done after interrupt", nil, true, 0, ""},
		{"failed", errors.New("2 files failed"), false, 1, "2 files failed"},
		{"error text with a control character", errors.New("list \x1b[2Jx: failed"), false, 1, `"list \x1b[2Jx: failed"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.ended {
				cancel()
			}
			if code, msg := exit(ctx, c.err); code != c.code || msg != c.msg {
				t.Fatalf("got %d, %q", code, msg)
			}
		})
	}
}

func runCLI(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = cli(context.Background(), args, stdio{in: stdin, out: &out, err: &errOut})
	return out.String(), errOut.String(), err
}

func TestVersionNeedsNoConfigAndPrintsTheVersion(t *testing.T) {
	t.Setenv("RCLONE_CONFIG", filepath.Join(t.TempDir(), "no-such-file"))
	out, _, err := runCLI(t, nil, "version")
	if err != nil || !strings.HasPrefix(out, "hdup ") || len(strings.TrimSpace(out)) <= len("hdup") {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestUploadWithJSONProgressGivesAnErrorEventWhenTheJobFileIsIncorrect(t *testing.T) {
	out, _, err := runCLI(t, nil, "upload", "--progress", "json", filepath.Join(t.TempDir(), "no-job.toml"))
	if err == nil {
		t.Fatal("got no error")
	}
	var ev map[string]string
	if jerr := json.Unmarshal([]byte(out), &ev); jerr != nil || ev["event"] != "error" || ev["error"] == "" {
		t.Fatalf("stdout %q: %v", out, jerr)
	}
}

func TestUploadWithTextProgressGivesNoJSONWhenTheJobFileIsIncorrect(t *testing.T) {
	out, _, err := runCLI(t, nil, "upload", filepath.Join(t.TempDir(), "no-job.toml"))
	if err == nil || out != "" {
		t.Fatalf("stdout %q, error %v", out, err)
	}
}

func TestUploadRejectsIncorrectFlagValues(t *testing.T) {
	for _, args := range [][]string{
		{"upload", "--progress", "xml", "job.toml"},
		{"upload", "--retries", "0", "job.toml"},
		{"upload", "--part-size", "4", "job.toml"},
		{"upload"},
		{"probe"},
		{"nope"},
	} {
		if _, _, err := runCLI(t, nil, args...); !errors.Is(err, errUsage) {
			t.Fatalf("%v: got error %v", args, err)
		}
	}
}

func TestSetupWithAConfigFileDoesNotReadTheRcloneConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RCLONE_CONFIG", filepath.Join(dir, "no-such-file"))
	jobPath, confPath := filepath.Join(dir, "job.toml"), filepath.Join(dir, "hdup.toml")
	write := func(p, text string) {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(jobPath, "remote = \"s\"\nbucket = \"b\"\n[[file]]\ndeck = \"d\"\npath = \"p\"\nkey = \"k\"\n")
	write(confPath, "[s3.s]\nendpoint = \"localhost\"\naccess_key_id = \"AK\"\nsecret_access_key = \"SK\"\n[ftp.d]\nhost = \"127.0.0.1\"\n")
	var info bytes.Buffer
	e, err := setup(context.Background(), jobPath, confPath, &info)
	if err != nil {
		t.Fatal(err)
	}
	if e.remote.AccessKey != "AK" || len(e.addrs) == 0 || !strings.Contains(info.String(), "addresses") {
		t.Fatalf("remote %+v, addrs %v, info %q", e.remote, e.addrs, info.String())
	}
}

func TestFileDecksGroupsTheFilesByDeckAndKeepsTheSequence(t *testing.T) {
	files := []job.File{
		{Deck: "d2", Path: "p1", Key: "k1"},
		{Deck: "d1", Path: "p2", Key: "k2"},
		{Deck: "d2", Path: "p3", Key: "k3"},
	}
	got := groupFiles(files)
	want := []deckFiles{
		{name: "d2", files: []run.Target{{Path: "p1", Key: "k1"}, {Path: "p3", Key: "k3"}}},
		{name: "d1", files: []run.Target{{Path: "p2", Key: "k2"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestStopOnEOFCancelsWhenTheInputCloses(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stopOnEOF(pr, cancel)
	pw.Write([]byte("ignored\n"))
	select {
	case <-ctx.Done():
		t.Fatal("cancelled before the input closed")
	case <-time.After(50 * time.Millisecond):
	}
	pw.Close()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled after the input closed")
	}
}

func TestParseRateReadsTheRcloneFormatForOneRate(t *testing.T) {
	good := map[string]int64{
		"10M": 10 << 20, "10m": 10 << 20, "1.5M": 3 << 19, "512K": 512 << 10, "512": 512 << 10,
		"100B": 100, "1G": 1 << 30, "off": 0, "0": 0, "": 0,
	}
	for in, want := range good {
		if got, err := parseRate(in); err != nil || got != want {
			t.Fatalf("%q: got %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"fast", "10X", "-1M", "10M:1M", "08:00,512K", "M", "1e3", "10 M", "0.0001B", "NaN", "Inf"} {
		if got, err := parseRate(in); err == nil {
			t.Fatalf("%q: got %d and no error", in, got)
		}
	}
}

func TestUploadRejectsABandwidthLimitItCannotRead(t *testing.T) {
	if _, _, err := runCLI(t, nil, "upload", "--bwlimit", "fast", "job.toml"); !errors.Is(err, errUsage) {
		t.Fatalf("got error %v", err)
	}
}
