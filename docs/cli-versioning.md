# Versioning CLI

The existing binary name remains `badger`. Commands are simple and scriptable; no TUI is introduced in Phase 1.

Common flags:

```text
--dir PATH
--read-only
--output human|json
--limit N
--since CLOCK
--yes
--value-output text|raw|hex|base64|json
```

Examples:

```bash
badger --dir ./db version put hello world
badger --dir ./db version get hello --value-output text
badger --dir ./db version history hello --output json
badger --dir ./db version show hello VERSION_ID --output json
badger --dir ./db version delete hello --yes
badger --dir ./db version rollback hello VERSION_ID --yes
badger --dir ./db audit operations --since 10 --limit 50 --output json
badger --dir ./db audit operation OPERATION_ID --output json
badger --dir ./db audit pending --output json
badger --dir ./db audit failed --output json
badger --dir ./db audit retry OPERATION_ID
badger --dir ./db git status --audit-dir ./audit --output json
badger --dir ./db git push --audit-dir ./audit --remote origin --branch main
badger --dir ./db softserve remote set ssh://git@example.com/repo.git --audit-dir ./audit
badger --dir ./db softserve remote test --ssh-url ssh://git@example.com/repo.git --audit-dir ./audit
```

Raw value output prints only the raw value. Human text mode avoids dumping unsafe binary bytes directly by falling back to base64 when the value is not safely printable. Destructive operations require `--yes`; in non-interactive environments they fail safely without it.

## Not implemented

Phase 1 CLI commands do not implement snapshots, timelines, detached workspaces, networking, master/replica sync, peer-to-peer sync, Raft, consensus, clustering, distributed conflict resolution, or a Charmbracelet TUI.
