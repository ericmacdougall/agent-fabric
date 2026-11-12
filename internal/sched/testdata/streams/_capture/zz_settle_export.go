//go:build settlegolden

package sched

import "fabric/internal/domain"

// SettleCommandForGolden exposes bd56178's settleCommand to the capture
// program (goodqa#15373 AC-5); exists only in the capture scratch tree.
func SettleCommandForGolden(m domain.MissionID, a domain.AttemptID, fence int64, runDir string, code int) (*domain.Command, error) {
	return settleCommand(m, a, fence, runDir, code)
}
