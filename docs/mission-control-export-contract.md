# Mission Control pull contract and producer fixtures

Source: amended Fabric takeover plan accepted by main2026-11-10, GoodQA15361.
`internal/control/export/testdata/mc-consumer-contract.json` contains public
synthetic producer events and opaque cursors for the separate MC owner. Its
events are checked against actual MapRow/MapLag output and the pinned schema;
the maximum outbox position is checked against the safe128-byte cursor contract.
The fixture is not a live-database resume token and is not a consumer test result.

For each case, resolve event_keys through events, encode one object per line,
then append the sole final object {"cursor": cursors[next_cursor]}. Keys in
since refer to cursors; null means the initial pull must omit --since entirely.
Never substitute0 or silently reset a refused cursor. More/Lagging are producer
metadata for the scenario, not additional stdout lines.
Saved-cursor expectations use the same named references. MC treats tokens as
opaque and does not need to decode their internal version or checksum; the
producer owns those checks. The numeric0 case is an exporter refusal, distinct
from the unsafe transport strings and129-character response cases.

Run these fixtures through the actual MC fetch, normalization, append and
FileDrop paths. Preserve historic:true on every initial/paginated/resumed old
event and lag notice; those events may enter history but must not pulse. A stall
with More=false/Lagging>0 keeps backfill. After release, the remaining rows stay
historic; the next genuinely new event is live. Repeat the same lag ID to prove
deduplication. An explicit reset omits --since and replays stable IDs as historic;
seen IDs must neither duplicate history nor pulse again.

The collector must retain its saved cursor and accept no partial result when a
fetch fails. It must also retain that cursor when appending the fetched page
fails; receiving a new token alone is not acknowledgement that rows are durable.
Reject unsafe, overlong, malformed/missing final-cursor responses without
advancing; overlong_cursor describes the129-character negative fixture. Initial
and resume argv must stay within the real allowlist and use no shell interpolation.

Producer DB coverage is TestBackfillRemainsHistoricWhileLagging,
TestPublishedCursorResumeFromStore, TestOutputCap, TestCursorResume,
TestSubjectTicket and TestTicketOriginalTextFromStore. TestStaleCursor explicitly
refuses numeric0 rather than guessing a reset. Producer fixture coverage is
TestMissionControlProducerFixtures. MC must supply its own actual consumer and
deployed proof; no consumer source or live install is changed by these fixtures.
