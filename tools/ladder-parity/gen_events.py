#!/usr/bin/env python3
"""Golden oracle for goodqa#15324 S1b (AC-1, PL-9).

Drives the REAL escalation.py Ledger with a seeded sequence of receipts and
writes, for every step, the record escalation.py classified (the shape
`fab ladder record` accepts) and the ladder_status after that step. The Go
tests replay the same records through Fabric and compare rung, failures,
fingerprint, climbs, blocked_by, launches and spend at EVERY prefix.

Usage: gen_events.py --hooks ~/.claude/hooks --seed 15324 --steps 300 --out testdata/rung_oracle.json
The Go tests never regenerate the file.
"""
import argparse
import hashlib
import json
import random
import sys
import tempfile
from pathlib import Path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--hooks', required=True)
    ap.add_argument('--seed', type=int, default=15324)
    ap.add_argument('--steps', type=int, default=300)
    ap.add_argument('--out', required=True)
    a = ap.parse_args()
    sys.path.insert(0, a.hooks)
    import escalation as esc  # noqa: E402
    import model_tiers as tiers  # noqa: E402

    policy = tiers.load_policy()
    rng = random.Random(a.seed)
    root = Path(tempfile.mkdtemp(prefix='ladder-oracle-'))
    ledger = esc.Ledger(root=root, policy=policy)
    task = 'oracle'
    findings = ['RT-1', 'RT-2', 'F9']
    heads = [format(rng.getrandbits(160), '040x') for _ in range(4)] + [format(rng.getrandbits(28), '07x')]
    steps, prev_spend, ord_n = [], 0.0, 0

    def state():
        return ledger._read(task)[0]

    def status_of(st):
        s = esc.ladder_status(st, policy)
        return {'rung': s['rung'], 'failures_at_rung': s['failures_at_rung'], 'fingerprint': s['fingerprint'],
                'climbs': s['climbs'], 'blocked_by': s['blocked_by'], 'needs_user_permission': s['needs_user_permission'],
                'launches': st.get('launches', 0), 'spend_micros': int(round(float(st.get('spend_usd', 0.0)) * 1_000_000))}

    for i in range(a.steps):
        before = state()
        pick = rng.random()
        if pick < 0.12:
            head = rng.choice(heads)
            ledger.claim_fix(task, head)
            kind = 'claim'
        elif pick < 0.2:
            usd = rng.choice([0.5, 0.5, 1.25, 2.0])
            ledger.add_spend(task, usd)
            kind = 'spend'
        else:
            kind = rng.choice(['review'] * 5 + ['test', 'spec_review'])
            fnd = [{'id': rng.choice(findings), 'file': 'f.go', 'category': 'c'}
                   for _ in range(rng.choice([0, 1, 1, 1, 2]))]
            head = rng.choice(heads + [None])
            if kind == 'spec_review':
                rec = {'verdict': rng.choice(['BLOCK', 'BLOCK', 'ACCEPT', 'NEEDS']), 'findings': fnd, 'n': i}
            else:
                rec = {'kind': 'final', 'evidence': rng.choice(['EXECUTED'] * 14 + ['NOT_EXECUTED']),
                       'usage': {'provider_launches': rng.choice([0, 1, 1, 1, 1, 2])},
                       'reason': rng.choice([None] * 8 + ['TRANSIENT_INFRA', 'QUOTA', 'CODE_FINDINGS', 'CODE_FINDINGS']),
                       'execution': rng.choice([None] * 12 + ['INTERRUPTED']), 'n': i,
                       'subject': {'head': head}, 'result': rng.choice(['CHECK_OK', 'CHECK_FAIL', None]),
                       'invocation': {'fingerprint': rng.choice(['inv-a', 'inv-b'])}}
                if kind == 'review':
                    rec['review'] = {'verdict': rng.choice(['BLOCK'] * 4 + ['ACCEPT']), 'findings': fnd}
            ledger.record(task, json.dumps(rec, sort_keys=True).encode(), kind=kind, source=kind)
        st = state()
        spend_delta = int(round((float(st.get('spend_usd', 0.0)) - float(before.get('spend_usd', 0.0))) * 1_000_000))
        if kind == 'spend':
            ord_n += 1
            record = {'class': 'NOT_COUNTED', 'step': 'spend', 'spend_micros': spend_delta, 'ord': ord_n, 'src': 'add-spend'}
        elif kind == 'claim':
            ev = st['events'][-1] if len(st['events']) > len(before['events']) else None
            if ev is None:  # a repeated claim_fix is a no-op in Python; nothing to record
                continue
            record = {'class': 'CLAIM', 'step': 'claim', 'commit': ev['head'], 'src': 'claim-fix'}
        else:
            ev = st['events'][-1]
            launched = ev['kind'] == 'review' and st.get('launches', 0) > before.get('launches', 0)
            rd = json.loads(json.dumps(rec))
            launched_flag = isinstance(rd.get('usage'), dict) and isinstance(rd['usage'].get('provider_launches'), int) \
                and rd['usage']['provider_launches'] >= 1
            record = {'class': ev['class'], 'step': ev['kind'], 'rsha': ev['receipt_sha256'], 'src': ev['source']}
            if ev['fingerprint'] is not None:
                record['fp'] = ev['fingerprint']
            if ev.get('head') is not None:
                record['commit'] = ev['head']
            if launched_flag:
                record['launched'] = True
            if launched:
                record['counted'] = True
            if spend_delta:
                record['spend_micros'] = spend_delta
            ids = [f['id'] for f in esc.named_findings(rec.get('review', rec).get('findings'))] if kind != 'test' else []
            if ids and ev['class'] == 'FAIL':
                record['ids'] = sorted(set(ids))[:32]
        if len(st['events']) == len(before['events']) and kind not in ('spend',):
            continue  # replayed receipt (identical bytes): not a new event
        steps.append({'record': record, 'status': status_of(st)})
    # Receipts Fabric must refuse (NEG-4): escalation.py would store these head
    # values (it does not validate a receipt's own head), Fabric refuses them.
    refusals = [{'why': 'non-hex head', 'record': {'class': 'FAIL', 'step': 'review', 'fp': 'a' * 64, 'rsha': 'b' * 64,
                                                    'commit': 'not-a-sha', 'launched': True}},
                {'why': 'fingerprint is a finding id', 'record': {'class': 'FAIL', 'step': 'review', 'fp': 'RT-1', 'rsha': 'b' * 64,
                                                                   'launched': True}}]
    out = {'seed': a.seed, 'policy': policy, 'steps': steps, 'refusals': refusals,
           'generator_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    Path(a.out).write_text(json.dumps(out, indent=1, sort_keys=True) + '\n')
    print('wrote %d steps to %s' % (len(steps), a.out))


if __name__ == '__main__':
    main()
