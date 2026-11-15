package tools

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEveryToolIsReadOnly(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range All() {
		n := d.Tool.Name
		if seen[n] {
			t.Errorf("%s defined twice", n)
		}
		seen[n] = true
		if d.Tool.Annotations == nil || !d.Tool.Annotations.ReadOnlyHint || d.Tool.Annotations.DestructiveHint {
			t.Errorf("%s isn't marked read-only", n)
		}
		if d.Tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s accepts unknown arguments", n)
		}
		if !strings.HasPrefix(n, d.App+"_") {
			t.Errorf("%s doesn't start with its app %s", n, d.App)
		}
	}
}

type fakeEnv struct {
	paths []string
	reply string
}

func (f *fakeEnv) Location() *time.Location {
	l, _ := time.LoadLocation("America/New_York")
	return l
}

func (f *fakeEnv) Get(_ context.Context, path string, q url.Values, out any) error {
	f.paths = append(f.paths, path+"?"+q.Encode())
	return json.Unmarshal([]byte(f.reply), out)
}

func find(name string) Def {
	for _, d := range All() {
		if d.Tool.Name == name {
			return d
		}
	}
	panic(name)
}

func TestBadArgumentsAreInputErrors(t *testing.T) {
	env := &fakeEnv{reply: `{}`}
	for _, c := range []struct{ tool, args string }{
		{"calendar_events", `{"from":"tomorrow"}`},
		{"calendar_events", `{"from":"2026-11-20","to":"2026-11-01"}`},
		{"calendar_events", `{"surprise":1}`},
		{"calendar_event", `{"id":0}`},
	} {
		_, err := find(c.tool).Run(context.Background(), env, json.RawMessage(c.args))
		if err == nil || !IsInputError(err) {
			t.Errorf("%s %s: %v", c.tool, c.args, err)
		}
	}
	if len(env.paths) != 0 {
		t.Errorf("bad arguments still called the app: %v", env.paths)
	}
}

func TestCalendarEventsFormatsLocalTimes(t *testing.T) {
	env := &fakeEnv{reply: `{"events":[{"id":3,"title":"Men's breakfast","starts_on":"2026-11-21","start_time":"08:00","ends_on":"2026-11-21","end_time":"09:30","rooms":["Fellowship Hall"]}]}`}
	res, err := find("calendar_events").Run(context.Background(), env, json.RawMessage(`{"from":"2026-11-16","to":"2026-11-22"}`))
	if err != nil || res.IsError {
		t.Fatal(err, res)
	}
	text := res.Content[0].Text
	for _, want := range []string{"Men's breakfast", "Sat Nov 21, 2026", "8:00 AM", "Fellowship Hall"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if env.paths[0] != "/api/v1/me/calendar?from=2026-11-16&to=2026-11-22" {
		t.Errorf("called %v", env.paths)
	}
}

func TestTrackerFindAllSupplies(t *testing.T) {
	env := &fakeEnv{reply: `{"supplies":[{"id":2,"name":"Paper towels","unit":"rolls","place":{"id":6,"path":"Main / Kitchen"},"on_hand":2,"total":2,"stock":"low"}]}`}
	res, err := find("tracker_find").Run(context.Background(), env, json.RawMessage(`{"kind":"supplies"}`))
	if err != nil || res.IsError || !strings.Contains(res.Content[0].Text, "Paper towels — 2 rolls on hand LOW at Main / Kitchen") {
		t.Fatal(err, res)
	}
	if len(env.paths) != 1 || env.paths[0] != "/api/v1/me/supplies?" {
		t.Errorf("called %v", env.paths)
	}
	// Other kinds still need q.
	for _, args := range []string{`{}`, `{"kind":"items"}`} {
		if _, err := find("tracker_find").Run(context.Background(), env, json.RawMessage(args)); err == nil || !IsInputError(err) {
			t.Errorf("%s: %v", args, err)
		}
	}
}

func TestTrackerMaintenance(t *testing.T) {
	env := &fakeEnv{reply: `{"tasks":[{"id":1,"item":"Furnace","place":{"id":3,"path":"Main / Mechanical"},"name":"Change air filter","interval":"Every 3 months","last_completed_on":"2026-04-12","next_due_on":"2026-07-12","status":"overdue","supply":"Furnace filters","supply_short":true},{"id":2,"item":"Chairs","place":{"id":4,"path":"Main / Hall"},"name":"Wipe down","interval":"One time","last_completed_on":"","next_due_on":"","status":"unscheduled","supply":"","supply_short":false}]}`}
	res, err := find("tracker_maintenance").Run(context.Background(), env, json.RawMessage(`{"q":"filter","status":"overdue","place":"main"}`))
	if err != nil || res.IsError {
		t.Fatal(err, res)
	}
	text := res.Content[0].Text
	for _, want := range []string{"OVERDUE: Furnace — Change air filter (every 3 months)", "last done Sun Apr 12, 2026", "due Sun Jul 12, 2026", "NOT ENOUGH ON HAND", "last done never recorded", "no due date"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if env.paths[0] != "/api/v1/me/tasks?place=main&q=filter&status=overdue" {
		t.Errorf("called %v", env.paths)
	}
}

func TestTrackerHistory(t *testing.T) {
	env := &fakeEnv{reply: `{"history":[{"id":9,"performed_on":"2026-04-12","item":"Furnace","place":{"id":3,"path":"Main / Mechanical"},"task":"Change air filter","kind":"replaced","replaced":"2 × Furnace filters","performed_by":"Eddie","notes":"20x25x1"}]}`}
	res, err := find("tracker_history").Run(context.Background(), env, json.RawMessage(`{"q":"filter","since":"2026-01-01","limit":5}`))
	if err != nil || res.IsError {
		t.Fatal(err, res)
	}
	text := res.Content[0].Text
	for _, want := range []string{"Sun Apr 12, 2026: Furnace — Change air filter (replaced 2 × Furnace filters) by Eddie at Main / Mechanical — 20x25x1"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if env.paths[0] != "/api/v1/me/history?limit=5&q=filter&since=2026-01-01" {
		t.Errorf("called %v", env.paths)
	}
	if _, err := find("tracker_history").Run(context.Background(), env, json.RawMessage(`{"since":"last year"}`)); err == nil || !IsInputError(err) {
		t.Errorf("bad since: %v", err)
	}
}

// A group item's units each get a line with their label and last
// replacement, and the check's filter count shows.
func TestTrackerMaintenanceUnits(t *testing.T) {
	env := &fakeEnv{reply: `{"tasks":[{"id":3,"item":"Air Return","place":{"id":9,"path":"Old - Fellowship Hall"},"name":"Check Filters","interval":"Every month","last_completed_on":"2026-11-07","next_due_on":"2026-12-07","status":"upcoming","supply":"14x30 Filter","supply_amount":4,"supply_short":false,"description":"","quantity":4,"item_notes":"","units":[{"number":1,"id":"1","label":"Northwest (away from sanctuary)","last_replaced":"2026-11-07"},{"number":2,"id":"2","label":"Southwest (towards Sanctuary)","last_replaced":""},{"number":3,"id":"3","label":"Northeast (away from sanctuary)","last_replaced":"2026-11-14"},{"number":4,"id":"4","label":"Southeast (towards Sanctuary)","last_replaced":""}]}]}`}
	res, err := find("tracker_maintenance").Run(context.Background(), env, json.RawMessage(`{"q":"filter"}`))
	if err != nil || res.IsError {
		t.Fatal(err, res)
	}
	text := res.Content[0].Text
	for _, want := range []string{"Air Return ×4 — Check Filters", "uses 4 × 14x30 Filter",
		"\n    #1 Northwest (away from sanctuary): 14x30 Filter last replaced Sat Nov 7, 2026",
		"\n    #2 Southwest (towards Sanctuary): no replacement recorded",
		"\n    #4 Southeast (towards Sanctuary)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}
