package crashpoint

// exempt lists the durable writes in the inventory packages that carry no
// point of their own, one reason each. A key is dir:Func:callee, or
// dir:Func:* for every write in that function. Two kinds of entry:
//   - primitive: the body of a write the table covers at each of its call
//     sites (a crash inside it is a crash at that point's before or after,
//     or a rolled-back transaction);
//   - off-path: not a hold, FINISH, settle, release or recovery write of the
//     M1 token path.
var exempt = map[string]string{
	// Primitives, covered at their call sites.
	"records:Append:writeAtomic":        "primitive: records.Append, covered at each call site",
	"records:writeAtomic:*":             "primitive: records.Append's temp-file write and rename",
	"sched:writeFileAtomic:os.Rename":   "primitive: writeFileAtomic, covered at each call site",
	"credential:writeDurable:*":         "primitive: MarkSettled and SaveMaterial's durable write, covered at their call sites",
	"store:Store.OperatorHold:Exec":     "primitive: OperatorHold, covered at hold.operator and recovery.hold",
	"store:Store.HoldMission:Exec":      "primitive: HoldMission, covered at finish.hold.w3",
	"store:Store.ReleaseOperatorHold:*": "primitive: the release transaction, covered at release.record and release.commit",
	"store:Store.Execute:*":             "primitive: one command transaction; covered at its M1 call sites (finish.*, recovery.expire, recovery.quarantine)",
	"store:Store.applyAttemptRow:*":     "primitive: inside Execute's transaction",
	"sched:Scheduler.finishCmd:Execute": "primitive: finishRetry's body, covered at finish.m1 and finish.prelaunch",

	"credential:Policy.writeBinding:*": "primitive: PrepareLaunch's binding record, covered at launch.binding",

	// Not durable, or not M1.
	"sched:Scheduler.runOnce:HoldMission": "off-path: the F1a undecodable-log hold after a prelaunch abandon; not M1, before any binding",
	"cmd/fab:unlinkedFile:*":              "not durable: the sweep unit's stdin temp file, unlinked, never read after a crash",

	// Pre-M1 paths (Config.Credential nil).
	"sched:Scheduler.afterWaitRun:finishRetry": "pre-M1: the non-token FINISH (Config.Credential nil)",
	"sched:Scheduler.afterWaitRun:HoldMission": "pre-M1: the non-token refused-token hold after FINISH (reported to the owner)",
	"sched:Scheduler.settle:finishRetry":       "pre-M1: the legacy settle",
	"sched:Scheduler.finish:finishRetry":       "pre-M1: the plain finish helper",

	// Off-path scheduler bookkeeping.
	"sched:Scheduler.Run:Exec":                 "off-path: LISTEN on the wake channel",
	"sched:Notify:Exec":                        "off-path: NOTIFY",
	"sched:Scheduler.bump:Exec":                "off-path: a metrics counter",
	"sched:Scheduler.consumeOutbox:*":          "off-path: waits bookkeeping",
	"sched:Scheduler.wakeExitedJobs:*":         "off-path: host jobs (goodqa#15082)",
	"sched:Scheduler.retireRequestedJobs:Exec": "off-path: host jobs",
	"sched:FinishJob:Exec":                     "off-path: host jobs",
	"store:Store.heartbeat:Exec":               "off-path: lease renewal (a lapse is what recovery.expire tests)",

	// Off-path store operations.
	"store:Store.migrateUpTo:*":                         "off-path: schema migration",
	"store:Store.Truncate:*":                            "off-path: test reset",
	"store:Store.VerifiedSnapshot:Commit":               "off-path: a read-only transaction",
	"store:Store.ApplyRolesPolicy:*":                    "off-path: role provisioning",
	"store:Store.CreateMission:Exec":                    "off-path: mission creation",
	"store:Store.SetCap:Exec":                           "off-path: slot cap",
	"store:Store.SetCapacity:*":                         "off-path: capacity",
	"store:Store.Hold:*":                                "off-path: resource reservations (named Hold, not the operator hold)",
	"store:Store.closeReservation:*":                    "off-path: resource reservations",
	"store:Store.decideHold:*":                          "off-path: resource reservations",
	"store:Store.AuthorizeDispatch:*":                   "off-path: dispatch authorization",
	"store:Store.AckOutbox:Exec":                        "off-path: outbox cursor",
	"store:Store.ConsumeOutbox:*":                       "off-path: outbox consumer (the operator hold row only shares the table)",
	"store:Store.drainOne:*":                            "off-path: W3 hold drain of the outbox",
	"store:ensureCursor:Exec":                           "off-path: outbox cursor",
	"store:hint:Exec":                                   "off-path: NOTIFY hint",
	"store:HostLock.Check:Exec":                         "off-path: reconcile host lock probe",
	"store:ladderPrepare:Exec":                          "off-path: ladder receipts",
	"store:Store.RecordLadderReceipt:Execute":           "off-path: ladder receipts",
	"store:Store.AnswerAsk:*":                           "off-path: asks",
	"store:Store.WithdrawAsk:*":                         "off-path: asks",
	"store:Store.wakeForAsk:Execute":                    "off-path: asks",
	"store:Store.EnsureAsk:QueryRow(write SQL)":         "off-path: asks",
	"store:Store.EnsureMissionSpec:QueryRow(write SQL)": "off-path: mission specs",

	// Off-path commands.
	"cmd/fab:daemonMain:os.WriteFile":          "off-path: the checkpoint schema file at daemon start",
	"cmd/fab:execute:*":                        "off-path: `fab` command intents (pfile) and their Execute",
	"cmd/fab:harnessProviderGate:os.RemoveAll": "off-path: provider gate scratch",
	"cmd/fab:installSealed:*":                  "off-path: provider gate install",
	"cmd/fab:providerMain:os.WriteFile":        "off-path: native capture",
	"cmd/fab:runClaudeCode:os.RemoveAll":       "off-path: harness scratch",
	"cmd/fab:sandboxMain:Exec":                 "off-path: syscall.Exec, not a database write",
}
