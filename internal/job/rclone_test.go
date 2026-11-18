package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRclone writes a substitute rclone command and returns its path.
func fakeRclone(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
case "$1" in
config) cat <<'JSON'
{"wasabi":{"type":"s3","endpoint":"https://s3.ap-southeast-2.wasabisys.com/","access_key_id":"AK","secret_access_key":"SK"},
 "HD1":{"type":"ftp","host":"10.0.0.1","user":"u","pass":"UaJq9L2mn8Bu22WaeUUnynHFKVGyPfHSLoJuB8sumfdIqA"},
 "HD2":{"type":"ftp","host":"10.0.0.2","port":"2121"},
 "HD3":{"type":"ftp","host":"10.0.0.3","user":"u"},
 "bare":{"type":"s3","endpoint":"s3.ap-southeast-2.wasabisys.com","access_key_id":"AK","secret_access_key":"SK"},
 "plain":{"type":"s3","endpoint":"http://s3.wasabisys.com","access_key_id":"AK","secret_access_key":"SK"},
 "other":{"type":"s3","endpoint":"ftp://s3.wasabisys.com","access_key_id":"AK","secret_access_key":"SK"}}
JSON
;;
reveal) exit 3 ;; # hdup must not put a password on a command line
esac
`
	p := filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestS3ReadsEndpointKeysAndRegionFromTheRemote(t *testing.T) {
	got, err := Rclone{Bin: fakeRclone(t)}.S3("wasabi")
	if err != nil {
		t.Fatal(err)
	}
	want := S3Remote{Endpoint: "s3.ap-southeast-2.wasabisys.com", Region: "ap-southeast-2", AccessKey: "AK", SecretKey: "SK"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFTPDecodesTheObscuredPasswordWithNoRcloneCommand(t *testing.T) {
	got, err := Rclone{Bin: fakeRclone(t)}.FTP("HD1")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.1:21", User: "u", Pass: "hdup-test-password"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFTPUsesAnonymousAndTheConfiguredPort(t *testing.T) {
	got, err := Rclone{Bin: fakeRclone(t)}.FTP("HD2")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.2:2121", User: "anonymous", Pass: "anonymous"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRemoteNotFoundIsAnError(t *testing.T) {
	_, err := Rclone{Bin: fakeRclone(t)}.S3("nothing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got error %v", err)
	}
}

func TestRemoteOfIncorrectTypeIsAnError(t *testing.T) {
	_, err := Rclone{Bin: fakeRclone(t)}.S3("HD1")
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("got error %v", err)
	}
}

func TestRcloneErrorIncludesTheStderrText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rclone")
	script := "#!/bin/sh\necho 'stdout secret'\necho '  config file is damaged  ' >&2\nexit 1\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Rclone{Bin: p}.S3("wasabi")
	if err == nil || !strings.Contains(err.Error(), ": config file is damaged") {
		t.Fatalf("got error %v", err)
	}
	if strings.Contains(err.Error(), "stdout") {
		t.Fatalf("the error includes stdout: %v", err)
	}
}

func TestS3AcceptsAnEndpointWithNoScheme(t *testing.T) {
	got, err := Rclone{Bin: fakeRclone(t)}.S3("bare")
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "s3.ap-southeast-2.wasabisys.com" {
		t.Fatalf("got endpoint %q", got.Endpoint)
	}
}

func TestS3RejectsAnEndpointWithASchemeThatIsNotHTTPS(t *testing.T) {
	for _, name := range []string{"plain", "other"} {
		_, err := Rclone{Bin: fakeRclone(t)}.S3(name)
		if err == nil || !strings.Contains(err.Error(), "only https endpoints are supported") {
			t.Fatalf("%s: got error %v", name, err)
		}
	}
}

func TestFTPUsesAnEmptyPasswordWhenTheRemoteHasAUserAndNoPassword(t *testing.T) {
	got, err := Rclone{Bin: fakeRclone(t)}.FTP("HD3")
	if err != nil {
		t.Fatal(err)
	}
	want := FTPRemote{Addr: "10.0.0.3:21", User: "u", Pass: ""}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
