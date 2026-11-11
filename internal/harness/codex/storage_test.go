package codex

import (
	"strings"
	"testing"
)

func TestControlledStorageUsesOneFreshDirectory(t *testing.T) {
	argv, err := FileEditStorageCommand("/pinned/codex", "fabric_demo_test", "/owned/run", "/owned/run.control/sqlite")
	text := strings.Join(argv, " ")
	if err != nil || !strings.Contains(text, `sqlite_home="/owned/run.control/sqlite"`) || !strings.Contains(text, `log_dir="/owned/run.control/sqlite"`) || argv[len(argv)-1] != "app-server" {
		t.Fatalf("wrong storage binding %v %s", err, text)
	}
	for _, bad := range []string{"/owned/existing", "/owned/run/sqlite", "relative", "/owned/run.control/../sqlite"} {
		if _, err := FileEditStorageCommand("/pinned/codex", "fabric_demo_test", "/owned/run", bad); err == nil {
			t.Fatal("unowned storage accepted")
		}
	}
}
