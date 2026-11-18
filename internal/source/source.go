// Package source lists and reads the files on a deck.
package source

import (
	"context"
	"sort"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

type File struct {
	Path    string // relative to the deck path, with "/"
	Size    int64
	ModTime time.Time
}

// StatResult is the File of one path, or the error for that path.
type StatResult struct {
	File File
	Err  error
}

type Source interface {
	List(ctx context.Context) ([]File, error)
	// Stat gives one result for each path, in the same sequence. It makes one
	// try. The error is not nil if the deck does not answer.
	Stat(ctx context.Context, paths []string) ([]StatResult, error)
	Size(ctx context.Context, path string) (int64, error)
	// ReadRange fills buf from offset, or returns an error.
	ReadRange(ctx context.Context, path string, offset int64, buf []byte) error
}

// ProgressReader is a Source that reports bytes while it reads a range.
type ProgressReader interface {
	ReadRangeProgress(ctx context.Context, path string, offset int64, buf []byte, progress func(n int64)) error
}

// Select keeps files modified at or after from and before to. If include is
// not empty, the path must also match one of its patterns.
func Select(files []File, from, to time.Time, include []string) []File {
	var out []File
	for _, f := range files {
		if f.ModTime.Before(from) || !f.ModTime.Before(to) {
			continue
		}
		if len(include) > 0 && !matchAny(include, f.Path) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if ok, _ := doublestar.Match(p, path); ok {
			return true
		}
	}
	return false
}
