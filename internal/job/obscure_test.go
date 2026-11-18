package job

import "testing"

func TestRevealDecodesAPasswordThatRcloneObscured(t *testing.T) {
	// Made with: rclone obscure "hdup-test-password"
	got, err := reveal("UaJq9L2mn8Bu22WaeUUnynHFKVGyPfHSLoJuB8sumfdIqA")
	if err != nil || got != "hdup-test-password" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRevealRejectsTextThatIsNotAnObscuredPassword(t *testing.T) {
	for _, s := range []string{"not base64 !", "c2hvcnQ"} {
		if _, err := reveal(s); err == nil {
			t.Fatalf("got no error for %q", s)
		}
	}
}
