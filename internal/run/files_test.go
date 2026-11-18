package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewyager/hdup/internal/fake"
)

// fileConfig has two decks with an explicit file list.
func fileConfig(s3 *fake.S3, out *bytes.Buffer) Config {
	cfg := config(s3, out)
	cfg.From, cfg.To = time.Time{}, time.Time{}
	cfg.Decks = []Deck{
		{Name: "deck-1", Src: &fake.Source{ModTime: modTime, Files: map[string][]byte{
			"internal/a.mov":     []byte("0123456789abcd"),
			"internal/b.mov":     []byte("xyz"),
			"internal/other.mov": []byte("not in the list"),
		}}, Files: []Target{
			{Path: "internal/b.mov", Key: "show/cam1/b.mov"},
			{Path: "internal/a.mov", Key: "show/cam1/a.mov"},
		}},
		{Name: "deck-2", Src: &fake.Source{ModTime: modTime, Files: map[string][]byte{
			"internal/c.mov": []byte("cam"),
		}}, Files: []Target{
			{Path: "internal/c.mov", Key: "show/cam2/c.mov"},
		}},
	}
	return cfg
}

func TestRunWithAFileListUploadsOnlyTheListedFilesToTheGivenKeys(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	sum, err := Run(context.Background(), fileConfig(s3, out))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Uploaded != 3 || len(s3.Objects) != 3 {
		t.Fatalf("summary %+v, objects %d", sum, len(s3.Objects))
	}
	if string(s3.Objects["show/cam1/a.mov"]) != "0123456789abcd" || string(s3.Objects["show/cam2/c.mov"]) != "cam" {
		t.Fatalf("objects %v", s3.Objects)
	}
}

func TestRunWithAFileListFailsAMissingFileAndUploadsTheOthers(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := fileConfig(s3, out)
	cfg.Decks[0].Files[0].Path = "internal/nope.mov"
	sum, err := Run(context.Background(), cfg)
	var failed FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("got error %v", err)
	}
	if sum.Uploaded != 2 || len(sum.Failed) != 1 || !strings.Contains(sum.Failed[0], "show/cam1/b.mov") {
		t.Fatalf("summary %+v", sum)
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunWithAFileListFailsEachFileOfADeckThatDoesNotAnswerAndUploadsTheOtherDecks(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := fileConfig(s3, out)
	cfg.Decks[0].Src.(*fake.Source).Down = true
	sum, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("got no error")
	}
	if sum.Uploaded != 1 || len(sum.Failed) != 2 || string(s3.Objects["show/cam2/c.mov"]) != "cam" {
		t.Fatalf("summary %+v", sum)
	}
}

func TestRunWithAFileListWhereAllFilesFailStillReturnsTheError(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := fileConfig(s3, out)
	cfg.Decks = cfg.Decks[:1]
	cfg.Decks[0].Src.(*fake.Source).Down = true
	if sum, err := Run(context.Background(), cfg); err == nil || len(sum.Failed) != 2 {
		t.Fatalf("summary %+v, error %v", sum, err)
	}
}

// decode decodes the JSON lines of out. It fails if a line is not a JSON
// object.
func decode(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// find gives the events of one type for one key. An empty key matches all.
func find(evs []map[string]any, event, key string) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev["event"] == event && (key == "" || ev["key"] == key) {
			out = append(out, ev)
		}
	}
	return out
}

func TestRunWithJSONWritesOnlyJSONLinesWithAnEventForEachFileStep(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	s3.Objects["show/cam2/c.mov"] = []byte("cam")
	cfg := fileConfig(s3, out)
	cfg.JSON = true
	cfg.Decks[0].Files[0].Path = "internal/nope.mov"
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("got no error")
	}
	evs := decode(t, out)
	if evs[len(evs)-1]["event"] != "summary" {
		t.Fatalf("the last event is %v", evs[len(evs)-1])
	}
	plan := find(evs, "plan", "")
	if len(plan) != 1 || plan[0]["files"] != 2.0 || plan[0]["bytes"] != 17.0 {
		t.Fatalf("plan %v", plan)
	}
	start := find(evs, "file_start", "show/cam1/a.mov")
	if len(start) != 1 || start[0]["deck"] != "deck-1" || start[0]["path"] != "internal/a.mov" || start[0]["size"] != 14.0 {
		t.Fatalf("file_start %v", start)
	}
	if done := find(evs, "file_done", "show/cam1/a.mov"); len(done) != 1 || done[0]["result"] != "uploaded" || done[0]["size"] != 14.0 {
		t.Fatalf("file_done %v", done)
	}
	if done := find(evs, "file_done", "show/cam2/c.mov"); len(done) != 1 || done[0]["result"] != "skipped" {
		t.Fatalf("file_done %v", done)
	}
	failed := find(evs, "file_failed", "show/cam1/b.mov")
	if len(failed) != 1 || failed[0]["path"] != "internal/nope.mov" || !strings.Contains(failed[0]["error"].(string), "550") {
		t.Fatalf("file_failed %v", failed)
	}
	sum := find(evs, "summary", "")[0]
	if sum["uploaded"] != 1.0 || sum["skipped"] != 1.0 || sum["failed"] != 1.0 || sum["interrupted"] != 0.0 || sum["bytes"] != 14.0 {
		t.Fatalf("summary %v", sum)
	}
}

func TestRunWithJSONReportsTheBytesOfTheFileThatIsInProgress(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	s3.FailPart = func(int32) error { time.Sleep(20 * time.Millisecond); return nil }
	cfg := fileConfig(s3, out)
	cfg.JSON = true
	cfg.Interval = time.Millisecond
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	progress := find(decode(t, out), "file_progress", "show/cam1/a.mov")
	if len(progress) == 0 {
		t.Fatal("no file_progress event")
	}
	for _, ev := range progress {
		if done := ev["done"].(float64); ev["size"] != 14.0 || ev["deck"] != "deck-1" || done < 0 || done > 14 {
			t.Fatalf("file_progress %v", ev)
		}
	}
}

func TestRunWithJSONReportsAnUploadThatFailsAndAnUploadThatIsInterrupted(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	s3.FailPart = func(int32) error { return errors.New("injected failure") }
	cfg := fileConfig(s3, out)
	cfg.JSON = true
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("got no error")
	}
	failed := find(decode(t, out), "file_failed", "show/cam1/a.mov")
	if len(failed) != 1 || !strings.Contains(failed[0]["error"].(string), "injected failure") {
		t.Fatalf("file_failed %v", failed)
	}

	s3, out = fake.NewS3(), &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s3.FailPart = func(int32) error { cancel(); return errors.New("injected failure") }
	cfg = fileConfig(s3, out)
	cfg.JSON = true
	if _, err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v", err)
	}
	evs := decode(t, out)
	if len(find(evs, "file_interrupted", "show/cam1/a.mov")) != 1 || len(find(evs, "file_failed", "")) != 0 {
		t.Fatalf("events %v", evs)
	}
	if sum := find(evs, "summary", "")[0]; sum["interrupted"].(float64) < 1 {
		t.Fatalf("summary %v", sum)
	}
}

func TestRunWithJSONAndTheDateWindowUsesTheDeckNameInTheKey(t *testing.T) {
	s3, out := fake.NewS3(), &bytes.Buffer{}
	cfg := config(s3, out)
	cfg.JSON = true
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if done := find(decode(t, out), "file_done", "ISO1/CH1/c.mov"); len(done) != 1 || done[0]["path"] != "CH1/c.mov" {
		t.Fatalf("file_done %v", done)
	}
}
