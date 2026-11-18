//go:build !windows

package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsAFileThatOtherUsersCanRead(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		p := filepath.Join(t.TempDir(), "hdup.toml")
		if err := os.WriteFile(p, []byte(validConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %o: got error %v", mode, err)
		}
	}
}
