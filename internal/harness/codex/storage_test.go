package codex

import (
	"reflect"
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

func TestControlledFeaturesPreserveOverridesAndAppServerStrictness(t *testing.T) {
	server, err := FileEditStorageCommand("/pinned/codex", "fabric_demo_test", "/owned/run", "/owned/run.control/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	original := append([]string(nil), server...)
	argv, err := FileEditFeatureCommand(server)
	if err != nil || len(argv) != len(server) || !reflect.DeepEqual(argv[1:len(argv)-2], server[2:len(server)-1]) || argv[len(argv)-2] != "features" || argv[len(argv)-1] != "list" || !reflect.DeepEqual(server, original) {
		t.Fatalf("metadata changed overrides or app-server argv: %v", err)
	}
	if _, err := FileEditFeatureCommand([]string{"/pinned/codex", "features", "list"}); err == nil {
		t.Fatal("accepted non-app-server input")
	}
}
