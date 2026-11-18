package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
[s3.wasabi]
endpoint = "https://s3.ap-southeast-2.wasabisys.com/"
access_key_id = "AK"
secret_access_key = "SK"

[s3.other]
endpoint = "s3.example.com"
region = "eu-west-1"
access_key_id = "AK2"
secret_access_key = "SK2"

[ftp.deck-1]
host = "10.0.0.1"

[ftp.deck-2]
host = "10.0.0.2"
port = 2121
user = "u"
pass = "plain text"
`

func loadConfig(t *testing.T, text string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hdup.toml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

func TestConfigS3ReadsEndpointKeysAndRegion(t *testing.T) {
	c, err := loadConfig(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.S3("wasabi")
	want := S3Remote{Endpoint: "s3.ap-southeast-2.wasabisys.com", Region: "ap-southeast-2", AccessKey: "AK", SecretKey: "SK"}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := c.S3("other"); err != nil || got.Region != "eu-west-1" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestConfigFTPUsesAnonymousAndPort21ByDefault(t *testing.T) {
	c, err := loadConfig(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.FTP("deck-1")
	if want := (FTPRemote{Addr: "10.0.0.1:21", User: "anonymous", Pass: "anonymous"}); err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestConfigFTPReadsThePortTheUserAndThePlainTextPassword(t *testing.T) {
	c, err := loadConfig(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.FTP("deck-2")
	if want := (FTPRemote{Addr: "10.0.0.2:2121", User: "u", Pass: "plain text"}); err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestConfigRejectsIncorrectEntries(t *testing.T) {
	cases := map[string]struct {
		old, new string
		get      func(*Config) error
		want     string
	}{
		"s3 name not in the file":  {``, ``, func(c *Config) error { _, err := c.S3("nope"); return err }, `"nope"`},
		"ftp name not in the file": {``, ``, func(c *Config) error { _, err := c.FTP("nope"); return err }, `"nope"`},
		"s3 with no secret key":    {`secret_access_key = "SK"`, ``, func(c *Config) error { _, err := c.S3("wasabi"); return err }, "secret_access_key"},
		"s3 endpoint not https":    {`https://s3.ap`, `http://s3.ap`, func(c *Config) error { _, err := c.S3("wasabi"); return err }, "https"},
		"ftp with no host":         {`host = "10.0.0.1"`, `user = "u"`, func(c *Config) error { _, err := c.FTP("deck-1"); return err }, "host"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := loadConfig(t, strings.Replace(validConfig, tc.old, tc.new, 1))
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.get(c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigRejectsAnUnknownKeyAndDoesNotShowItsValue(t *testing.T) {
	_, err := loadConfig(t, validConfig+"\n[s3.typo]\nsecret_acess_key = \"TOPSECRET\"\n")
	if err == nil || !strings.Contains(err.Error(), "secret_acess_key") || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("got error %v", err)
	}
}

func TestLoadConfigDoesNotShowTheTextOfALineItCannotRead(t *testing.T) {
	_, err := loadConfig(t, "[s3.wasabi]\nsecret_access_key = TOPSECRET\n")
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("got error %v", err)
	}
}

func TestConfigS3TakesTheKeysFromTheEnvironmentBeforeTheFile(t *testing.T) {
	t.Setenv("HDUP_S3_ACCESS_KEY_ID", "ENVAK")
	t.Setenv("HDUP_S3_SECRET_ACCESS_KEY", "ENVSK")
	c, err := loadConfig(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.S3("wasabi"); err != nil || got.AccessKey != "ENVAK" || got.SecretKey != "ENVSK" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestConfigS3AcceptsAFileWithNoKeysWhenTheEnvironmentHasThem(t *testing.T) {
	t.Setenv("HDUP_S3_ACCESS_KEY_ID", "ENVAK")
	t.Setenv("HDUP_S3_SECRET_ACCESS_KEY", "ENVSK")
	c, err := loadConfig(t, "[s3.wasabi]\nendpoint = \"s3.example.com\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.S3("wasabi"); err != nil || got.AccessKey != "ENVAK" || got.SecretKey != "ENVSK" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
