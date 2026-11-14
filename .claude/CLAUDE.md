# Church MCP server: project notes

Read-only MCP server ("AI Connector") so AI tools use the church's apps as
the signed-in person. Sibling of `../calendar`, `../member_directory`,
`../event_planning`, `../maintenance_tracking`, `../production_planning`
and `../user_management`. Same stack and conventions: Go + SQLite
(modernc, no CGO) + server-rendered templates + vendored Stimulus with
the no-build autoloader. No Node, no bundler, no new dependencies, no MCP
SDK (README "Why no MCP SDK").

- The plan is `../.claude/plans/mcp-server.md` (phase 3 is this app).
  The apps' side is `APP_CONTRACTS.md`: keep it and `internal/tools`
  in step.
- Everything is per person: an AI can only see what the person can see.
  Tools call `<app>/api/v1/me/…` with a `umd_…` token from User
  Management delegation (`internal/um/contract.go` is the only file that
  knows UM's shapes). Never add app-wide tokens.
- Read-only until change history and rollback exist (plan phase 5).
  Every tool has `readOnlyHint: true`; `internal/tools` tests check it.
- The audit log records who, client, tool, when, OK. Never arguments or
  results; never log tool output or tokens.
- MCP: 2026-07-28 (stateless, headers must match the body) plus the
  2025-11-25/06-18/03-26 `initialize` handshake on the same `POST /mcp`.
  JSON responses only. Tool failures are `isError` results.
- OAuth: PKCE S256 required, codes single-use, refresh tokens rotate and
  reuse revokes the connection, everything stored hashed. Redirect hosts
  are allowlisted (admin page); loopback matches any port. Client ID
  Metadata Documents are fetched with the SSRF-guarded client in
  `cimd.go`; keep it that way.
- Connections die with the UM grant (re-checked every 5 min). Sign-out
  here is local only, so it doesn't end AI connections.
- Sign-in is only through UM (no local passwords). SSO client ID `mcp`
  (Keychain `um-mcp`). First setup: `church-mcp connection set-sso`.
- Cookies `mcp_*`; tokens `mcpa_` (access), `mcpr_` (refresh), clients
  `mcpc_`/`mcps_`; local port 8150 (tracker 8080, planner 8090, UM 8100,
  events 8110, calendar 8120, directory 8130, 8140 taken by member files
  and the website).
- Secrets never go in files or `export` lines in docs; the user keeps them
  in the macOS Keychain and runs make through `~/.zshrc` helpers.
- CSP is `style-src 'self'`: no inline `style=` attributes in templates.
- No deploy yet; phase 4 adds `deploy/ansible` like the other apps.
- The user runs all `git commit`s themselves (YubiKey signing). Don't commit.
