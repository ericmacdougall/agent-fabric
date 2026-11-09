#!/usr/bin/env python3
"""Independent generator for internal/envelope/testdata/vectors.json (goodqa#15372).

It shares no code with the Go implementation: the event id and the canonical
bytes of the v2 view are computed here from the field table in
docs/envelope.md. `python3 -I tools/gen_vectors.py` rewrites the vector file
and `python3 -I tools/gen_vectors.py --check` fails if the committed file
differs. Run it from the repository root.
"""
import hashlib
import json
import struct
import sys
from datetime import datetime, timezone

CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def event_id(mission: str, seq: int, at_ms: int) -> str:
    body = hashlib.sha256(
        b"fabric.v1.event_id\x00" + struct.pack(">I", len(mission)) + mission.encode("ascii") + struct.pack(">Q", seq)
    ).digest()[:10]
    raw = at_ms.to_bytes(6, "big") + body  # 48 bit time then 80 bit body: 128 bits
    n = int.from_bytes(raw, "big")  # 128 bit integer; 26 groups of 5 bits = 130 bits
    return "evt_" + "".join(CROCKFORD[(n >> (5 * i)) & 31] for i in reversed(range(26)))


def at_string(at_us: int) -> str:
    dt = datetime.fromtimestamp(at_us // 1_000_000, tz=timezone.utc)
    return dt.strftime("%Y-%m-%dT%H:%M:%S") + ".%06dZ" % (at_us % 1_000_000)


def canonical(obj) -> str:
    # RFC 8785 for this subset: integers, ASCII strings, objects, arrays, no floats.
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


CASES = [
    # name, mission, seq, kind, actor, at in microseconds, payload
    ("admitted", "m_demo1", 1, "MISSION_ADMITTED", "chat", 1_762_646_400_123_456,
     {"kind": "MISSION_ADMITTED", "mission": "m_demo1", "seq": 1, "revision": 1, "generation": 1}),
    ("fraction-500", "m_demo1", 2, "ATTEMPT_STARTED", "fabd", 1_762_646_400_000_500,
     {"kind": "ATTEMPT_STARTED", "mission": "m_demo1", "seq": 2, "attempt": "r_demo1_1", "fence": 1, "host": "h_demo"}),
    ("fraction-999", "m_demo1", 3, "ATTEMPT_FINISHED", "native-runner", 1_762_646_400_000_999,
     {"kind": "ATTEMPT_FINISHED", "mission": "m_demo1", "seq": 3, "attempt": "r_demo1_1", "fence": 1, "outcome": "checkpoint"}),
    ("max-mission", "m_" + "x" * 62, 7, "MISSION_RELEASED", "op:alice", 1_762_646_401_000_000,
     {"kind": "MISSION_RELEASED", "mission": "m_" + "x" * 62, "seq": 7}),
    ("seq-large", "m_demo1", 4294967296, "DELIVERED", "fabd", 1_762_646_402_654_321,
     {"kind": "DELIVERED", "mission": "m_demo1", "seq": 4294967296}),
]


def build():
    out = []
    for name, mission, seq, kind, actor, at_us, payload in CASES:
        at_ms = at_us // 1000
        view = {
            "schema_version": 2,
            "event_id": event_id(mission, seq, at_ms),
            "tenant_id": "t_personal",
            "principal": {"issuer": "legacy-unverified", "subject": actor},
            "mission": mission,
            "seq": seq,
            "kind": kind,
            "at": at_string(at_us),
            "payload": payload,
        }
        out.append({
            "name": name, "mission": mission, "seq": seq, "kind": kind, "actor": actor, "at_micros": at_us,
            "payload": canonical(payload), "event_id": view["event_id"], "canonical": canonical(view),
        })
    refused = [
        {"name": "non-ascii-mission", "mission": "m_café", "seq": 1, "at_micros": 1_762_646_400_000_000},
        {"name": "empty-mission", "mission": "", "seq": 1, "at_micros": 1_762_646_400_000_000},
        {"name": "long-mission", "mission": "m" * 65, "seq": 1, "at_micros": 1_762_646_400_000_000},
    ]
    return {"version": 1, "cases": out, "refused": refused}


def main():
    text = json.dumps(build(), indent=1, sort_keys=True, ensure_ascii=True) + "\n"
    path = "internal/envelope/testdata/vectors.json"
    if "--check" in sys.argv:
        with open(path) as f:
            if f.read() != text:
                print("vectors.json differs from the generator output")
                sys.exit(1)
        print("vectors.json matches")
        return
    with open(path, "w") as f:
        f.write(text)
    print("wrote", path, hashlib.sha256(text.encode()).hexdigest())


if __name__ == "__main__":
    main()
