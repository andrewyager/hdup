package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

type S3Remote struct {
	Endpoint  string // host name, no scheme
	Region    string
	AccessKey string
	SecretKey string
}

type FTPRemote struct {
	Addr string // host:port
	User string
	Pass string
}

type Rclone struct{ Bin string }

// FindRclone looks in PATH, then in the usual Homebrew locations.
func FindRclone() (string, error) {
	if p, err := exec.LookPath("rclone"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/rclone", "/usr/local/bin/rclone"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("rclone is not installed")
}

// output runs rclone. On failure, the error includes the stderr text of rclone
// but never stdout, because stdout can hold keys.
func (r Rclone) output(args ...string) ([]byte, error) {
	out, err := exec.Command(r.Bin, args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if msg := strings.TrimSpace(string(exit.Stderr)); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

func (r Rclone) remote(name, wantType string) (map[string]string, error) {
	out, err := r.output("config", "dump")
	if err != nil {
		return nil, fmt.Errorf("rclone config dump: %w", err)
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("rclone config dump: %w", err)
	}
	c, ok := all[name]
	if !ok {
		return nil, fmt.Errorf("rclone remote %q not found", name)
	}
	if c["type"] != wantType {
		return nil, fmt.Errorf("rclone remote %q has type %q, not %q", name, c["type"], wantType)
	}
	return c, nil
}

func (r Rclone) S3(name string) (S3Remote, error) {
	c, err := r.remote(name, "s3")
	if err != nil {
		return S3Remote{}, err
	}
	endpoint := strings.TrimPrefix(c["endpoint"], "https://")
	if strings.Contains(endpoint, "://") {
		return S3Remote{}, fmt.Errorf("rclone remote %q: only https endpoints are supported", name)
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	s := S3Remote{Endpoint: endpoint, Region: c["region"], AccessKey: c["access_key_id"], SecretKey: c["secret_access_key"]}
	if s.Endpoint == "" || s.AccessKey == "" || s.SecretKey == "" {
		return S3Remote{}, fmt.Errorf("rclone remote %q must have endpoint, access_key_id and secret_access_key", name)
	}
	if s.Region == "" {
		s.Region = regionOf(s.Endpoint)
	}
	return s, nil
}

// regionOf gets the region from a name such as s3.ap-southeast-2.wasabisys.com.
func regionOf(endpoint string) string {
	p := strings.Split(endpoint, ".")
	if len(p) == 4 && p[0] == "s3" {
		return p[1]
	}
	return "us-east-1"
}

func (r Rclone) FTP(name string) (FTPRemote, error) {
	c, err := r.remote(name, "ftp")
	if err != nil {
		return FTPRemote{}, err
	}
	if c["host"] == "" {
		return FTPRemote{}, fmt.Errorf("rclone remote %q has no host", name)
	}
	port := c["port"]
	if port == "" {
		port = "21"
	}
	f := FTPRemote{Addr: net.JoinHostPort(c["host"], port), User: c["user"]}
	if f.User == "" {
		f.User, f.Pass = "anonymous", "anonymous"
	}
	if c["pass"] != "" {
		pass, err := reveal(c["pass"])
		if err != nil {
			return FTPRemote{}, fmt.Errorf("rclone remote %q: password: %w", name, err)
		}
		f.Pass = pass
	}
	return f, nil
}
