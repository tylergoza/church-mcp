package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// Calendar shapes (APP_CONTRACTS.md, Calendar). Same fields as the
// calendar's existing /api/v1/events; only what the tools show is kept.

type calEvent struct {
	ID          int64    `json:"id"`
	Busy        bool     `json:"busy"`
	Source      string   `json:"source"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	StartsOn    string   `json:"starts_on"`
	StartTime   string   `json:"start_time"`
	EndsOn      string   `json:"ends_on"`
	EndTime     string   `json:"end_time"`
	Rooms       []string `json:"rooms"`
	Public      bool     `json:"public"`
	URL         string   `json:"url"`
	Signup      *struct {
		Label    string `json:"label"`
		URL      string `json:"url"`
		State    string `json:"state"`
		ClosesAt string `json:"closes_at"`
	} `json:"signup"`
}

type calBlock struct {
	StartsOn  string   `json:"starts_on"`
	StartTime string   `json:"start_time"`
	EndsOn    string   `json:"ends_on"`
	EndTime   string   `json:"end_time"`
	Rooms     []string `json:"rooms"`
	Status    string   `json:"status"`
}

type calBooking struct {
	ID        int64    `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	StartsOn  string   `json:"starts_on"`
	StartTime string   `json:"start_time"`
	EndsOn    string   `json:"ends_on"`
	EndTime   string   `json:"end_time"`
	Rooms     []string `json:"rooms"`
	CanChange bool     `json:"can_change"`
	TakenOver bool     `json:"taken_over"`
}

func rooms(r []string) string {
	if len(r) == 0 {
		return ""
	}
	return "in " + strings.Join(r, ", ")
}

func statusNote(s string) string {
	if s == "" || s == "confirmed" {
		return ""
	}
	return "(" + s + ")"
}

func (e calEvent) line() string {
	when := fmtWhen(e.StartsOn, e.StartTime, e.EndsOn, e.EndTime)
	if e.Busy {
		return join(" ", when, "busy (no details)", rooms(e.Rooms))
	}
	return join(" ", fmt.Sprintf("#%d", e.ID), when+":", e.Title, statusNote(e.Status), rooms(e.Rooms))
}

func calendarTools() []Def {
	return []Def{
		{App: "calendar", Tool: tool("calendar_events", "Calendar: events",
			"Church calendar events between two dates, as you may see them (private and pending ones included for members; locked ones show only as busy). Defaults to the next 14 days.",
			map[string]any{
				"from": str("First day, YYYY-MM-DD. Default today."),
				"to":   str("Last day, YYYY-MM-DD. Default 14 days after from. At most 400 days."),
				"room": str("Only events in this room (name as listed by calendar_rooms)."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ From, To, Room string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				from, to, err := rangeArgs(env, a.From, a.To, 14)
				if err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{"from": {from}, "to": {to}}
				if a.Room != "" {
					q.Set("room", a.Room)
				}
				var out struct {
					Events []calEvent `json:"events"`
				}
				if err := env.Get(ctx, "/api/v1/me/calendar", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, e := range out.Events {
					ls = append(ls, e.line())
				}
				return lines(fmt.Sprintf("Calendar %s to %s (ids work with calendar_event):", fmtDate(from), fmtDate(to)), ls,
					"Nothing on the calendar from "+fmtDate(from)+" to "+fmtDate(to)+"."), nil
			}},
		{App: "calendar", Tool: tool("calendar_event", "Calendar: one event",
			"Details of one calendar event by its id (from calendar_events).",
			map[string]any{"id": num("The event's id.")}, "id"),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ ID int64 }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				p, err := idPath("/api/v1/me/calendar", a.ID)
				if err != nil {
					return mcp.Result{}, err
				}
				var e calEvent
				if err := env.Get(ctx, p, nil, &e); err != nil {
					return mcp.Result{}, err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s\nWhen: %s\n", e.Title, fmtWhen(e.StartsOn, e.StartTime, e.EndsOn, e.EndTime))
				if len(e.Rooms) > 0 {
					fmt.Fprintf(&b, "Where: %s\n", strings.Join(e.Rooms, ", "))
				}
				vis := "private (members only)"
				if e.Public {
					vis = "public"
				}
				fmt.Fprintf(&b, "Status: %s; %s\n", orDefault(e.Status, "confirmed"), vis)
				if e.Kind != "" {
					fmt.Fprintf(&b, "Kind: %s\n", e.Kind)
				}
				if e.Source != "" {
					fmt.Fprintf(&b, "From: %s\n", e.Source)
				}
				if e.Description != "" {
					fmt.Fprintf(&b, "About: %s\n", e.Description)
				}
				if e.Signup != nil {
					fmt.Fprintf(&b, "Sign-up: %s (%s) %s\n", e.Signup.Label, e.Signup.State, e.Signup.URL)
				}
				if e.URL != "" {
					fmt.Fprintf(&b, "Link: %s\n", e.URL)
				}
				return mcp.Text(strings.TrimRight(b.String(), "\n")), nil
			}},
		{App: "calendar", Tool: tool("calendar_rooms", "Calendar: rooms",
			"The rooms that can be booked on the church calendar, by building.", nil),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var out struct {
					Buildings []struct {
						Name  string `json:"name"`
						Rooms []struct {
							Name     string `json:"name"`
							Capacity int    `json:"capacity"`
						} `json:"rooms"`
					} `json:"buildings"`
				}
				if err := env.Get(ctx, "/api/v1/me/rooms", nil, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, b := range out.Buildings {
					var rs []string
					for _, r := range b.Rooms {
						if r.Capacity > 0 {
							rs = append(rs, fmt.Sprintf("%s (%d)", r.Name, r.Capacity))
						} else {
							rs = append(rs, r.Name)
						}
					}
					if len(rs) == 0 {
						ls = append(ls, b.Name+" (the whole building)")
					} else {
						ls = append(ls, b.Name+": "+strings.Join(rs, ", "))
					}
				}
				return lines("Bookable rooms (capacity in brackets):", ls, "No rooms can be booked."), nil
			}},
		{App: "calendar", Tool: tool("calendar_availability", "Calendar: room availability",
			"When rooms are already taken between two dates (busy times only, no event details). Anything not listed is free.",
			map[string]any{
				"rooms": str("Room names, comma separated. Default: all bookable rooms."),
				"from":  str("First day, YYYY-MM-DD. Default today."),
				"to":    str("Last day, YYYY-MM-DD. Default the same day as from."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ Rooms, From, To string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				from, to, err := rangeArgs(env, a.From, a.To, 0)
				if err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{"from": {from}, "to": {to}}
				if a.Rooms != "" {
					q.Set("rooms", a.Rooms)
				}
				var out struct {
					Busy []calBlock `json:"busy"`
				}
				if err := env.Get(ctx, "/api/v1/me/availability", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, b := range out.Busy {
					ls = append(ls, join(" ", fmtWhen(b.StartsOn, b.StartTime, b.EndsOn, b.EndTime), rooms(b.Rooms), statusNote(b.Status)))
				}
				what := "rooms"
				if a.Rooms != "" {
					what = a.Rooms
				}
				return lines(fmt.Sprintf("Busy times for %s, %s to %s (everything else is free):", what, fmtDate(from), fmtDate(to)), ls,
					fmt.Sprintf("%s: free the whole time, %s to %s.", what, fmtDate(from), fmtDate(to))), nil
			}},
		{App: "calendar", Tool: tool("calendar_my_bookings", "Calendar: my bookings",
			"Your own room bookings and requests on the church calendar, upcoming first, with their status.", nil),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var out struct {
					Bookings []calBooking `json:"bookings"`
				}
				if err := env.Get(ctx, "/api/v1/me/bookings", nil, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, b := range out.Bookings {
					note := ""
					if b.TakenOver {
						note = "(a planner took this over)"
					}
					ls = append(ls, join(" ", fmt.Sprintf("#%d", b.ID), fmtWhen(b.StartsOn, b.StartTime, b.EndsOn, b.EndTime)+":",
						b.Title, "—", orDefault(b.Status, "confirmed"), rooms(b.Rooms), note))
				}
				return lines("Your bookings:", ls, "You have no room bookings or requests."), nil
			}},
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
