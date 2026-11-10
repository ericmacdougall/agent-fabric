package main

import "fmt"

// controlMain dispatches the Control Room's explicit operator commands.
func controlMain(args []string) error {
	if len(args) == 0 {
		return usageError{fmt.Errorf("usage: fab control export [--since CURSOR] | ingest --source NAME | bootstrap [--check-prefix PREFIX] | cursor-rollback CURSOR")}
	}
	switch args[0] {
	case "export":
		return controlExportMain(args[1:])
	case "ingest":
		return controlIngestMain(args[1:])
	case "bootstrap":
		return controlBootstrapMain(args[1:])
	case "cursor-rollback":
		return controlCursorRollbackMain(args[1:])
	default:
		return usageError{fmt.Errorf("fab control: unknown subcommand %q (usage: fab control export | ingest | bootstrap | cursor-rollback)", args[0])}
	}
}
