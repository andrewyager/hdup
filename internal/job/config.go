package job

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the hdup config file. It holds the same data as the remotes of an
// rclone config file, and it holds the secrets as plain text.
type Config struct {
	path string
	S3s  map[string]s3Config  `toml:"s3"`
	FTPs map[string]ftpConfig `toml:"ftp"`
}

type s3Config struct {
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	AccessKey string `toml:"access_key_id"`
	SecretKey string `toml:"secret_access_key"`
}

type ftpConfig struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
	User string `toml:"user"`
	Pass string `toml:"pass"`
}

// LoadConfig reads an hdup config file. It refuses a file that other users
// can read.
func LoadConfig(path string) (*Config, error) {
	if err := checkPrivate(path); err != nil {
		return nil, err
	}
	c := Config{path: path}
	md, err := toml.DecodeFile(path, &c)
	var pe toml.ParseError
	if errors.As(err, &pe) {
		// The text of the error can hold a secret. Show only the line number.
		return nil, fmt.Errorf("%s: line %d is not correct TOML", path, pe.Position.Line)
	}
	if err != nil {
		return nil, err
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", path, u[0].String())
	}
	return &c, nil
}

func (c *Config) S3(name string) (S3Remote, error) {
	s, ok := c.S3s[name]
	if !ok {
		return S3Remote{}, fmt.Errorf("%s: there is no [s3.%s] section for the remote %q", c.path, name, name)
	}
	return newS3Remote(fmt.Sprintf("%s: [s3.%s]", c.path, name), s.Endpoint, s.Region, s.AccessKey, s.SecretKey)
}

func (c *Config) FTP(name string) (FTPRemote, error) {
	f, ok := c.FTPs[name]
	if !ok {
		return FTPRemote{}, fmt.Errorf("%s: there is no [ftp.%s] section for the deck %q", c.path, name, name)
	}
	port := ""
	if f.Port != 0 {
		port = strconv.Itoa(f.Port)
	}
	r, err := newFTPRemote(fmt.Sprintf("%s: [ftp.%s]", c.path, name), f.Host, port, f.User)
	if err != nil {
		return FTPRemote{}, err
	}
	if f.Pass != "" {
		r.Pass = f.Pass
	}
	return r, nil
}
