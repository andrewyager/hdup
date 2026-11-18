package job

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

// Remotes holds the remotes of an rclone config file: name, then key, then
// value.
type Remotes map[string]map[string]string

// ConfigPath gives the rclone config file: RCLONE_CONFIG, or the first
// default location of rclone that exists.
func ConfigPath() (string, error) {
	if p := os.Getenv("RCLONE_CONFIG"); p != "" {
		return p, nil
	}
	var candidates []string
	if appData := os.Getenv("APPDATA"); appData != "" {
		// Windows.
		candidates = append(candidates, filepath.Join(appData, "rclone", "rclone.conf"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "rclone", "rclone.conf"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".config", "rclone", "rclone.conf"),
			filepath.Join(home, ".rclone.conf"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no rclone config file found; set RCLONE_CONFIG to its path")
}

// LoadRemotes reads an rclone config file. hdup reads the file itself and
// does not run rclone.
func LoadRemotes(path string) (Remotes, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	remotes := Remotes{}
	var section map[string]string
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line == "RCLONE_ENCRYPT_V0:":
			return nil, fmt.Errorf("%s is encrypted; hdup cannot read an encrypted rclone config", path)
		case line[0] == '[' && line[len(line)-1] == ']':
			section = map[string]string{}
			remotes[line[1:len(line)-1]] = section
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok || section == nil {
				// The line can hold a secret. Do not show it.
				return nil, fmt.Errorf("%s: line %d is not in a section or has no \"=\"", path, n)
			}
			section[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return remotes, nil
}

func (r Remotes) remote(name, wantType string) (map[string]string, error) {
	c, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("rclone remote %q not found", name)
	}
	if c["type"] != wantType {
		return nil, fmt.Errorf("rclone remote %q has type %q, not %q", name, c["type"], wantType)
	}
	return c, nil
}

func (r Remotes) S3(name string) (S3Remote, error) {
	c, err := r.remote(name, "s3")
	if err != nil {
		return S3Remote{}, err
	}
	return newS3Remote(fmt.Sprintf("rclone remote %q", name), c["endpoint"], c["region"], c["access_key_id"], c["secret_access_key"])
}

// newS3Remote checks the values of an S3 remote. what names the remote in an
// error.
func newS3Remote(what, endpoint, region, accessKey, secretKey string) (S3Remote, error) {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	if strings.Contains(endpoint, "://") {
		return S3Remote{}, fmt.Errorf("%s: only https endpoints are supported", what)
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	s := S3Remote{Endpoint: endpoint, Region: region, AccessKey: accessKey, SecretKey: secretKey}
	if s.Endpoint == "" || s.AccessKey == "" || s.SecretKey == "" {
		return S3Remote{}, fmt.Errorf("%s must have endpoint, access_key_id and secret_access_key", what)
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

func (r Remotes) FTP(name string) (FTPRemote, error) {
	c, err := r.remote(name, "ftp")
	if err != nil {
		return FTPRemote{}, err
	}
	f, err := newFTPRemote(fmt.Sprintf("rclone remote %q", name), c["host"], c["port"], c["user"])
	if err != nil {
		return FTPRemote{}, err
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

// newFTPRemote checks the values of an FTP remote. With no user, the user and
// the password are "anonymous". With a user, the password is empty.
func newFTPRemote(what, host, port, user string) (FTPRemote, error) {
	if host == "" {
		return FTPRemote{}, fmt.Errorf("%s has no host", what)
	}
	if port == "" {
		port = "21"
	}
	f := FTPRemote{Addr: net.JoinHostPort(host, port), User: user}
	if f.User == "" {
		f.User, f.Pass = "anonymous", "anonymous"
	}
	return f, nil
}
