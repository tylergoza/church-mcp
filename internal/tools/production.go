package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// Production planner shapes (APP_CONTRACTS.md, Production planner). The
// planner has no JSON API yet; these are new.

type ppDate struct {
	Kind      string   `json:"kind"` // rehearsal | tech | dress | performance | …
	StartsOn  string   `json:"starts_on"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
	Tentative bool     `json:"tentative"`
	Rooms     []string `json:"rooms"`
	Notes     string   `json:"notes"`
}

type ppProduction struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Where    string   `json:"where"`
	Director string   `json:"director"`
	Notes    string   `json:"notes"`
	NextDate *ppDate  `json:"next_date"`
	Schedule []ppDate `json:"schedule"`
	Cast     []struct {
		Name string `json:"name"`
		Part string `json:"part"`
	} `json:"cast"`
}

func (d ppDate) line() string {
	pencil := ""
	if d.Tentative {
		pencil = "(pencilled in)"
	}
	return join(" ", fmtWhen(d.StartsOn, d.StartTime, "", d.EndTime)+":", d.Kind, pencil, rooms(d.Rooms), d.Notes)
}

func productionTools() []Def {
	return []Def{
		{App: "production", Tool: tool("production_list", "Productions: list",
			"Plays, musicals and other productions you can see in the production planner, with status and next date.",
			map[string]any{"status": enum("Only this status.", "planning", "rehearsing", "ready", "done", "cancelled")}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ Status string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				q := url.Values{}
				if a.Status != "" {
					q.Set("status", a.Status)
				}
				var out struct {
					Productions []ppProduction `json:"productions"`
				}
				if err := env.Get(ctx, "/api/v1/me/productions", q, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, p := range out.Productions {
					next := ""
					if p.NextDate != nil {
						next = "— next: " + p.NextDate.line()
					}
					ls = append(ls, join(" ", fmt.Sprintf("#%d", p.ID), p.Title, statusNote(strings.ToLower(p.Status)), next))
				}
				return lines("Productions (ids work with production_detail):", ls, "No productions you can see."), nil
			}},
		{App: "production", Tool: tool("production_detail", "Productions: one production",
			"One production's details and full schedule (rehearsals, tech, performances), plus cast if you may see it.",
			map[string]any{"id": num("The production's id (from production_list).")}, "id"),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ ID int64 }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				p, err := idPath("/api/v1/me/productions", a.ID)
				if err != nil {
					return mcp.Result{}, err
				}
				var pr ppProduction
				if err := env.Get(ctx, p, nil, &pr); err != nil {
					return mcp.Result{}, err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s\n", join(" ", pr.Title, statusNote(pr.Kind)))
				for _, f := range [][2]string{{"Status", pr.Status}, {"Where", pr.Where}, {"Director", pr.Director}, {"Notes", pr.Notes}} {
					if f[1] != "" {
						fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
					}
				}
				if len(pr.Schedule) > 0 {
					b.WriteString("Schedule:\n")
					for i, d := range pr.Schedule {
						if i == maxLines {
							fmt.Fprintf(&b, "… and %d more dates\n", len(pr.Schedule)-maxLines)
							break
						}
						fmt.Fprintf(&b, "- %s\n", d.line())
					}
				}
				if len(pr.Cast) > 0 {
					var cs []string
					for _, c := range pr.Cast {
						cs = append(cs, join(" as ", c.Name, c.Part))
					}
					fmt.Fprintf(&b, "Cast: %s\n", strings.Join(cs, "; "))
				}
				return mcp.Text(strings.TrimRight(b.String(), "\n")), nil
			}},
	}
}
