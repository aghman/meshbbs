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
| `ssh.bind` | string | `0.0.0.0` | `MESHBBS_SSH_BIND` | Address to listen on. |
| `ssh.enabled` | bool | `true` | `MESHBBS_SSH_ENABLED` | Serve SSH. |
| `ssh.max_sessions` | int | `32` | `MESHBBS_SSH_MAX_SESSIONS` | Maximum concurrent sessions. |
| `ssh.port` | int | `2222` | `MESHBBS_SSH_PORT` | Port to listen on. 2222 avoids needing root; 22 is conventional for a dedicated host. |
| `storage.data_dir` | string | *(empty)* | `MESHBBS_STORAGE_DATA_DIR` | Root data directory. Empty selects the OS convention (~/.local/share/meshbbs, ~/Library/Application Support/MeshBBS, %APPDATA%\MeshBBS). |
| `storage.database` | string | `bbs.db` | `MESHBBS_STORAGE_DATABASE` | SQLite database filename, relative to data_dir unless absolute. |
| `storage.files_dir` | string | `files` | `MESHBBS_STORAGE_FILES_DIR` | Directory holding file areas, served over SFTP. Relative to data_dir unless absolute. |
| `storage.keys_dir` | string | `keys` | `MESHBBS_STORAGE_KEYS_DIR` | Directory holding node and host keys, relative to data_dir unless absolute. Must be mode 0700 with 0600 keys. |
| `telnet.bind` | string | `0.0.0.0` | `MESHBBS_TELNET_BIND` | Address to listen on. |
| `telnet.enabled` | bool | `false` | `MESHBBS_TELNET_ENABLED` | Serve telnet. OFF by default: telnet is plaintext, so anything typed can be read by anyone on the network path ([D12]). |
| `telnet.guest_only` | bool | `true` | `MESHBBS_TELNET_GUEST_ONLY` | Serve read-only guest sessions only. Recommended: browsing over plaintext costs nothing, typing a password over it does. |
| `telnet.port` | int | `2323` | `MESHBBS_TELNET_PORT` | Port to listen on. 23 is conventional but needs root. |
| `theme.default` | string | `classic` | `MESHBBS_THEME_DEFAULT` | Built-in or file theme name. Run the BBS and press N for the list. |
| `theme.default_encoding` | string | `auto` | `MESHBBS_THEME_DEFAULT_ENCODING` | auto, utf8, or cp437. 'auto' guesses from the client's locale and terminal type. |
| `theme.dir` | string | `themes` | `MESHBBS_THEME_DIR` | Directory scanned for *.toml style overrides, relative to data_dir unless absolute ([N5]). |
| `users.default_directory_listed` | bool | `true` | `MESHBBS_USERS_DEFAULT_DIRECTORY_LISTED` | Whether new users are listed in the network directory ([N9]). |
| `users.guest_enabled` | bool | `true` | `MESHBBS_USERS_GUEST_ENABLED` | Allow anonymous read-only access via ssh guest@. |
| `users.registration_mode` | string | `open` | `MESHBBS_USERS_REGISTRATION_MODE` | open, approval, invite, or closed. Default 'open' with federated posting withheld: the door is open, the shared airtime is gated ([N7]). |
