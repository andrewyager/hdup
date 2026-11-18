package run

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/andrewyager/hdup/internal/fake"
	"github.com/andrewyager/hdup/internal/upload"
)

var modTime = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func config(s3 *fake.S3, out *bytes.Buffer) Config {
	return Config{
		Decks: []Deck{
			{Name: "HD1", Src: &fake.Source{ModTime: modTime, Files: map[string][]byte{
				"a.mov": []byte("0123456789abcd"),
				"b.mov": []byte("xyz"),
			}}},
			{Name: "ISO1", Include: []string{"CH*/**"}, Src: &fake.Source{ModTime: modTime, Files: map[string][]byte{
				"CH1/c.mov": []byte("cam"),
				"notes.txt": []byte("not selected"),
			}}},
		},
		From: modTime.Add(-time.Hour),
		To:   modTime.Add(time.Hour),
		Uploader: upload.Uploader{API: s3, Bucket: "b", PartSize: 4, Workers: 2, Retries: 2,
			Backoff: time.Millisecond, IdleTimeout: 5 * time.Second},
		Yes:      true,
		In:       strings.NewReader(""),
		Out:      out,
		Interval: time.Hour,
	}
}

func TestRunUploadsTheSelectedFilesBelowTheDeckName(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	sum, err := Run(context.Background(), config(s3, out))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Uploaded != 3 || sum.Skipped != 0 || len(sum.Failed) != 0 || sum.Bytes != 20 || sum.Sent != 20 {
		t.Fatalf("summary %+v", sum)
	}
	if string(s3.Objects["HD1/a.mov"]) != "0123456789abcd" || string(s3.Objects["ISO1/CH1/c.mov"]) != "cam" {
		t.Fatalf("objects %v", s3.Objects)
	}
	if _, ok := s3.Objects["ISO1/notes.txt"]; ok {
		t.Fatal("a file that does not match include was uploaded")
	}
	if !strings.Contains(out.String(), "HD1/a.mov") || !strings.Contains(out.String(), "3 files") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunWithDryRunShowsTheFilesAndUploadsNothing(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := config(s3, out)
	cfg.DryRun = true
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(s3.Objects) != 0 || !strings.Contains(out.String(), "ISO1/CH1/c.mov") {
		t.Fatalf("%d objects, output:\n%s", len(s3.Objects), out.String())
	}
}

func TestRunStopsWhenTheAnswerIsNotYes(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := config(s3, out)
	cfg.Yes = false
	cfg.In = strings.NewReader("n\n")
	if _, err := Run(context.Background(), cfg); !errors.Is(err, ErrAborted) {
		t.Fatalf("got error %v", err)
	}
	if len(s3.Objects) != 0 {
		t.Fatal("objects were uploaded")
	}
}

func TestRunUploadsWhenTheAnswerIsYes(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := config(s3, out)
	cfg.Yes = false
	cfg.In = strings.NewReader("y\n")
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(s3.Objects) != 3 {
		t.Fatalf("%d objects", len(s3.Objects))
	}
}

func TestRunContinuesAfterAFileFailsAndReturnsAnError(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	// Only HD1/a.mov has more than one part, so only that file fails.
	s3.FailPart = func(int32) error { return errors.New("injected failure") }
	sum, err := Run(context.Background(), config(s3, out))
	if err == nil {
		t.Fatal("got no error")
	}
	if sum.Uploaded != 2 || len(sum.Failed) != 1 || !strings.Contains(sum.Failed[0], "HD1/a.mov") {
		t.Fatalf("summary %+v", sum)
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunCountsFilesThatAnEarlierRunUploadedAsSkipped(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	if _, err := Run(context.Background(), config(s3, out)); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), config(s3, out))
	if err != nil || sum.Skipped != 3 || sum.Uploaded != 0 {
		t.Fatalf("summary %+v, error %v", sum, err)
	}
}

func TestRunSummarySentHasOnlyTheBytesThatThisRunSent(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	// Parts 1 and 2 of HD1/a.mov are stored; part 3 fails.
	s3.FailPart = func(n int32) error {
		if n == 3 {
			return errors.New("injected failure")
		}
		return nil
	}
	if _, err := Run(context.Background(), config(s3, out)); err == nil {
		t.Fatal("the first run must fail")
	}
	s3.FailPart = nil
	sum, err := Run(context.Background(), config(s3, out))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Uploaded != 1 || sum.Bytes != 14 || sum.Sent <= 0 || sum.Sent > 6 {
		t.Fatalf("summary %+v; want Sent of parts 3 and 4 only", sum)
	}
}

func TestRunWithNoFilesInTheWindowDoesNotAsk(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := config(s3, out)
	cfg.Yes = false
	cfg.From, cfg.To = modTime.Add(time.Hour), modTime.Add(2*time.Hour)
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 files") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunCancelledInTheMiddleCountsInterruptedNotFailed(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s3.FailPart = func(int32) error { cancel(); return errors.New("injected failure") }
	sum, err := Run(ctx, config(s3, out))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v", err)
	}
	if len(sum.Failed) != 0 || sum.Interrupted < 1 {
		t.Fatalf("summary %+v", sum)
	}
	if strings.Contains(out.String(), "FAILED") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunCancelledDuringThePromptReturnsTheCancelError(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	pr, pw := io.Pipe()
	defer pw.Close()
	cfg := config(s3, out)
	cfg.Yes = false
	cfg.In = pr
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v", err)
	}
}

func TestPrintableEscapesControlCharactersOnly(t *testing.T) {
	if got := Printable("CH1/clip 01.mov"); got != "CH1/clip 01.mov" {
		t.Fatalf("got %q", got)
	}
	if got := Printable("a\x1b[2Jb"); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("got %q", got)
	}
}

func TestConnsLineKeepsAnAddressWithoutADot(t *testing.T) {
	if got := connsLine(map[string]int{"::1": 2}); got != "::1=2" {
		t.Fatalf("got %q", got)
	}
}
