package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `# a comment
; a second comment

[wasabi]
type = s3
provider = Wasabi
endpoint = https://s3.ap-southeast-2.wasabisys.com/
access_key_id = AK
secret_access_key = SK

[HD1]
type = ftp
host = 10.0.0.1
user = u
pass = UaJq9L2mn8Bu22WaeUUnynHFKVGyPfHSLoJuB8sumfdIqA

[HD2]
type = ftp
host = 10.0.0.2
port = 2121

[HD3]
type = ftp
host = 10.0.0.3
user = u

[bare]
type = s3
endpoint = s3.ap-southeast-2.wasabisys.com
access_key_id = AK
secret_access_key = SK

[plain]
type = s3
endpoint = http://s3.wasabisys.com
access_key_id = AK
secret_access_key = SK

[other]
type = s3
endpoint = ftp://s3.wasabisys.com
access_key_id = AK
secret_access_key = SK

[Event Media Uploads]
type = s3
endpoint = s3.example.com
access_key_id = AK
secret_access_key = abc/def+ghi==
`

// writeConfig writes an rclone config file and returns its path.
func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func remotes(t *testing.T) Remotes {
	t.Helper()
	r, err := LoadRemotes(writeConfig(t, testConfig))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadRemotesReadsANameWithSpacesAndAValueWithEqualsSigns(t *testing.T) {
	got, err := remotes(t).S3("Event Media Uploads")
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "s3.example.com" || got.SecretKey != "abc/def+ghi==" {
		t.Fatalf("got endpoint %q and a secret key of %d characters", got.Endpoint, len(got.SecretKey))
	}
}

func TestLoadRemotesRejectsAnEncryptedConfig(t *testing.T) {
	p := writeConfig(t, "# Encrypted rclone configuration File\n\nRCLONE_ENCRYPT_V0:\nAAAABBBBCCCC\n")
	_, err := LoadRemotes(p)
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("got error %v", err)
	}
}

func TestLoadRemotesGivesTheLineNumberOfALineItCannotRead(t *testing.T) {
	p := writeConfig(t, "[a]\ntype = s3\nthis line has no key\n")
	_, err := LoadRemotes(p)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("got error %v", err)
	}
	if strings.Contains(err.Error(), "this line") {
		t.Fatalf("the error shows the content of the line: %v", err)
	}
}

func TestLoadRemotesFailsWhenTheFileDoesNotExist(t *testing.T) {
	_, err := LoadRemotes(filepath.Join(t.TempDir(), "none.conf"))
	if err == nil {
		t.Fatal("got no error")
	}
}

func TestConfigPathUsesTheRcloneConfigVariableFirst(t *testing.T) {
	t.Setenv("RCLONE_CONFIG", "/somewhere/my.conf")
	got, err := ConfigPath()
	if err != nil || got != "/somewhere/my.conf" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// setHome sets the home directory for all platforms and removes APPDATA.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", "")
}

func TestConfigPathUsesTheAppDataDirectoryOfWindowsFirst(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("RCLONE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	setHome(t, t.TempDir())
	t.Setenv("APPDATA", appData)
	want := filepath.Join(appData, "rclone", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigPath()
	if err != nil || got != want {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestConfigPathFindsTheDefaultFileBelowTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RCLONE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	setHome(t, home)
	want := filepath.Join(home, ".config", "rclone", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigPath()
	if err != nil || got != want {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestConfigPathUsesTheXDGDirectoryBeforeTheHomeDefault(t *testing.T) {
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("RCLONE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	setHome(t, home)
	want := filepath.Join(xdg, "rclone", "rclone.conf")
	for _, p := range []string{want, filepath.Join(home, ".config", "rclone", "rclone.conf")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ConfigPath()
	if err != nil || got != want {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestConfigPathFailsWithAClearMessageWhenThereIsNoConfigFile(t *testing.T) {
	t.Setenv("RCLONE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	setHome(t, t.TempDir())
	_, err := ConfigPath()
	if err == nil || !strings.Contains(err.Error(), "RCLONE_CONFIG") {
		t.Fatalf("got error %v", err)
	}
}

func TestS3ReadsEndpointKeysAndRegionFromTheRemote(t *testing.T) {
	got, err := remotes(t).S3("wasabi")
	if err != nil {
		t.Fatal(err)
	}
	want := S3Remote{Endpoint: "s3.ap-southeast-2.wasabisys.com", Region: "ap-southeast-2", AccessKey: "AK", SecretKey: "SK"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFTPDecodesTheObscuredPassword(t *testing.T) {
	got, err := remotes(t).FTP("HD1")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.1:21", User: "u", Pass: "hdup-test-password"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFTPUsesAnonymousAndTheConfiguredPort(t *testing.T) {
	got, err := remotes(t).FTP("HD2")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.2:2121", User: "anonymous", Pass: "anonymous"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRemoteNotFoundIsAnError(t *testing.T) {
	_, err := remotes(t).S3("nothing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got error %v", err)
	}
}

func TestRemoteOfIncorrectTypeIsAnError(t *testing.T) {
	_, err := remotes(t).S3("HD1")
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("got error %v", err)
	}
}

func TestS3AcceptsAnEndpointWithNoScheme(t *testing.T) {
	got, err := remotes(t).S3("bare")
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "s3.ap-southeast-2.wasabisys.com" {
		t.Fatalf("got endpoint %q", got.Endpoint)
	}
}

func TestS3RejectsAnEndpointWithASchemeThatIsNotHTTPS(t *testing.T) {
	for _, name := range []string{"plain", "other"} {
		_, err := remotes(t).S3(name)
		if err == nil || !strings.Contains(err.Error(), "only https endpoints are supported") {
			t.Fatalf("%s: got error %v", name, err)
		}
	}
}

func TestFTPUsesAnEmptyPasswordWhenTheRemoteHasAUserAndNoPassword(t *testing.T) {
	got, err := remotes(t).FTP("HD3")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.3:21", User: "u", Pass: ""}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
