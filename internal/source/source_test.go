package source

import (
	"reflect"
	"testing"
	"time"
)

func paths(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func TestSelectKeepsFilesInsideTheTimeWindow(t *testing.T) {
	from := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	files := []File{
		{Path: "before.mov", ModTime: from.Add(-time.Second)},
		{Path: "at-from.mov", ModTime: from},
		{Path: "inside.mov", ModTime: from.Add(time.Hour)},
		{Path: "at-to.mov", ModTime: to},
	}
	got := paths(Select(files, from, to, nil))
	if !reflect.DeepEqual(got, []string{"at-from.mov", "inside.mov"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSelectKeepsOnlyPathsThatMatchAnIncludePattern(t *testing.T) {
	now := time.Now()
	files := []File{
		{Path: "CH1/a.mov", ModTime: now},
		{Path: "CH2/sub/b.mov", ModTime: now},
		{Path: "other/c.mov", ModTime: now},
		{Path: "top.mov", ModTime: now},
	}
	got := paths(Select(files, now.Add(-time.Hour), now.Add(time.Hour), []string{"CH*/**"}))
	if !reflect.DeepEqual(got, []string{"CH1/a.mov", "CH2/sub/b.mov"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSelectSortsByPath(t *testing.T) {
	now := time.Now()
	files := []File{{Path: "b", ModTime: now}, {Path: "a", ModTime: now}}
	got := paths(Select(files, now.Add(-time.Hour), now.Add(time.Hour), nil))
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}
