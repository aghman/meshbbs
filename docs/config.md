# Configuration reference

Generated from the source. Do not edit by hand.

| Key | Type | Default | Environment variable | Description |
|---|---|---|---|---|
| `log.file` | string | *(empty)* | `MESHBBS_LOG_FILE` | Path to a log file. Empty logs to stderr. |
| `log.format` | string | `text` | `MESHBBS_LOG_FORMAT` | text or json. |
| `log.level` | string | `info` | `MESHBBS_LOG_LEVEL` | debug, info, warn, or error. |
| `mesh.airtime_ceiling_pct` | float64 | `5` | `MESHBBS_MESH_AIRTIME_CEILING_PCT` | Share of the channel the WHOLE BBS network should use, as a percentage. Divided by expected_instance_count to get this node's allowance. Clamped to 15 in code (§7.6). |
| `mesh.channel_name` | string | `bbsnet` | `MESHBBS_MESH_CHANNEL_NAME` | Name of the Meshtastic channel carrying BBS traffic (§7.1). Create it in the Meshtastic app as a secondary channel with the same name and key on every instance. |
| `mesh.door_event_batch_window` | string | `auto` | `MESHBBS_MESH_DOOR_EVENT_BATCH_WINDOW` | How long a partial batch of door-league events waits before it is sent (§9.5). 'auto' derives it from what the area's airtime share actually buys, so a measured flood multiplier propagates without a config edit. An explicit Go duration like '30m' overrides, and is clamped to 5m-12h. |
| `mesh.door_event_max_age` | string | `24h` | `MESHBBS_MESH_DOOR_EVENT_MAX_AGE` | How long a queued door-league event may wait before it is dropped unsent (§9.5). Generous on purpose: it exists to stop a node that has been offline for a week spending its whole budget on a game that finished, not to tighten latency. |
| `mesh.enabled` | bool | `false` | `MESHBBS_MESH_ENABLED` | Federate over a Meshtastic radio. Off by default: enabling it transmits on a shared band. |
| `mesh.expected_instance_count` | int | `50` | `MESHBBS_MESH_EXPECTED_INSTANCE_COUNT` | How many instances divide the ceiling. The design plans for 50 ([D2]). |
| `mesh.flood_multiplier` | float64 | `4` | `MESHBBS_MESH_FLOOD_MULTIPLIER` | R: how many times the mesh rebroadcasts each packet. Every airtime figure scales linearly with it, and 4 is a GUESS — run 'meshbbs mesh survey' to measure yours (§7.8). |
| `mesh.flood_multiplier_override` | bool | `false` | `MESHBBS_MESH_FLOOD_MULTIPLIER_OVERRIDE` | Pin flood_multiplier and disable live refinement. Testing only: it stops the node correcting a value that is too low. |
| `mesh.ham_mode_override` | string | *(empty)* | `MESHBBS_MESH_HAM_MODE_OVERRIDE` | Set to 'i_accept_part97_responsibility' to transmit encrypted traffic while the radio reports a licensed operator. FCC Part 97 prohibits obscuring the meaning of amateur transmissions; the licence at risk is yours (§8.3). |
| `mesh.hop_limit` | int | `0` | `MESHBBS_MESH_HOP_LIMIT` | Hop limit for BBS packets, 0-7. Zero uses the radio's own setting. Hop limit multiplies what every packet costs the mesh (§1.1), so set it as low as your topology allows. |
| `mesh.mode` | string | `auto` | `MESHBBS_MESH_MODE` | How to reach the radio: 'serial', 'tcp', or 'auto' (try the configured serial device or auto-detect, then fall back to tcp_host). |
| `mesh.quiet_hours` | string | *(empty)* | `MESHBBS_MESH_QUIET_HOURS` | Comma-separated local-time windows of zero transmission, e.g. '22:00-06:00'. Windows may wrap midnight. |
| `mesh.rx_timeout_secs` | int | `300` | `MESHBBS_MESH_RX_TIMEOUT_SECS` | Reconnect if the radio has sent nothing for this many seconds. A USB serial handle can go one-way — writes keep succeeding and transmissions go out while nothing is ever received — and only silence reveals it. Zero disables the check. Below about 60 a busy-but-quiet radio may be reconnected needlessly, and each reconnect re-announces. |
| `mesh.serial_baud` | int | `115200` | `MESHBBS_MESH_SERIAL_BAUD` | Serial baud rate. Every current firmware uses 115200. |
| `mesh.serial_device` | string | *(empty)* | `MESHBBS_MESH_SERIAL_DEVICE` | Serial port, e.g. /dev/ttyUSB0 or COM3. Empty auto-detects; run 'meshbbs mesh ports' to see candidates. |
| `mesh.tcp_host` | string | *(empty)* | `MESHBBS_MESH_TCP_HOST` | Host of a node on WiFi. Port defaults to 4403 if not given. |
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
| `telnet.max_sessions` | int | `16` | `MESHBBS_TELNET_MAX_SESSIONS` | Maximum concurrent telnet sessions. This is a public plaintext port, so it is capped. |
| `telnet.port` | int | `2323` | `MESHBBS_TELNET_PORT` | Port to listen on. 23 is conventional but needs root. |
| `theme.default` | string | `classic` | `MESHBBS_THEME_DEFAULT` | Built-in or file theme name, applied to every session on this instance. 'meshbbs serve' prints the available names at startup; there is no per-user theme picker in the interface. |
| `theme.default_encoding` | string | `auto` | `MESHBBS_THEME_DEFAULT_ENCODING` | auto, utf8, or cp437. 'auto' guesses from the client's locale and terminal type. |
| `theme.dir` | string | `themes` | `MESHBBS_THEME_DIR` | Directory scanned for *.toml style overrides, relative to data_dir unless absolute ([N5]). |
| `users.default_directory_listed` | bool | `true` | `MESHBBS_USERS_DEFAULT_DIRECTORY_LISTED` | Whether new users are listed in the network directory ([N9]). |
| `users.guest_enabled` | bool | `true` | `MESHBBS_USERS_GUEST_ENABLED` | Allow anonymous read-only access via ssh guest@. |
| `users.registration_mode` | string | `open` | `MESHBBS_USERS_REGISTRATION_MODE` | open, approval, invite, or closed. Default 'open' with federated posting withheld: the door is open, the shared airtime is gated ([N7]). |
| `users.session_time_limit_mins` | int | `0` | `MESHBBS_USERS_SESSION_TIME_LIMIT_MINS` | End a session after this many minutes. 0 means no limit. Sysops are never timed out: the limit shares lines between callers, and the operator is not competing for one. |
| `web.auth_attempts_per_hour` | int | `60` | `MESHBBS_WEB_AUTH_ATTEMPTS_PER_HOUR` | Sign-in attempts allowed per client per hour. Looser than enrolment: a passkey prompt that the user dismisses costs an attempt, and that is a normal thing to do more than once. |
| `web.bind` | string | `0.0.0.0` | `MESHBBS_WEB_BIND` | Address to listen on. |
| `web.enabled` | bool | `false` | `MESHBBS_WEB_ENABLED` | Serve the browser front end. Off by default: it needs a public origin and a TLS certificate, which have no sensible defaults. |
| `web.enrol_attempts_per_hour` | int | `10` | `MESHBBS_WEB_ENROL_ATTEMPTS_PER_HOUR` | Passkey-enrolment code attempts allowed per client per hour. A person typing a code off their SSH session needs two or three; a script guessing codes needs far more. |
| `web.enrolment_code_ttl_mins` | int | `10` | `MESHBBS_WEB_ENROLMENT_CODE_TTL_MINS` | How long a passkey-enrolment code stays valid ([D18]). It is read off a terminal and typed into a browser on the same desk, so minutes are generous. |
| `web.idle_timeout_mins` | int | `30` | `MESHBBS_WEB_IDLE_TIMEOUT_MINS` | Disconnect a browser session idle this long. |
| `web.max_sessions` | int | `64` | `MESHBBS_WEB_MAX_SESSIONS` | Maximum concurrent browser sessions. A public listener with no cap is a file-descriptor exhaustion away from taking SSH down with it. |
| `web.max_sessions_per_user` | int | `8` | `MESHBBS_WEB_MAX_SESSIONS_PER_USER` | Maximum concurrent browser sessions for one account. |
| `web.origin` | string | *(empty)* | `MESHBBS_WEB_ORIGIN` | Public origin browsers reach this BBS at, e.g. https://bbs.example.com. REQUIRED when enabled and has no default: passkeys are bound to it, and a mismatch fails every sign-in with an error that does not say why (webui.md §7.1). |
| `web.port` | int | `8443` | `MESHBBS_WEB_PORT` | Port to listen on. 443 is conventional but needs root. |
| `web.session_ttl_hours` | int | `12` | `MESHBBS_WEB_SESSION_TTL_HOURS` | Absolute lifetime of a browser session, however active it is. |
| `web.tls_cert` | string | *(empty)* | `MESHBBS_WEB_TLS_CERT` | Path to the TLS certificate chain. Required when enabled unless bound to loopback, which browsers treat as a secure context. |
| `web.tls_key` | string | *(empty)* | `MESHBBS_WEB_TLS_KEY` | Path to the TLS private key. |
| `web.trusted_proxies` | list of string | *(empty)* | `MESHBBS_WEB_TRUSTED_PROXIES` | IPs or CIDRs of reverse proxies allowed to report the real client via X-Forwarded-For. EMPTY BY DEFAULT, which means the header is ignored and every request is attributed to whatever address it arrived from — correct when nothing sits in front, and it collapses per-client rate limits into one shared allowance when something does. Set this only for proxies you run: any address listed here can claim to be any client. |
| `web.unlocked_idle_timeout_mins` | int | `10` | `MESHBBS_WEB_UNLOCKED_IDLE_TIMEOUT_MINS` | Disconnect a session that has unlocked mail after this much idleness. Shorter than idle_timeout_mins on purpose: such a session holds the passphrase in memory, and a closing browser tab is a far less reliable goodbye than an SSH disconnect (webui.md §9). |
