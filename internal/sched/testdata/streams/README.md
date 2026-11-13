# Settle goldens (goodqa#15373 AC-5)

One case per `settleCommand` branch. Each case has `stream.jsonl` (in
`stream_unreadable` it is a directory, so the open succeeds and the read
fails), `exit` (the worker's exit code) and `golden.json`: the command (or
null), the error, and the exact usage.json bytes that bd56178's settleCommand
produced for it, with mission m_golden, attempt a_golden, fence 7. The
`stream_unreadable` reason embeds the run directory path; tests compare it
after replacing that path.

The goldens were produced by bd56178 code, not by this branch:

    git archive bd56178ed8658138ece6b72d0d3c8b1076369cde | (mkdir T && cd T && tar -xf -)
    python3 -I _capture/mkfixtures.py T
    cp _capture/zz_settle_export.go T/internal/sched/
    mkdir T/capture && cp _capture/capture_main.go T/capture/
    cd T/internal/sched && go run -tags settlegolden ../../capture/

Run on t2-dev on 2026-11-12. The capture sources are under `_capture/` and
are not built (testdata is ignored by the go tool).
