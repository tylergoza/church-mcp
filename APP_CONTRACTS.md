# App contracts: the per-person read API

## 1. Purpose and status

The church MCP server is read-only. Each of its tools calls one church app's
`GET /api/v1/me/...` endpoint as the signed-in person, using a delegated
token from User Management (UM). This document is the contract those apps
implement; it is phase 3 of `../.claude/plans/mcp-server.md`.

Most of these endpoints do not exist yet. The paths, query parameters and
JSON field names below are exactly what the Go code in
`internal/tools/*.go` sends and reads, so an app that follows this document
works with the MCP server unchanged. Fields the tools do not read may be
added freely; the server ignores them. Endpoints are GET only.

| App | MCP app id | UM audience (client ID) | Tools file |
|-----|-----------|-------------------------|------------|
| Church calendar | `calendar` | `calendar` | `internal/tools/calendar.go` |
| Member directory | `directory` | `directory` | `internal/tools/directory.go` |
| Event planner | `events` | `events` | `internal/tools/events.go` |
| Maintenance tracker | `tracker` | `tracker` | `internal/tools/tracker.go` |
| Production planner | `production` | `planner` | `internal/tools/production.go` |

## 2. Common rules for every app

### Auth

Every request carries `Authorization: Bearer umd_...`, a delegated token
issued by UM for this app's audience.

The app verifies it by calling UM:

    GET {um}/api/v1/token
    Authorization: Basic base64(<app's own UM client ID>:<client secret>)
    X-Token: umd_...

UM answers 200 with

    {"user":{"sub":"...","username":"...","name":"...","email":"...","role":"...","user_admin":false},"via":"mcp"}

Any failure is 401 `{"error":"invalid_token"}`. The app may cache a good
answer for at most 60 seconds, keyed by a hash of the token (never the token
itself). Do not cache failures.

`role` is the person's role in THIS app. The app must apply exactly the same
visibility rules as its web pages do for that user: an AI tool must never
show a person more than they would see by signing in. When in doubt, show
less.

In UM, the app must list `mcp` under "Accepts tokens from". Otherwise UM
refuses to delegate (403 `not_accepted`) and the MCP server reports that the
app is not switched on for AI tools.

### Status codes

These are interpreted by `internal/apps/apps.go`:

| Status | Meaning to the MCP server |
|--------|---------------------------|
| 200 | Success; body is the JSON below. |
| 400 + JSON error | Bad parameter; the message is shown to the model. |
| 401 | Token bad or expired. The MCP server fetches a new `umd_` token once and retries. Return 401 ONLY for bad or expired tokens. |
| 403 | The person's role in the app does not allow this. |
| 404 + JSON error body | Not found, or not visible to this person. Do not reveal which. |
| 404 without a JSON error body | "The endpoint is not built yet / API is off". |
| 5xx, network error, unreadable body | App unreachable. |
| other 4xx | Shown as "the app turned that down" if a message exists. |

Error bodies: `apps.go` accepts both of these, so use either:

    {"error":"message","code":"not_found"}
    {"error":{"code":"not_found","message":"message"}}

An error needs a non-empty `error` message or `code`, or a 404 is taken as
"not built". Note the existing calendar and event planner catch-all
(`/api/` answers JSON 404 "no such endpoint") would read as "not found", not
"not built"; the `/me/` routes must be registered explicitly, and the
catch-all is fine as is.

### Rules settled by the calendar pilot (2026-11-14)

The calendar (`../calendar`, branch `mcp-me-api`) is the reference
implementation. Copy `internal/umtoken/` and `internal/server/me_auth.go`
from it; the reuse steps are in the header of `me_auth.go`.

- UM unreachable, or UM rejecting the app's own client credentials: 503
  with `Retry-After`, never 401 (401 makes the MCP server fetch a new token).
- Only tokens whose `via` is `mcp` or `website` are accepted; any other
  `via` gets 403.
- Lists stop at 100; `?limit=` is honoured up to 100.
- Answers send `Cache-Control: private, no-store`, no cookies, no CORS
  (CORS comes with the members site).
- Extra fields are fine where the shapes below can't say something
  (the calendar adds `type` on `/me/bookings` and `whole` on `/me/rooms`).
- With UM not connected, every `/me` path is a plain-text 404 ("not built").

### Formats

- Dates are `YYYY-MM-DD`; times are `HH:MM`, 24 hour. Both are church local
  time with no zone (each app uses its `TZ` env, `America/Chicago`).
- A missing optional field is omitted or an empty string. A blank time means
  all day. `ends_on` omitted or equal to `starts_on` means a single day.
- Lists are always JSON arrays, `[]` when empty, never `null`, and in an
  object under the key named below.
- Keep lists bounded: honor `limit` where given, otherwise cap at 100 items.
  The tools show only the first 40 and say how many more there were.
- Ids are integers, stable, and usable in the matching detail path.
- Date-range endpoints take `from` and `to` (both inclusive); the tools
  never ask for more than 400 days.

### Read-only and privacy

GET only; no side effects (do not mark anything read, touch last-seen, and so
on). Do not log response bodies or tokens. Send `Cache-Control: private,
no-store`.

### Health

Each app's `/healthz` returns `ok` (200, plain text). The Connections page
Test button uses it.

## 3. Apps

### 3.1 Church calendar (`calendar`)

Reuses the shape of the existing `GET /api/v1/events` (`api.go`), but as the
signed-in person rather than a read token, so visibility follows the person.

| Method | Path | Query | Tool |
|--------|------|-------|------|
| GET | `/api/v1/me/calendar` | `from`, `to` (dates, required in practice; the tool always sends them), `room` (optional room name) | `calendar_events` |
| GET | `/api/v1/me/calendar/{id}` | none | `calendar_event` |
| GET | `/api/v1/me/rooms` | none | `calendar_rooms` |
| GET | `/api/v1/me/availability` | `from`, `to`, `rooms` (optional, comma-separated room names; default all bookable rooms) | `calendar_availability` |
| GET | `/api/v1/me/bookings` | none | `calendar_my_bookings` |

`GET /api/v1/me/calendar?from=2026-11-16&to=2026-11-30`

    {"events":[
      {"id":42,"busy":false,"source":"calendar","title":"Choir practice",
       "kind":"event","status":"confirmed","description":"",
       "starts_on":"2026-11-18","start_time":"19:00",
       "ends_on":"2026-11-18","end_time":"20:30",
       "rooms":["Sanctuary"],"public":true,"url":"https://...",
       "signup":null},
      {"id":43,"busy":true,"starts_on":"2026-11-19","start_time":"09:00",
       "ends_on":"2026-11-19","end_time":"12:00","rooms":["Fellowship Hall"]}
    ]}

Fields read: `id`, `busy`, `title`, `status`, `starts_on`, `start_time`,
`ends_on`, `end_time`, `rooms`. `status` is `confirmed` (the default when
omitted), `pending` or another status the web UI shows; anything other than
`confirmed` is shown to the model in parentheses.

`GET /api/v1/me/calendar/{id}` returns ONE event object, not wrapped, with
the same fields plus the detail fields the tool reads: `kind`, `source`
(which app it came from), `description`, `public` (false = private, members
only), `url`, and `signup` (null or omitted, or
`{"label":"Fall dinner","url":"https://...","state":"open","closes_at":"2026-11-10"}`).

Visibility: events are shown as the person may see them on the calendar web
pages. Private and pending ones are included for members. An event that is
locked to this person (the web pages show only "busy") comes back with
`"busy":true`, no `title`, `description`, `kind`, `source`, `url` or `signup`,
and keeps only the time and rooms. `GET /api/v1/me/calendar/{id}` for a busy
or invisible event: 404 with a JSON error (not the busy shape).

`GET /api/v1/me/rooms`

    {"buildings":[
      {"name":"Main Building","rooms":[
        {"name":"Sanctuary","capacity":250},
        {"name":"Fellowship Hall","capacity":120}]},
      {"name":"Pavilion","rooms":[]}]}

`capacity` 0 or omitted means unknown. A building with `rooms: []` is shown
as bookable as a whole. List only what can be booked (the calendar's
bookable places).

`GET /api/v1/me/availability?from=2026-11-16&to=2026-11-16&rooms=Sanctuary,Fellowship%20Hall`

    {"busy":[
      {"starts_on":"2026-11-16","start_time":"09:00","ends_on":"2026-11-16",
       "end_time":"11:00","rooms":["Sanctuary"],"status":"confirmed"}
    ]}

Busy times only, with no event titles or details, whatever the person's
role. Include pending blocks with `status` `pending`. Anything not listed is
free. An unknown room name is a 400 JSON error.

`GET /api/v1/me/bookings`

    {"bookings":[
      {"id":42,"title":"Youth lock-in","status":"pending",
       "starts_on":"2026-12-05","start_time":"18:00",
       "ends_on":"2026-12-06","end_time":"08:00",
       "rooms":["Fellowship Hall"],"can_change":true,"taken_over":false}
    ]}

Only bookings and requests made by this person, upcoming first.
`taken_over` is true when a planner (event planner or production planner)
took the booking over. `can_change` is read by the model's future write
tools and is currently only parsed, not shown.

### 3.2 Member directory (`directory`)

Reuses the directory's existing person and household JSON where there is
one; the difference is that every field has passed through the directory's
`Visible()` check for the viewer. A field the viewer may not see is OMITTED
(not blank-but-present, and never a placeholder). The tools never guess at
omitted fields.

| Method | Path | Query | Tool |
|--------|------|-------|------|
| GET | `/api/v1/me` | none | `directory_household` (when no id is given) |
| GET | `/api/v1/me/people` | `q` (part of first, last or goes-by name), `limit` (the tool sends 41) | `directory_search` |
| GET | `/api/v1/me/people/{id}` | none | `directory_person` |
| GET | `/api/v1/me/households/{id}` | none | `directory_household` |

`GET /api/v1/me` is the caller's own entry. The tool reads only the
`households` array, whose items each need `id`. Return the caller's
households (empty array if they are not in the directory yet; still 200).
Other fields (name, id and so on) are welcome but unread.

    {"id":7,"name":"Pat Smith","households":[{"id":3,"name":"Smith","role":"head"}]}

`GET /api/v1/me/people?q=smith&limit=41`

    {"people":[
      {"id":7,"name":"Pat Smith","email":"pat@example.org","phone":"555-0100",
       "household":{"id":3,"name":"Smith"}}
    ]}

Matches names case-insensitively (first, last, goes-by). `q` is required;
a missing or empty `q` is a 400 JSON error. Results are limited to people
the viewer may see, ordered by name, at most `limit` (cap 100). Only people
that exist for the viewer appear: a person hidden entirely does not appear.

`GET /api/v1/me/people/{id}`

    {"id":7,"name":"Pat Smith","role":"head","age":44,
     "email":"pat@example.org","phone":"555-0100",
     "birthday":"March 4","anniversary":"June 12, 2005",
     "households":[{"id":3,"name":"Smith","role":"head"}],
     "family":[{"id":8,"name":"Sam Smith","relation":"spouse"}],
     "groups":[{"id":2,"name":"Choir"}]}

Fields read: `name`, `email`, `phone`, `birthday`, `anniversary` (text such
as `March 4` or `March 4, 1980`, exactly as the directory displays it, so
whether the year shows follows the person's own sharing choice),
`households[]` (`id`, `name`, `role` = `head`, `adult` or `child`),
`family[]` (`id`, `name`, `relation`), `groups[]` (`id`, `name`). Also
parsed: `id`, `role`, `age`, `household` (single, `{id,name}`, used in search
results). Omit anything not Visible() to the viewer; omit `age` for people
whose age is not shared (0 is treated as unknown). A person the viewer
cannot see: 404 with JSON error.

`GET /api/v1/me/households/{id}`

    {"id":3,"name":"Smith",
     "addresses":[{"id":1,"label":"Home","lines":["12 Main St","Springfield, PA 19000"]}],
     "members":[
       {"id":7,"name":"Pat Smith","role":"head","age":44,
        "email":"pat@example.org","phone":"555-0100","address_id":1}]}

Members use the person shape above (fields the tool reads: `id`, `name`,
`role`, `age`, `email`, `phone`; `address_id` is parsed, not shown).
Addresses and member fields are omitted when not Visible(). A household the
viewer may not see: 404 with JSON error.

### 3.3 Event planner (`events`)

Events reuse the planner's existing `GET /api/v1/events` shape, including
its `busy` entries, but filtered to what the person's role may see.

| Method | Path | Query | Tool |
|--------|------|-------|------|
| GET | `/api/v1/me/events` | `from`, `to` (dates, always sent) | `events_list` |
| GET | `/api/v1/me/signups/open` | none | `events_open_signups` |
| GET | `/api/v1/me/signups` | none | `events_my_signups` |

`GET /api/v1/me/events?from=2026-11-16&to=2026-12-16`

    {"events":[
      {"id":12,"busy":false,"title":"Christmas dinner","kind":"dinner",
       "status":"Planning","starts_on":"2026-12-13","start_time":"17:00",
       "ends_on":"2026-12-13","end_time":"20:00","tentative":true,
       "rooms":["Fellowship Hall"],"description":"","url":"https://..."},
      {"id":13,"busy":true,"starts_on":"2026-12-14","start_time":"",
       "ends_on":"2026-12-14","end_time":"","rooms":[]}
    ]}

Fields read: `id`, `busy`, `title`, `status` (any case; the tool lowercases
it), `starts_on`, `start_time`, `ends_on`, `end_time`, `tentative` (date
pencilled in), `rooms`. `kind`, `description`, `url` are parsed only. As in
the calendar, a `busy` entry has no title or details. Include whatever the
person's role sees in the planner's web pages (planners see drafts; others
may see only published events or busy blocks).

`GET /api/v1/me/signups/open`

    {"events":[
      {"event_id":12,"title":"Christmas dinner","starts_on":"2026-12-13",
       "start_time":"17:00","rooms":["Fellowship Hall"],
       "public_description":"Bring a dish.",
       "form":{"code":"xmas","label":"Dinner RSVP","state":"open",
               "closes_at":"2026-12-06","url":"https://..."},
       "signed_up":1}
    ]}

Events with a sign-up form the person can use now, soonest first. `state` is
`open` or `full`. `closes_at` is a date (the tool formats it as a date;
send `YYYY-MM-DD`, not a timestamp). `signed_up` is the number of active
sign-ups this person has on it (0 or omitted = none).

`GET /api/v1/me/signups`

    {"signups":[
      {"id":5,"event_id":12,"event_title":"Christmas dinner",
       "starts_on":"2026-12-13","start_time":"17:00","form_label":"Dinner RSVP",
       "status":"in","people":["Pat Smith","Sam Smith"],"guests":2,
       "can_change":true}
    ]}

Only this person's own sign-ups, upcoming first. `status` is `in` or
`withdrawn` (withdrawn ones are marked as such). `people` are names;
`guests` is the number of extra unnamed guests. `can_change` and `id`,
`event_id` are parsed, not shown.

### 3.4 Maintenance tracker (`tracker`)

Reuses the tracker's existing `/api/v1` shapes (places, products, items,
supplies) under `/api/v1/me`, adding a `q` search to each. Each endpoint is
queried separately by `tracker_find`.

| Method | Path | Query | Tool |
|--------|------|-------|------|
| GET | `/api/v1/me/places` | `q` (optional text search of name/path) | `tracker_find` |
| GET | `/api/v1/me/products` | `q` | `tracker_find` |
| GET | `/api/v1/me/items` | `q` | `tracker_find` |
| GET | `/api/v1/me/supplies` | `q`, `low=1` (only supplies low or out); neither = every supply | `tracker_find` |
| GET | `/api/v1/me/tasks` | `q`, `status` (`overdue`, `due-soon`, `upcoming`, `unscheduled`), `place` (text of the place path), `limit` | `tracker_maintenance` |
| GET | `/api/v1/me/history` | `q`, `item` (item id), `since` (YYYY-MM-DD, inclusive), `limit` | `tracker_history` |
| GET | `/api/v1/me/problems` | `q`, `status` (`active` = open or in progress, the default; `open`, `in_progress`, `resolved`, `all`), `place` (text of the place path), `limit` | `tracker_problems` |

`q` is omitted when the model asks for just low supplies, or for every
supply (`tracker_find` with `kind: supplies` and no q: the stock overview). For `low=1` the
tool calls only `/supplies`. Search is case-insensitive over names (and for
items the manufacturer and model, for places the path). Cap each list at 100;
the tool shows 40.

    GET /api/v1/me/places?q=hall
    {"places":[{"id":4,"kind":"room","name":"Fellowship Hall",
                "path":"Main Building / Fellowship Hall","bookable":true,
                "address":"","tags":["events"]}]}

    GET /api/v1/me/products?q=projector
    {"products":[{"id":9,"name":"Epson projector","category":"AV",
                  "total":3,"place_count":2}]}

    GET /api/v1/me/items?q=projector
    {"items":[{"id":31,"name":"Epson projector","category":"AV","quantity":1,
               "place":{"id":4,"path":"Main Building / Fellowship Hall"},
               "manufacturer":"Epson","model":"EB-992F","notes":"",
               "supply":"","supply_per":1,"units":[]}]}

    GET /api/v1/me/supplies?low=1
    {"supplies":[{"id":2,"name":"Paper towels","unit":"rolls",
                  "place":{"id":6,"path":"Main Building / Kitchen"},
                  "on_hand":2,"total":10,"stock":"low"}]}

    GET /api/v1/me/tasks?q=filter&status=overdue
    {"tasks":[{"id":1,"item":"Furnace",
               "place":{"id":3,"path":"Main Building / Mechanical Room"},
               "name":"Change air filter","interval":"Every 3 months",
               "last_completed_on":"2026-04-12","next_due_on":"2026-07-12",
               "status":"overdue","supply":"Furnace filters",
               "supply_amount":1,"supply_short":false,
               "description":"","quantity":1,"item_notes":"","units":[]}]}

    A group item's task (four air returns in one room):
               …"item":"Air Return","name":"Check Filters","supply":"14x30 Filter",
               "supply_amount":4,"quantity":4,
               "units":[{"number":1,"id":"1","label":"Northwest (away from sanctuary)",
                         "last_replaced":"2026-11-07"}, … #2–#4]

    GET /api/v1/me/history?q=filter&since=2026-01-01&limit=40
    {"history":[{"id":9,"performed_on":"2026-04-12","item":"Furnace",
                 "place":{"id":3,"path":"Main Building / Mechanical Room"},
                 "task":"Change air filter","kind":"replaced",
                 "replaced":"2 × Furnace filters",
                 "units":"#1 – Northwest; #3 – Northeast","supply_used":"",
                 "performed_by":"Eddie",
                 "notes":"20x25x1"}]}

    GET /api/v1/me/problems?q=tv&limit=40
    {"problems":[{"id":4,"title":"TV won't turn on","details":"No power light.",
                  "status":"in_progress","item":"Hall TV","unit":"#2 – left of the stage",
                  "place":{"id":3,"path":"Main Building / Fellowship Hall"},
                  "assigned_to":"Eddie","reported_by":"Ann",
                  "created_at":"2026-11-08T15:04:00Z","updated_at":"2026-11-09T16:00:00Z",
                  "resolved_at":"",
                  "updates":[{"at":"2026-11-09T16:00:00Z","by":"Eddie",
                              "status":"in_progress","assigned_to":"",
                              "note":"Ordered a new power board."}]}]}

`problems` are problem reports, newest first; `q` matches the title,
details, item name and place path; times are RFC 3339 UTC (the tool shows
them in the church's zone); `item`/`unit`/`assigned_to`/`resolved_at` are
`""` when not set; `updates` are oldest first, at most the last 10, with
`status`/`assigned_to` set only when that update changed them. The
reporter's contact details are never sent. Every signed-in person sees
problems on the web (`auth` routes), so every token gets the same list.

`tasks` are active tasks only, most urgent first (overdue, due-soon,
upcoming, unscheduled; soonest due first within each); `q` matches the item
(product) name, task name or supply name; `interval` is a label ("Every 3
months", "One time"); `last_completed_on` and `next_due_on` are YYYY-MM-DD or
`""`; `supply` is `""` when the task uses none; `supply_amount` is how many
one completion uses (0 when none); `supply_short` is true when
too little is on hand to do it once; `description` is the task's
instructions; `quantity`, `item_notes` and `units` are the item's (`units`
lists a group item's numbered units with their location labels and when each
one's supply was last replaced; `[]` for a single item). Items carry the same
`notes`, `supply`, `supply_per` and `units`. `history` is newest first; `q` matches
item name, task name, the item's supply and any supply taken; `kind` is
`work`, `replaced` or `moved`; `units` names which units of a group item
the entry was about (`""` when not recorded); `supply_used` is stock taken,
e.g. `"4 × 14x30 Filter"`; `replaced` is `""` or e.g. `"2 × Furnace
filters"`, on any kind (filters swapped during a `work` task count); `performed_by` falls back to whoever logged it. A bad `status`,
`since` or `item` is 400 `{"error":…}`. No cost or serial numbers are sent.
Item notes are (every signed-in person sees them on the item page). Visibility: every signed-in person (editor or
admin) sees maintenance and history on the web pages (`auth` routes in the
tracker's `routes.go`, not admin-only), so every token the gate lets in gets
the same lists.

Fields read: tasks `item`, `name`, `interval`, `last_completed_on`,
`next_due_on`, `status`, `supply`, `supply_short`, `place.path` (`id` parsed
only); history `performed_on`, `item`, `task`, `kind`, `replaced`,
`performed_by`, `notes`, `place.path`. Places `id`, `name`, `path`, `kind`, `bookable`, `address`
(`tags` parsed only); products `id`, `name`, `category`, `total`,
`place_count`; items `id`, `name`, `quantity`, `manufacturer`, `model`,
`place.path` (`category`, `place.id` parsed only); supplies `id`, `name`,
`unit`, `on_hand`, `place.path`, `stock` (`ok`, `low` or `out`; `total` is
parsed only). Include `place.path` always; the tool prints it unconditionally.
Show only what the person's tracker role may see.

### 3.5 Production planner (`production`, UM audience `planner`)

The production planner has no JSON API yet; everything here is new. Note the
MCP app id is `production`, but the UM client ID (the token audience) is
`planner`, so the app verifies against UM with its `planner` credentials.

| Method | Path | Query | Tool |
|--------|------|-------|------|
| GET | `/api/v1/me/productions` | `status` (optional: `planning`, `rehearsing`, `ready`, `done`, `cancelled`) | `production_list` |
| GET | `/api/v1/me/productions/{id}` | none | `production_detail` |

`GET /api/v1/me/productions?status=rehearsing`

    {"productions":[
      {"id":3,"title":"Christmas Pageant","kind":"play","status":"Rehearsing",
       "next_date":{"kind":"rehearsal","starts_on":"2026-11-18",
                    "start_time":"18:30","end_time":"20:00",
                    "tentative":false,"rooms":["Sanctuary"],"notes":""}}
    ]}

Only productions the person's role may see. `next_date` is the next
upcoming schedule item, or null or omitted when there is none.

`GET /api/v1/me/productions/{id}`

    {"id":3,"title":"Christmas Pageant","kind":"play","status":"Rehearsing",
     "where":"Sanctuary","director":"Jo Lee","notes":"Costumes by Dec 1.",
     "next_date":{...same as a schedule item...},
     "schedule":[
       {"kind":"rehearsal","starts_on":"2026-11-18","start_time":"18:30",
        "end_time":"20:00","tentative":false,"rooms":["Sanctuary"],"notes":""},
       {"kind":"performance","starts_on":"2026-12-20","start_time":"17:00",
        "end_time":"18:00","tentative":true,"rooms":["Sanctuary"],
        "notes":"Pencilled in"}],
     "cast":[{"name":"Pat Smith","part":"Narrator"}]}

Schedule item fields read: `kind` (`rehearsal`, `tech`, `dress`,
`performance` and so on), `starts_on`, `start_time`, `end_time`,
`tentative`, `rooms`, `notes`. A schedule item is one day; there is no
`ends_on`. `status` is any case (the tool lowercases it in the list). The
schedule is in date order, bounded at a sensible size (the tool shows 40).
`cast` is included ONLY if the person may see it in the web pages; omit the
key (or send `[]`) otherwise. Not visible or unknown: 404 with a JSON error.

## 4. Checklist for an app

1. In UM, add `mcp` under the app's "Accepts tokens from".
2. Verify bearer tokens with UM `GET /api/v1/token` (HTTP Basic with the
   app's own client ID and secret, `X-Token` header); cache a good answer
   for at most 60 seconds keyed by a token hash; 401 `invalid_token` on any
   failure. Map UM's `user.role` to the app's own role and visibility.
3. Implement the endpoints for the app above, GET only, with the exact paths,
   query names and JSON field names; omit fields the person may not see;
   `[]` for empty lists; JSON error bodies for 400, 403 and 404.
4. Register the `/api/v1/me/` routes explicitly; answer 404 without a JSON
   body only when the API is off.
5. `/healthz` answers `ok`.
6. Tests: a bad or missing token gives 401; a role that may not see a thing
   cannot see it through the API (compare against the web page rules);
   busy-only and omitted-field cases; empty results are `[]`; the response
   decodes into the matching Go structs in `internal/tools`.
