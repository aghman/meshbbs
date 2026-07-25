# Configuration reference

Generated from the source. Do not edit by hand.

| Key | Type | Default | Environment variable | Description |
|---|---|---|---|---|
| `log.file` | string | *(empty)* | `MESHBBS_LOG_FILE` | Path to a log file. Empty logs to stderr. |
| `log.format` | string | `text` | `MESHBBS_LOG_FORMAT` | text or json. |
| `log.level` | string | `info` | `MESHBBS_LOG_LEVEL` | debug, info, warn, or error. |
| `node.display_name` | string | `meshbbs` | `MESHBBS_NODE_DISPLAY_NAME` | Self-declared label published in this node's NODE record. Not unique, not authoritative, never used for routing (§6.1.4). |
| `node.environment` | string | `production` | `MESHBBS_NODE_ENVIRONMENT` | 'development' or 'production'. The dev subcommands refuse to run against a production datadir (§6.7). |
| `node.sysop_contact` | string | *(empty)* | `MESHBBS_NODE_SYSOP_CONTACT` | Free-text contact address for the sysop, published in the NODE record. |
| `node.sysop_name` | string | *(empty)* | `MESHBBS_NODE_SYSOP_NAME` | Sysop's name, for display. |
| `node.timezone` | string | `Local` | `MESHBBS_NODE_TIMEZONE` | IANA timezone name used for display. Wall-clock time is advisory (§6.2.1). |
| `storage.data_dir` | string | *(empty)* | `MESHBBS_STORAGE_DATA_DIR` | Root data directory. Empty selects the OS convention (~/.local/share/meshbbs, ~/Library/Application Support/MeshBBS, %APPDATA%\MeshBBS). |
| `storage.database` | string | `bbs.db` | `MESHBBS_STORAGE_DATABASE` | SQLite database filename, relative to data_dir unless absolute. |
| `storage.keys_dir` | string | `keys` | `MESHBBS_STORAGE_KEYS_DIR` | Directory holding node and host keys, relative to data_dir unless absolute. Must be mode 0700 with 0600 keys. |
