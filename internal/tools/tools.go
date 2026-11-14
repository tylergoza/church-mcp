// Package tools defines the read-only MCP tools, one set per church app.
// Each tool calls that app's `GET /api/v1/me/…` (APP_CONTRACTS.md) as the
// signed-in person and turns the answer into short text: one line per
// thing, dates and times in the church's local time as the apps send them.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// App is a church app the tools can reach. ID is its connection on the
// admin Connections page; Audience is its User Management client ID.
type App struct {
	ID       string
	Name     string
	Audience string
	Hint     string // example address
}

// Apps, in the order the tools are listed.
var Apps = []App{
	{ID: "calendar", Name: "Church calendar", Audience: "calendar", Hint: "https://calendar.example.org"},
	{ID: "directory", Name: "Member directory", Audience: "directory", Hint: "https://directory.example.org"},
	{ID: "events", Name: "Event planner", Audience: "events", Hint: "https://events.example.org"},
	{ID: "tracker", Name: "Maintenance tracker", Audience: "tracker", Hint: "https://tracker.example.org"},
	{ID: "production", Name: "Production planner", Audience: "planner", Hint: "https://planner.example.org"},
}

// Env is what a tool gets to work with: a GET against its app as the
// person, and the church's time zone.
type Env interface {
	Get(ctx context.Context, path string, q url.Values, out any) error
	Location() *time.Location
}

// Def is one tool.
type Def struct {
	App  string // App.ID
	Tool mcp.Tool
	Run  func(ctx context.Context, env Env, args json.RawMessage) (mcp.Result, error)
}

// All is every tool, in a fixed order.
func All() []Def {
	var out []Def
	out = append(out, calendarTools()...)
	out = append(out, directoryTools()...)
	out = append(out, eventsTools()...)
	out = append(out, trackerTools()...)
	out = append(out, productionTools()...)
	return out
}

// maxLines caps a list; the model is told how many more there were.
const maxLines = 40

// helpers ------------------------------------------------------------------

func tool(name, title, desc string, props map[string]any, required ...string) mcp.Tool {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(props) == 0 {
		schema["properties"] = map[string]any{}
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return mcp.Tool{
		Name: name, Title: title, Description: desc, InputSchema: schema,
		Annotations: &mcp.Annotations{Title: title, ReadOnlyHint: true, IdempotentHint: true},
	}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

// argError is an input mistake the model can fix.
type argError string

func (e argError) Error() string { return string(e) }

// parse decodes arguments, as a tool error the model can act on.
func parse(raw json.RawMessage, into any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return argError("Couldn't read the arguments: " + err.Error())
	}
	return nil
}

// dateArg checks a YYYY-MM-DD argument; "" gives def.
func dateArg(name, v, def string) (string, error) {
	if v == "" {
		return def, nil
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", argError(fmt.Sprintf("%s should be a date like 2026-11-16, not %q.", name, v))
	}
	return v, nil
}

func today(env Env) time.Time { return time.Now().In(env.Location()) }

func day(t time.Time) string { return t.Format("2006-01-02") }

// fmtDate turns "2026-11-16" into "Mon Nov 16, 2026".
func fmtDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("Mon Jan 2, 2006")
}

// fmtTime turns "18:30" into "6:30 PM".
func fmtTime(hm string) string {
	t, err := time.Parse("15:04", hm)
	if err != nil {
		return hm
	}
	return t.Format("3:04 PM")
}

// fmtWhen is a short "Mon Nov 16, 2026 6:30 PM–8:00 PM" from the apps'
// local date and time fields. Times may be blank (all day).
func fmtWhen(startsOn, startTime, endsOn, endTime string) string {
	s := fmtDate(startsOn)
	if startTime != "" {
		s += " " + fmtTime(startTime)
	} else {
		s += " (all day)"
	}
	if endsOn != "" && endsOn != startsOn {
		s += " – " + fmtDate(endsOn)
		if endTime != "" {
			s += " " + fmtTime(endTime)
		}
	} else if endTime != "" {
		s += "–" + fmtTime(endTime)
	}
	return s
}

// lines joins a header and list lines, capped at maxLines.
func lines(header string, items []string, empty string) mcp.Result {
	if len(items) == 0 {
		return mcp.Text(empty)
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for i, it := range items {
		if i == maxLines {
			fmt.Fprintf(&b, "… and %d more not shown; narrow the search or the dates.\n", len(items)-maxLines)
			break
		}
		b.WriteString("- ")
		b.WriteString(it)
		b.WriteString("\n")
	}
	return mcp.Text(strings.TrimRight(b.String(), "\n"))
}

// join drops blanks and joins with sep.
func join(sep string, parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

func idPath(prefix string, id int64) (string, error) {
	if id <= 0 {
		return "", argError("id should be a positive number from an earlier list.")
	}
	return fmt.Sprintf("%s/%d", prefix, id), nil
}

func parseDay(d string) (time.Time, error) { return time.Parse("2006-01-02", d) }

// rangeArgs checks a from/to pair: from defaults to today, to to from plus
// defDays; to may not be before from or more than 400 days after it.
func rangeArgs(env Env, from, to string, defDays int) (string, string, error) {
	from, err := dateArg("from", from, day(today(env)))
	if err != nil {
		return "", "", err
	}
	f, _ := parseDay(from)
	to, err = dateArg("to", to, day(f.AddDate(0, 0, defDays)))
	if err != nil {
		return "", "", err
	}
	t, _ := parseDay(to)
	if t.Before(f) {
		return "", "", argError("to is before from.")
	}
	if t.Sub(f) > 400*24*time.Hour {
		return "", "", argError("That range is too long; ask for 400 days or fewer.")
	}
	return from, to, nil
}

// IsInputError reports whether err is a mistake in the tool's arguments,
// whose message is meant for the model.
func IsInputError(err error) bool {
	var a argError
	return errors.As(err, &a)
}
