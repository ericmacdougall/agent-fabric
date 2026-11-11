package main

// Reviewed native hook script identities, separate from native definition hashes.
// An update requires source review; it is not permission to trust a changed hook.
var claudeHookScriptSHA = map[string]string{
	"agent_gate.py":         "d8bfdf626af96fd69e51e266db94d7936b5792b243c984aa57dda859868da9cc",
	"bg_lint.py":            "eb4fa5065e4ecfbe890638226b08b9f6687918cc1ab7303e4b7d656ba0314cc4",
	"command_guard_hook.py": "3877823f1599e54fe4f3e7edb905c8208efffdf828865abec3c6633f39dbf677",
	"commit_policy.py":      "e0f2591bc137d19a584c07f61d3589203f3e873fbd062fd97a61d494a5332934",
	"dropped_gate.py":       "af7f73786b744c65805dbbef265ac06becea6b92dc851f21f7661215d47a06bc",
	"heavy_job_guard.py":    "a78e1f274f4eb3f673c71fa79bdccaef24a946ee8272d525166fd9f4094d7064",
	"kill_guard.py":         "bbeb3f64256df3c61cdeeb4f4436e8316e27a8d3b1256c782f923828597184a7",
	"ledger_reminder.py":    "0a9a60d2586511fccbdb1e878c3ab1e84a4f37752e4c3dc1ea908fd35c345593",
	"log_backup.py":         "c6496f970bbc3de404b81358324f7aa1f6f021f9ae1a4d652995c909fc2883a1",
	"mc_inbox.py":           "6339713e26ace767e801aaa8e00e3982bbbf8ed2dc52d3bd9c2f9ba6782a6d30",
	"merge_gate.py":         "dcaa5f43c78dba0f2d3767961b34edd3d43aea6a387bdde957c96053b589fd45",
	"prod_delete_guard.py":  "867df783774a919bb593441995bcb04d643b8bef48240d16cdaf29a17acbce32",
	"provenance_hooks.py":   "8da3f7cae94c7e9ea665031f5c4153a1a7741abba70ce7241f657d4b5e8d6f0c",
	"spec_flow.py":          "a9627d7fd737894eab5128016d28d8879f1e0e6b24bfc8ac8e33d42c9b5a783a",
	"tier_gate.py":          "58f8889b50ff7bd2fa8fd713ec4a721cb1c0573826d2b8442e9ea55b94888c82",
	"wal.py":                "8514ada9ec57c34e7965a608e98511ff4c935f4a1b9b01df93fa3a549ecdefc7",
	"wal_cadence.py":        "949a5687a470273d0e4c0dd0772a5c5006aa1a663ebd2e4c824bd6f35e314857",
	"worktree_hygiene.py":   "8c55dab2812b53ab148ea8d269902646cd14f6234bd7049aa19e9a15a0319496",
}
