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
