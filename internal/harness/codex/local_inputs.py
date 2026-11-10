# Dedicated metadata-only local input check for the reviewed Darwin client.
# Never opens authentication, history, memory or conversation files.
import json, os, re, stat, sys, tomllib
from pathlib import Path


def refuse():
    print('{"error":"unapproved_local_input"}')
    raise SystemExit(1)


try:
    home = Path.home()
    workspace = Path(sys.argv[1])
    path = home / '.codex/config.toml'
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_size > 262144:
        refuse()
    config = tomllib.loads(path.read_text())
    # Every additional input-bearing root is rejected; these UI/project settings
    # cannot override fixed argv policy and the new empty workspace has no project.
    allowed = {'approval_policy', 'browser_use', 'default_permissions', 'desktop',
               'hooks', 'marketplaces', 'mcp_servers', 'model', 'model_reasoning_effort',
               'model_verbosity', 'notify', 'permissions', 'plugins', 'projects',
               'service_tier', 'tui', 'web_search'}
    if set(config) - allowed:
        refuse()
    for candidate in [Path('/etc/codex/config.toml'), Path('/etc/codex/requirements.toml'),
                      Path('/etc/codex/managed_config.toml'), home / '.codex/requirements.toml',
                      home / '.codex/managed_config.toml', home / '.codex/AGENTS.override.md']:
        if candidate.exists() or candidate.is_symlink():
            refuse()
    # System MDM settings are intentional policy but require their own concrete
    # audit; never decode their potentially credential-bearing values here.
    import ctypes
    cf = ctypes.CDLL('/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation')
    cf.CFStringCreateWithCString.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_uint32]
    cf.CFStringCreateWithCString.restype = ctypes.c_void_p
    cf.CFPreferencesAppValueIsForced.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    cf.CFPreferencesAppValueIsForced.restype = ctypes.c_bool
    cf.CFRelease.argtypes = [ctypes.c_void_p]
    app = cf.CFStringCreateWithCString(None, b'com.openai.codex', 0x08000100)
    try:
        for name in [b'config_toml_base64', b'requirements_toml_base64']:
            key = cf.CFStringCreateWithCString(None, name, 0x08000100)
            try:
                if cf.CFPreferencesAppValueIsForced(key, app):
                    refuse()
            finally:
                cf.CFRelease(key)
    finally:
        cf.CFRelease(app)
    # No inherited cwd instructions or project config may be discovered.
    for parent in [workspace, *workspace.parents]:
        for name in ['AGENTS.md', 'AGENTS.override.md', '.codex/config.toml']:
            f = parent / name
            if f == path:
                continue  # the one user config was parsed and classified above
            if f.exists() or f.is_symlink():
                refuse()
    ids = []
    for kind in ['mcp_servers', 'plugins']:
        entries = config.get(kind, {})
        if not isinstance(entries, dict):
            refuse()
        for identity in entries:
            if not re.fullmatch(r'[A-Za-z0-9_./@-]{1,160}', identity) or re.search(r'(?i)sk-|token|password|secret|credential', identity):
                refuse()
            ids.append({'Kind': kind, 'ID': identity})
    print(json.dumps({'disabled': ids, 'global_agents': str(home / '.codex/AGENTS.md')}))
except Exception:
    refuse()
