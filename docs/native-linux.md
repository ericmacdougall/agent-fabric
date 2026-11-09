# Running the native loop on Linux

The native loop (`fab mission run`, `fab harness run --adapter native`) refuses to start tools until the host advertises the `linux-cgroup-landlock` boundary. On a same-UID host two of its nine controls always fail (`controlling_user_manager_unreachable`, `controlling_user_process_unsignallable`), so the loop is refused there by design. There is no fake-host shortcut for a real run.

## What the boundary needs

- A dedicated, locked worker account with a running (lingering) user manager. On the fleet host this is `fab-worker`, with a sudoers rule that lets the daemon account run `env`, `systemd-run`, `systemctl`, `kill`, `pgrep` and `sh` as that account.
- `FABRIC_WORKER_USER=<account>` and `FABRIC_WORKER_ROOT=<dir>`, a directory both accounts can write (mode 2775, group shared), used for the control scratch space. When it is set, the controls create their own `nc-<time>` directories under it and `--scratch` is not used.
- A `fab` binary the worker account can execute. The sandbox re-executes `fab __sandbox` as the worker, so a binary under a mode-0750 home directory fails with "Failed to find executable". The deployed `/srv/fabric/bin/fab-live` qualifies.
- The grant's `worktree` and `run_dir` under a directory the worker can read and write (group `fabric`, setgid), and `required_controls` naming the controls the run depends on. An empty list is refused ("no required controls configured").
- `oracle_hooks` configured: the policy oracle fails closed without hooks.

## Check the boundary

    FABRIC_WORKER_USER=fab-worker FABRIC_WORKER_ROOT=/srv/fabric/work \
      fab host controls --scratch /srv/fabric/work/<scratch dir, mode 2775>

Expected: `linux-cgroup-landlock: 9/9 controls passed`. Each control start uses its own run id (goodqa#15466). The usual cause of the abort `worker environment file ... file exists` is a `fab` binary the worker account cannot execute (for example one under a mode-0700 home directory): the sandbox never reads its environment file and the next control with the same run id then collides. With a per-control run id that situation is reported as per-control failures (`Failed to find executable ... Permission denied`) and the profile is refused, instead of aborting; it does not turn a failing host into a passing one. A binary the worker can run passes 9/9 with or without this change.

## The key

The grant's `key_command` must be able to read the provider key. It runs with the daemon account's environment and sees only its own stdout. The account running it needs permission to access the secret value, not only to list secrets; a command that fails with a permission error ends the run before any request.
