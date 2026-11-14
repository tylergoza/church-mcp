package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// Event planner shapes (APP_CONTRACTS.md, Event planner). Events are the
// planner's existing /api/v1/events shape.

type epEvent struct {
	ID          int64    `json:"id"`
	Busy        bool     `json:"busy"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	StartsOn    string   `json:"starts_on"`
	StartTime   string   `json:"start_time"`
	EndsOn      string   `json:"ends_on"`
	EndTime     string   `json:"end_time"`
	Tentative   bool     `json:"tentative"`
	Rooms       []string `json:"rooms"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
}

type epOpen struct {
	EventID     int64    `json:"event_id"`
	Title       string   `json:"title"`
	StartsOn    string   `json:"starts_on"`
	StartTime   string   `json:"start_time"`
	Rooms       []string `json:"rooms"`
	Description string   `json:"public_description"`
	Form        struct {
		Code     string `json:"code"`
		Label    string `json:"label"`
		State    string `json:"state"` // open | full
		ClosesAt string `json:"closes_at"`
		URL      string `json:"url"`
	} `json:"form"`
	SignedUp int64 `json:"signed_up"`
}

type epSignup struct {
	ID         int64    `json:"id"`
	EventID    int64    `json:"event_id"`
	EventTitle string   `json:"event_title"`
	StartsOn   string   `json:"starts_on"`
	StartTime  string   `json:"start_time"`
	FormLabel  string   `json:"form_label"`
	Status     string   `json:"status"` // in | withdrawn
	People     []string `json:"people"`
	Guests     int      `json:"guests"`
	CanChange  bool     `json:"can_change"`
}

func eventsTools() []Def {
	return []Def{
		{App: "events", Tool: tool("events_list", "Event planner: events",
			"Events in the event planner between two dates, as your role there lets you see them (status, pencilled-in dates, rooms). Defaults to the next 30 days.",
			map[string]any{
				"from": str("First day, YYYY-MM-DD. Default today."),
				"to":   str("Last day, YYYY-MM-DD. Default 30 days after from."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ From, To string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				from, to, err := rangeArgs(env, a.From, a.To, 30)
				if err != nil {
					return mcp.Result{}, err
				}
				var out struct {
					Events []epEvent `json:"events"`
				}
				if err := env.Get(ctx, "/api/v1/me/events", url.Values{"from": {from}, "to": {to}}, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, e := range out.Events {
					when := fmtWhen(e.StartsOn, e.StartTime, e.EndsOn, e.EndTime)
					if e.Busy {
						ls = append(ls, join(" ", when, "busy (no details)", rooms(e.Rooms)))
						continue
					}
					pencil := ""
					if e.Tentative {
						pencil = "(date pencilled in)"
					}
					ls = append(ls, join(" ", fmt.Sprintf("#%d", e.ID), when+":", e.Title, statusNote(strings.ToLower(e.Status)), pencil, rooms(e.Rooms)))
				}
				return lines(fmt.Sprintf("Planned events %s to %s:", fmtDate(from), fmtDate(to)), ls,
					"No planned events from "+fmtDate(from)+" to "+fmtDate(to)+"."), nil
			}},
		{App: "events", Tool: tool("events_open_signups", "Event planner: open sign-ups",
			"Events you can sign up for now (open or full), soonest first, and whether you already did.", nil),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var out struct {
					Events []epOpen `json:"events"`
				}
				if err := env.Get(ctx, "/api/v1/me/signups/open", nil, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, e := range out.Events {
					state := e.Form.State
					if e.SignedUp != 0 {
						state += ", you're signed up"
					}
					closes := ""
					if e.Form.ClosesAt != "" {
						closes = "closes " + fmtDate(e.Form.ClosesAt)
					}
					ls = append(ls, join(" ", fmtWhen(e.StartsOn, e.StartTime, "", "")+":", e.Title, "—", e.Form.Label,
						"("+join(", ", state, closes)+")", e.Form.URL))
				}
				return lines("Sign-ups you can join:", ls, "There are no open sign-ups for you right now."), nil
			}},
		{App: "events", Tool: tool("events_my_signups", "Event planner: my sign-ups",
			"What you've signed up for, upcoming first, with who's included.", nil),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var out struct {
					Signups []epSignup `json:"signups"`
				}
				if err := env.Get(ctx, "/api/v1/me/signups", nil, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, s := range out.Signups {
					who := strings.Join(s.People, ", ")
					if s.Guests > 0 {
						who = join(" ", who, fmt.Sprintf("+%d guests", s.Guests))
					}
					st := ""
					if s.Status == "withdrawn" {
						st = "(withdrawn)"
					}
					ls = append(ls, join(" ", fmtWhen(s.StartsOn, s.StartTime, "", "")+":", s.EventTitle, "—", s.FormLabel, st, who))
				}
				return lines("Your sign-ups:", ls, "You haven't signed up for anything."), nil
			}},
	}
}
