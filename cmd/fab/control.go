package main

import "fmt"

// controlMain dispatches `fab control <subcommand>` (goodqa#15586). Only
// export lives here; later slices add their subcommands to this switch.
func controlMain(args []string) error {
	if len(args) == 0 {
		return usageError{fmt.Errorf("usage: fab control export [--since CURSOR] | ingest --source NAME")}
	}
	switch args[0] {
	case "export":
		return controlExportMain(args[1:])
	case "ingest":
		return controlIngestMain(args[1:])
	default:
		return usageError{fmt.Errorf("fab control: unknown subcommand %q (usage: fab control export [--since CURSOR] | ingest --source NAME)", args[0])}
	}
}
