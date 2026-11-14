# Church MCP server (AI Connector)

Lets AI tools (Claude, ChatGPT, Claude Code, Cursor, anything that speaks
MCP) read the church's apps **as the signed-in person**: the calendar,
member directory, event planner, maintenance tracker and production
planner. Each person sees only what they could see in each app, and
nothing can be changed. Phase 3 of `../.claude/plans/mcp-server.md`.

```
AI tool ──MCP + OAuth 2.1──► church-mcp ──sign-in──► User Management (client ID mcp)
                                 │  per tool call: POST /api/v1/delegate → umd_… (15 min, cached)
                                 └──► <app>/api/v1/me/…   Authorization: Bearer umd_…
```

- Go + SQLite (modernc, no CGO), server-rendered pages, vendored Stimulus
  with the no-build autoloader, installable as a PWA. One dependency:
  `modernc.org/sqlite`.
- No passwords here: everyone signs in through User Management.
- The apps' side of the deal (endpoints and JSON) is in
  [`APP_CONTRACTS.md`](APP_CONTRACTS.md).

## Quick start

```sh
make dev            # http://127.0.0.1:8150, templates and static from disk
make test           # go vet + go test (includes the end-to-end OAuth + MCP test)
make build          # bin/church-mcp
```

Port **8150** (tracker 8080, production planner 8090, User Management
8100, event planner 8110, calendar 8120, directory 8130; 8140 is claimed
by the planned member files app and the church website).

First setup, with no admin page reachable yet (there is no local login):

```sh
security find-generic-password -w -s um-mcp | \
  ./bin/church-mcp connection set-sso http://localhost:8100 mcp http://localhost:8150
```

The client secret is read from standard input. After that, sign in as a
User Management admin who has the `admin` role in this app and manage the
rest on **Connections**.

## Connecting an AI tool

The MCP address is `<base URL>/mcp`.

- Claude Code: `claude mcp add --transport http church http://localhost:8150/mcp`,
  then `/mcp` in Claude Code to sign in.
- claude.ai / ChatGPT: add a custom connector with the address (needs the
  deployed https address; phase 4).

The AI tool discovers everything else, sends the person to sign in through
User Management and then to a consent page that names the tool and where
it sends them back to. **Connected AI tools** lists and ends connections.

## What's in it

| Path | What |
|------|------|
| `/.well-known/oauth-protected-resource[/mcp]` | RFC 9728 resource metadata |
| `/.well-known/oauth-authorization-server` | RFC 8414 server metadata |
| `POST /register` | RFC 7591 dynamic client registration (rate limited) |
| `GET/POST /authorize` | sign-in, consent, code (PKCE S256 required, `iss` returned) |
| `POST /token` | code and refresh grants |
| `POST /revoke` | RFC 7009 revocation |
| `POST /mcp` | the MCP endpoint; `GET`/`DELETE` answer 405 |
| `/ai-tools` | each person's connections and recent tool calls |
| `/admin/connections`, `/admin/redirect-hosts`, `/admin/activity` | admins |

### OAuth

- Clients register with **Client ID Metadata Documents** (an https
  `client_id` URL, preferred by the MCP spec since 2025-11-25) or with
  dynamic registration. Metadata documents are fetched over https only,
  never from private or loopback addresses, with no redirects, 64 KB max.
- Every redirect URI must be https on an **allowlisted host** (admin page
  Redirect hosts; seeded with claude.ai, claude.com, chatgpt.com,
  localhost and 127.0.0.1), or http on loopback. Loopback hosts match any
  port. Anything else is refused at registration and at authorize.
- Scope `mcp:read`. Codes last 60 s and are single-use (reuse revokes the
  connection). Access tokens `mcpa_…` last an hour; refresh tokens
  `mcpr_…` last 30 days and rotate on every use, and reusing an old one
  revokes the whole connection. Only SHA-256 hashes are stored.
- A connection keeps the person's User Management grant (sealed with
  `secrets.key`, next to the database). It is re-checked with User
  Management at most every 5 minutes; when the grant has ended, or the
  person lost their role here, the connection is revoked. A 401 from
  `/mcp` carries `WWW-Authenticate: Bearer resource_metadata="…"`.
- Signing out of this site is local only: signing out of User Management
  would end the grant that every AI connection made from it depends on.
  (Long-lived grants for delegation are a User Management TODO.)

### MCP

- Targets the **2026-07-28** spec (stateless requests: protocol version
  in `_meta` plus the `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name`
  headers, `server/discover`, no sessions) and also speaks the
  **2025-11-25, 2025-06-18 and 2025-03-26** `initialize` handshake on the
  same endpoint, since most clients still use it.
- JSON responses only (no SSE): plain tools don't need streaming.
  Notifications and client responses get 202. Methods: `initialize`,
  `server/discover`, `ping`, `tools/list`, `tools/call`.
- `tools/list` shows only tools for apps the person has a role in
  (User Management `me/apps`). Every tool is `readOnlyHint: true`. A
  tool failure is a result with `isError: true` and a short sentence the
  AI can pass on, never a stack trace.
- Results are short text (40 lines at most), dates and times in the
  church's time zone (`TZ`, default America/New_York).
- The audit log (`tool_calls`, Activity page) records who, which AI
  tool, which tool, when, OK or not and how long. Never arguments or
  results.

#### Why no MCP SDK

The official Go SDK was considered and not used: the server needs one
POST endpoint, five methods and JSON responses, about 400 lines. Writing
it by hand keeps the one-dependency rule, makes it easy to serve the new
stateless 2026-07-28 protocol and the older `initialize` one side by side
(SDKs lag spec releases), and keeps authorization in our own code, where
the OAuth server already is. `internal/mcp` has the protocol and its
tests; revisit if we need SSE, resources or prompts.

## Settings (Connections page)

Environment variables win over what's saved on the page.

| Connection | Variables |
|------------|-----------|
| Single sign-on (required) | `SSO_URL`, `SSO_INTERNAL_URL`, `SSO_CLIENT_ID`, `SSO_CLIENT_SECRET`, `BASE_URL` |
| Calendar | `CALENDAR_URL`, `CALENDAR_AUDIENCE` |
| Member directory | `DIRECTORY_URL`, `DIRECTORY_AUDIENCE` |
| Event planner | `EVENTS_URL`, `EVENTS_AUDIENCE` |
| Maintenance tracker | `TRACKER_URL`, `TRACKER_AUDIENCE` |
| Production planner | `PRODUCTION_URL`, `PRODUCTION_AUDIENCE` |

An app's address is optional: without one, the address User Management
lists for the app is used. The audience is the app's client ID in User
Management (calendar, directory, events, tracker, planner).

Saving single sign-on checks the client ID and secret with User
Management first (there's no pending/test-sign-in flow like the other
apps have, because there's no local login to fall back to).

Flags: `-addr` (`ADDR`, default `:8150`), `-db` (`DB_PATH`, default
`data/mcp.db`), `-dev` (`DEV=1`), `-trust-proxy` (`TRUST_PROXY=1`). `TZ`
sets the church time zone.

## Registering with User Management

In User Management: add the app with client ID `mcp`, redirect URI
`<base URL>/auth/callback`, give it "May act for users in" each app, and
in each app tick "Accepts tokens from" `mcp`. Give people a role in
`mcp` (`admin` for whoever manages it, `user` for everyone else).

## Commands

```sh
church-mcp connection set-sso URL CLIENT_ID [BASE_URL]   # secret on stdin
church-mcp connection clear ID                           # sso, calendar, directory, events, tracker, production
church-mcp backup FILE
```

Flags go before the command.

## Deploying

Not yet: phase 4 adds `deploy/ansible` like the other apps
(`mcp.<dns_zone>` on the apps box behind the edge proxy). State to carry:
the database and `secrets.key`.

## Layout

```
main.go                     flags, commands, server start
internal/server             HTTP: OAuth server, /mcp, sign-in, pages
internal/mcp                MCP JSON-RPC over Streamable HTTP (hand-written)
internal/tools              the read-only tools, one file per app
internal/um                 User Management delegation client (the one file with UM's shapes: contract.go) + cache
internal/apps               calls to the apps' /api/v1/me/…
internal/sso                sign-in through User Management
internal/store              SQLite: users, sessions, clients, connections, codes, tokens, audit
internal/connections        environment-or-settings connection values
internal/secrets            AES-GCM sealing for secrets in the database
web/                        templates, CSS, Stimulus controllers, icons
APP_CONTRACTS.md            what each app's /api/v1/me/… must return
```
