# Control Room security contract amendment

Source: inferred implementation refinement of the accepted F19 and role
contracts, GoodQA #15361 and #15623, reconciled through two independent plan
reviews. The metadata amendment follows the actual export query's ticket and
held-stream joins. It preserves the accepted output behavior with four column
grants, rather than granting additional whole-table reads.

- AC-1: Export reads events/outbox and exactly the four joined metadata columns
  documented in [control-roles.md](control-roles.md). Its matrix represents column
  grants separately from table SELECT. Verify by: `TestExportRoleJoinedColumns`
  and `TestControlRolesPrivilegeMatrix`, including real export output, forbidden
  whole-table/other-column reads, writes, drift detection and reapply convergence.
- AC-2: Unknown JSON keys never enter rejection reasons or stderr. Known-field
  diagnostics may name only fixed schema fields. Verify by:
  `TestUnknownKeysNeverEcho` and the actual CLI rejection probes.
- AC-3: Marker text inside a larger credential match cannot exempt that match.
  Generated placeholders remain stable. Verify by:
  `TestMarkerCannotExemptCredential`, stored-row redaction and CLI stderr probes.
- AC-4: Runner refusal covers direct and assumable-member column SELECT in
  Control Room tables and views. Unrelated role drift leaves clean runners
  operational. Verify by: `TestRoleSafetyCoversControl`.
- AC-5: The absolute ingest deadline covers parsing, database work, postcommit
  notification and connection/transaction cleanup. Confirmed commit plus failed
  hint is success; uncertain commit is an operational error. Verify by:
  `TestStoreNotificationUsesOriginalDeadline`, `TestStoreUncertainCommitIsError`
  and the actual CLI blocked-input probes.
- AC-6: Password SQL is hidden from concurrent activity observers before any
  role mutation; protection is transaction-local, verified on the same
  connection, and fail-closed. Uncertain completion/restoration discards that
  connection. Verify by: `TestControlRolePasswordActivityDuringBlockedAlter`
  (stats-capable observer, actual blocked sensitive ALTER, positive baseline
  count then zero protected count), `TestControlRolesRefuseUnprotectedActivity`
  and transaction-restoration controls.
- AC-7: All loaded, trimmed password values are validated before mutation;
  empty values require explicit NOLOGIN confirmation. Verify by:
  `TestControlRolesLoadedPasswordsNeedConfirmation` and actual CLI probes.

Negative cases include short ASCII credential-shaped keys, long/control/Unicode
keys, embedded redaction markers, column-only privilege drift and NOINHERIT
members, notification after a delayed commit, uncertain commit, protection
permission denial, connection restoration failure and empty/whitespace files for
each of the three roles. Source: the same accepted contracts and seven reviewed
security findings. Full validation, independent acceptance and live deployment
are separate release gates; this document is not evidence that they passed.
