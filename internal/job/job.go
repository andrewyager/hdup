// Package job reads the job file and the rclone remotes it names.
package job

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"
)

type Job struct {
	Remote string `toml:"remote"`
	Bucket string `toml:"bucket"`
	From   string `toml:"from"`
	To     string `toml:"to"`
	Decks  []Deck `toml:"deck"`

	FromTime time.Time `toml:"-"`
	ToTime   time.Time `toml:"-"`
}

type Deck struct {
	Remote  string   `toml:"remote"`
	Path    string   `toml:"path"`
	Include []string `toml:"include"`
}

// Load reads a job file. Dates are midnight in loc.
func Load(path string, loc *time.Location) (*Job, error) {
	var j Job
	md, err := toml.DecodeFile(path, &j)
	if err != nil {
		return nil, err
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", path, u[0].String())
	}
	if j.Remote == "" {
		return nil, fmt.Errorf("%s: remote is missing", path)
	}
	if j.Bucket == "" {
		return nil, fmt.Errorf("%s: bucket is missing", path)
	}
	if j.FromTime, err = time.ParseInLocation(time.DateOnly, j.From, loc); err != nil {
		return nil, fmt.Errorf("%s: from must be YYYY-MM-DD", path)
	}
	if j.ToTime, err = time.ParseInLocation(time.DateOnly, j.To, loc); err != nil {
		return nil, fmt.Errorf("%s: to must be YYYY-MM-DD", path)
	}
	if !j.ToTime.After(j.FromTime) {
		return nil, fmt.Errorf("%s: to must be after from", path)
	}
	if len(j.Decks) == 0 {
		return nil, fmt.Errorf("%s: there is no deck", path)
	}
	seen := map[string]bool{}
	for i, d := range j.Decks {
		if d.Remote == "" {
			return nil, fmt.Errorf("%s: deck %d: remote is missing", path, i+1)
		}
		// The object key starts with the remote, so two decks with one remote
		// could write to the same key.
		if seen[d.Remote] {
			return nil, fmt.Errorf("%s: deck %s: the remote is in more than one deck", path, d.Remote)
		}
		seen[d.Remote] = true
		if d.Path == "" {
			return nil, fmt.Errorf("%s: deck %s: path is missing", path, d.Remote)
		}
		for _, p := range d.Include {
			if !doublestar.ValidatePattern(p) {
				return nil, fmt.Errorf("%s: deck %s: include pattern %q is incorrect", path, d.Remote, p)
			}
		}
	}
	return &j, nil
}
