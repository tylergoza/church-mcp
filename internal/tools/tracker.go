package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// Tracker shapes (APP_CONTRACTS.md, Maintenance tracker): the tracker's
// existing /api/v1 shapes, under /api/v1/me with a q search on each.

type trPlaceRef struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

type trPlace struct {
	ID       int64    `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Bookable bool     `json:"bookable"`
	Address  string   `json:"address"`
	Tags     []string `json:"tags"`
}

type trProduct struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Total      int    `json:"total"`
	PlaceCount int    `json:"place_count"`
}

type trItem struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Category     string     `json:"category"`
	Quantity     int        `json:"quantity"`
	Place        trPlaceRef `json:"place"`
	Manufacturer string     `json:"manufacturer"`
	Model        string     `json:"model"`
	Notes        string     `json:"notes"`
	Supply       string     `json:"supply"`
	SupplyPer    int        `json:"supply_per"`
	Units        []trUnit   `json:"units"`
}

// trUnit is one numbered unit of a group item (e.g. one of four air
// returns in a room).
type trUnit struct {
	Number       int    `json:"number"`
	ID           string `json:"id"`
	Label        string `json:"label"`
	LastReplaced string `json:"last_replaced"`
}

// unitLines renders a group item's units, one indented line each:
// "    #1 Northwest (away from sanctuary): filter last replaced Sat Nov 7, 2026".
func unitLines(units []trUnit, supply string) string {
	if len(units) == 0 {
		return ""
	}
	what := "supply"
	if supply != "" {
		what = supply
	}
	var b strings.Builder
	for _, u := range units {
		name := "#" + strconv.Itoa(u.Number)
		if u.ID != "" && u.ID != strconv.Itoa(u.Number) {
			name += " (ID " + u.ID + ")"
		}
		last := "no replacement recorded"
		if u.LastReplaced != "" {
			last = what + " last replaced " + fmtDate(u.LastReplaced)
		}
		b.WriteString("\n    " + join(" ", name, u.Label) + ": " + last)
	}
	return b.String()
}

type trSupply struct {
	ID     int64      `json:"id"`
	Name   string     `json:"name"`
	Unit   string     `json:"unit"`
	Place  trPlaceRef `json:"place"`
	OnHand int        `json:"on_hand"`
	Total  int        `json:"total"`
	Stock  string     `json:"stock"` // ok | low | out
}

type trTask struct {
	ID              int64      `json:"id"`
	Item            string     `json:"item"`
	Place           trPlaceRef `json:"place"`
	Name            string     `json:"name"`
	Interval        string     `json:"interval"`
	LastCompletedOn string     `json:"last_completed_on"`
	NextDueOn       string     `json:"next_due_on"`
	Status          string     `json:"status"` // overdue | due-soon | upcoming | unscheduled
	Supply          string     `json:"supply"`
	SupplyAmount    int        `json:"supply_amount"`
	SupplyShort     bool       `json:"supply_short"`
	Description     string     `json:"description"`
	Quantity        int        `json:"quantity"`
	ItemNotes       string     `json:"item_notes"`
	Units           []trUnit   `json:"units"`
}

type trLog struct {
	ID          int64      `json:"id"`
	PerformedOn string     `json:"performed_on"`
	Item        string     `json:"item"`
	Place       trPlaceRef `json:"place"`
	Task        string     `json:"task"`
	Kind        string     `json:"kind"` // work | replaced | moved
	Replaced    string     `json:"replaced"`
	Units       string     `json:"units"`
	SupplyUsed  string     `json:"supply_used"`
	PerformedBy string     `json:"performed_by"`
	Notes       string     `json:"notes"`
}

var trackerKinds = []string{"places", "products", "items", "supplies"}

func trackerTools() []Def {
	return append([]Def{
		{App: "tracker", Tool: tool("tracker_find", "Tracker: find",
			"Search the maintenance tracker: places (buildings, rooms), products (kinds of equipment), items (where each thing is) and supplies (stock levels). Searches all four unless kind is given. For \"what do our stocks look like?\" use kind: \"supplies\" with no q (lists every supply with how many are on hand; low: true for just those running low or out). This does NOT say when work was done or what is due: for \"when were the furnace filters last changed?\" use tracker_history, and for \"what maintenance is overdue?\" use tracker_maintenance.",
			map[string]any{
				"q":    str("What to look for, e.g. \"projector\" or \"fellowship hall\". Optional only with kind: \"supplies\"."),
				"kind": enum("Only this kind.", trackerKinds...),
				"low":  map[string]any{"type": "boolean", "description": "Supplies only: just the ones running low or out."},
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct {
					Q, Kind string
					Low     bool
				}
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				kinds := trackerKinds
				if a.Kind != "" {
					kinds = []string{a.Kind}
				}
				if a.Low {
					kinds = []string{"supplies"}
				}
				// No q is fine for supplies alone: the stock overview.
				if strings.TrimSpace(a.Q) == "" && !a.Low && a.Kind != "supplies" {
					return mcp.Result{}, argError("q is needed (or kind: \"supplies\" alone for every supply, or low: true for supplies running low).")
				}
				var sections []string
				for _, k := range kinds {
					q := url.Values{}
					if a.Q != "" {
						q.Set("q", a.Q)
					}
					if a.Low {
						q.Set("low", "1")
					}
					ls, err := trackerSearch(ctx, env, k, q)
					if err != nil {
						return mcp.Result{}, err
					}
					if len(ls) > 0 {
						sections = append(sections, lines(strings.ToUpper(k[:1])+k[1:]+":", ls, "").Content[0].Text)
					}
				}
				if len(sections) == 0 {
					return mcp.Text(fmt.Sprintf("Nothing in the tracker matches %q.", a.Q)), nil
				}
				return mcp.Text(strings.Join(sections, "\n\n")), nil
			}},
	}, trackerMaintenanceTools()...)
}

var trackerStatuses = []string{"overdue", "due-soon", "upcoming", "unscheduled"}

func (t trTask) line() string {
	last := "never recorded"
	if t.LastCompletedOn != "" {
		last = fmtDate(t.LastCompletedOn)
	}
	due := "no due date"
	if t.NextDueOn != "" {
		due = "due " + fmtDate(t.NextDueOn)
	}
	supply := ""
	if t.Supply != "" {
		supply = "uses "
		if t.SupplyAmount > 1 {
			supply += strconv.Itoa(t.SupplyAmount) + " × "
		}
		supply += t.Supply
		if t.SupplyShort {
			supply += " (NOT ENOUGH ON HAND)"
		}
		supply += ";"
	}
	item := t.Item
	if t.Quantity > 1 {
		item = fmt.Sprintf("%s ×%d", t.Item, t.Quantity)
	}
	return join(" ", strings.ToUpper(t.Status)+":", item+" —", t.Name, "("+strings.ToLower(t.Interval)+");",
		"last done "+last+";", due+";", atPlace("at ", t.Place.Path+semicolonIf(t.Place.Path)), supply,
		atPlace("Instructions: ", t.Description), atPlace("Item notes: ", t.ItemNotes)) + unitLines(t.Units, t.Supply)
}

func (l trLog) line() string {
	what := orDefault(l.Task, l.Kind)
	if l.Replaced != "" {
		what += " (replaced " + l.Replaced + ")"
	} else if l.SupplyUsed != "" {
		what += " (used " + l.SupplyUsed + ")"
	}
	if l.Units != "" {
		what += " on " + l.Units
	}
	return join(" ", fmtDate(l.PerformedOn)+":", l.Item+" —", what, atPlace("by ", l.PerformedBy), atPlace("at ", l.Place.Path), atPlace("— ", l.Notes))
}

func trackerMaintenanceTools() []Def {
	return []Def{
		{App: "tracker", Tool: tool("tracker_maintenance", "Tracker: maintenance schedule",
			"Scheduled maintenance tasks from the tracker, most urgent first: item, task, how often, when it was last completed, when it is next due, whether it is overdue, how many of its supply one check uses, its instructions and item notes. When an item is a group (e.g. an air return ×4 in one room), each unit is listed on its own line with its location label and when its filter/supply was last replaced: report every unit. Use for \"what maintenance is overdue?\" (status: overdue) or \"what is due soon?\"; q finds one task (\"furnace filter\") and its last_completed date tells when it was last done. For the full record of what was done and when (every filter change, who did it), use tracker_history.",
			map[string]any{
				"q":      str("Optional: matches the item, task or supply name, e.g. \"filter\"."),
				"status": enum("Only tasks in this state.", trackerStatuses...),
				"place":  str("Optional: part of a place name, e.g. \"kitchen\"."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ Q, Status, Place string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{}
				for k, v := range map[string]string{"q": a.Q, "status": a.Status, "place": a.Place} {
					if strings.TrimSpace(v) != "" {
						q.Set(k, strings.TrimSpace(v))
					}
				}
				var out struct {
					Tasks []trTask `json:"tasks"`
				}
				if err := env.Get(ctx, "/api/v1/me/tasks", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, t := range out.Tasks {
					ls = append(ls, t.line())
				}
				return lines("Maintenance tasks, most urgent first:", ls, "No maintenance tasks match."), nil
			}},
		{App: "tracker", Tool: tool("tracker_history", "Tracker: maintenance history",
			"What maintenance was done and when, newest first: date, item, task, what was replaced (e.g. \"2 × Furnace filters\"), who did it, notes. Use for \"when were the furnace filters last changed?\" (q: \"filter\"; the first line is the latest), \"what was done this year?\" (since). For what is coming due or overdue, use tracker_maintenance.",
			map[string]any{
				"q":     str("Optional: matches the item, task or supply name, e.g. \"filter\" or \"furnace\"."),
				"since": str("Optional: only work done on or after this date, YYYY-MM-DD."),
				"limit": num("Optional: how many entries, newest first (default and most 40)."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct {
					Q, Since string
					Limit    int
				}
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				since, err := dateArg("since", a.Since, "")
				if err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{}
				if strings.TrimSpace(a.Q) != "" {
					q.Set("q", strings.TrimSpace(a.Q))
				}
				if since != "" {
					q.Set("since", since)
				}
				if a.Limit > 0 && a.Limit < maxLines {
					q.Set("limit", fmt.Sprint(a.Limit))
				} else {
					q.Set("limit", fmt.Sprint(maxLines))
				}
				var out struct {
					History []trLog `json:"history"`
				}
				if err := env.Get(ctx, "/api/v1/me/history", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, l := range out.History {
					ls = append(ls, l.line())
				}
				return lines("Maintenance history, newest first:", ls, "No maintenance history matches."), nil
			}},
		{App: "tracker", Tool: tool("tracker_problems", "Tracker: problem reports",
			"Problem reports: things people reported broken or not working (a TV, a light, a leak, the sound board), with status (open, in progress, resolved), the item and place, who reported it, who it's assigned to, details and the latest updates. Use for \"what's broken?\", \"is anyone fixing the TV?\", \"what problems are open in the fellowship hall?\". By default shows open and in-progress reports; status \"resolved\" or \"all\" for fixed ones. For scheduled maintenance use tracker_maintenance.",
			map[string]any{
				"q":      str("Optional: matches the title, details, item or place, e.g. \"TV\" or \"leak\"."),
				"status": enum("Optional: active (open or in progress, the default), open, in_progress, resolved or all.", "active", "open", "in_progress", "resolved", "all"),
				"place":  str("Optional: only reports in a place, e.g. \"fellowship hall\"."),
			}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ Q, Status, Place string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{}
				for k, v := range map[string]string{"q": a.Q, "status": a.Status, "place": a.Place} {
					if strings.TrimSpace(v) != "" {
						q.Set(k, strings.TrimSpace(v))
					}
				}
				q.Set("limit", fmt.Sprint(maxLines))
				var out struct {
					Problems []trProblem `json:"problems"`
				}
				if err := env.Get(ctx, "/api/v1/me/problems", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, p := range out.Problems {
					ls = append(ls, p.line(env.Location()))
				}
				return lines("Problem reports, newest first:", ls, "No problem reports match."), nil
			}},
	}
}

type trProblemUpdate struct {
	At         string `json:"at"`
	By         string `json:"by"`
	Status     string `json:"status"`
	AssignedTo string `json:"assigned_to"`
	Note       string `json:"note"`
}

type trProblem struct {
	ID         int64             `json:"id"`
	Title      string            `json:"title"`
	Details    string            `json:"details"`
	Status     string            `json:"status"`
	Item       string            `json:"item"`
	Unit       string            `json:"unit"`
	Place      trPlaceRef        `json:"place"`
	AssignedTo string            `json:"assigned_to"`
	ReportedBy string            `json:"reported_by"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
	ResolvedAt string            `json:"resolved_at"`
	Updates    []trProblemUpdate `json:"updates"`
}

var problemStatus = map[string]string{"open": "OPEN", "in_progress": "IN PROGRESS", "resolved": "RESOLVED"}

// localStamp turns an RFC 3339 time into "Sun Nov 15, 2026 2:05 PM" in
// the church's time zone.
func localStamp(ts string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.In(loc).Format("Mon Jan 2, 2006 3:04 PM")
}

func (p trProblem) line(loc *time.Location) string {
	st := problemStatus[p.Status]
	if st == "" {
		st = strings.ToUpper(p.Status)
	}
	what := p.Title
	if p.Item != "" {
		what += " (" + join(", ", p.Item, p.Unit) + ")"
	}
	head := join("; ",
		atPlace("at ", p.Place.Path),
		atPlace("reported by ", p.ReportedBy)+atPlace(" on ", localStamp(p.CreatedAt, loc)),
		"assigned to "+cmp.Or(p.AssignedTo, "nobody"),
		atPlace("resolved ", localStamp(p.ResolvedAt, loc)))
	s := fmt.Sprintf("%s: %s — %s", st, what, head)
	if d := strings.TrimSpace(p.Details); d != "" {
		s += "\n  Details: " + d
	}
	for _, u := range p.Updates {
		var bits []string
		if u.Status != "" {
			bits = append(bits, "status → "+cmp.Or(problemStatus[u.Status], u.Status))
		}
		if u.AssignedTo != "" {
			bits = append(bits, "assigned to "+u.AssignedTo)
		}
		if n := strings.TrimSpace(u.Note); n != "" {
			bits = append(bits, n)
		}
		who := ""
		if u.By != "" {
			who = "(" + u.By + ")"
		}
		s += "\n  " + join(" ", localStamp(u.At, loc), who) + ": " + strings.Join(bits, "; ")
	}
	return s
}

func trackerSearch(ctx context.Context, env Env, kind string, q url.Values) ([]string, error) {
	var ls []string
	switch kind {
	case "places":
		var out struct {
			Places []trPlace `json:"places"`
		}
		if err := env.Get(ctx, "/api/v1/me/places", q, &out); err != nil {
			return nil, err
		}
		for _, p := range out.Places {
			b := ""
			if p.Bookable {
				b = "(bookable)"
			}
			ls = append(ls, join(" ", fmt.Sprintf("#%d", p.ID), orDefault(p.Path, p.Name), statusNote(p.Kind), b, p.Address))
		}
	case "products":
		var out struct {
			Products []trProduct `json:"products"`
		}
		if err := env.Get(ctx, "/api/v1/me/products", q, &out); err != nil {
			return nil, err
		}
		for _, p := range out.Products {
			ls = append(ls, join(" ", fmt.Sprintf("#%d", p.ID), p.Name, statusNote(p.Category),
				fmt.Sprintf("— %d in %d place(s)", p.Total, p.PlaceCount)))
		}
	case "items":
		var out struct {
			Items []trItem `json:"items"`
		}
		if err := env.Get(ctx, "/api/v1/me/items", q, &out); err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			n := ""
			if it.Quantity > 1 {
				n = fmt.Sprintf("x%d", it.Quantity)
			}
			supply := ""
			if it.Supply != "" {
				supply = "uses " + it.Supply
				if it.SupplyPer > 1 {
					supply = fmt.Sprintf("uses %d × %s each", it.SupplyPer, it.Supply)
				}
			}
			ls = append(ls, join(" ", fmt.Sprintf("#%d", it.ID), it.Name, n, join(" ", it.Manufacturer, it.Model), atPlace("— at ", it.Place.Path),
				atPlace("— ", supply), atPlace("— notes: ", it.Notes))+unitLines(it.Units, it.Supply))
		}
	case "supplies":
		var out struct {
			Supplies []trSupply `json:"supplies"`
		}
		if err := env.Get(ctx, "/api/v1/me/supplies", q, &out); err != nil {
			return nil, err
		}
		for _, s := range out.Supplies {
			stock := ""
			if s.Stock != "" && s.Stock != "ok" {
				stock = strings.ToUpper(s.Stock)
			}
			ls = append(ls, join(" ", fmt.Sprintf("#%d", s.ID), s.Name, fmt.Sprintf("— %d %s on hand", s.OnHand, s.Unit), stock, atPlace("at ", s.Place.Path)))
		}
	default:
		return nil, errors.New("unknown kind")
	}
	return ls, nil
}

// atPlace is "<prefix><path>", or "" when the app sent no path, so a line
// never ends in a dangling "at".
// semicolonIf is ";" when s isn't blank.
func semicolonIf(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ";"
}

func atPlace(prefix, path string) string {
	if path == "" {
		return ""
	}
	return prefix + path
}
