package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tylergoza/church-mcp/internal/mcp"
)

// Directory shapes (APP_CONTRACTS.md, Member directory). Every field the
// directory sends has been through its Visible(); a field it left out is
// one this person may not see, and the tools never guess at it.

type dirRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type dirPerson struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Role        string  `json:"role"` // head | adult | child, in the household
	Age         int     `json:"age"`
	Email       string  `json:"email"`
	Phone       string  `json:"phone"`
	Birthday    string  `json:"birthday"`    // "March 4" or "March 4, 1980"
	Anniversary string  `json:"anniversary"` // same
	Household   *dirRef `json:"household"`
	Households  []struct {
		dirRef
		Role string `json:"role"`
	} `json:"households"`
	Family []struct {
		dirRef
		Relation string `json:"relation"`
	} `json:"family"`
	Groups []dirRef `json:"groups"`
}

type dirHousehold struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Addresses []struct {
		ID    int64    `json:"id"`
		Label string   `json:"label"`
		Lines []string `json:"lines"`
	} `json:"addresses"`
	Members []struct {
		dirPerson
		AddressID int64 `json:"address_id"`
	} `json:"members"`
}

func (p dirPerson) contact() string {
	return join(", ", p.Email, p.Phone)
}

func directoryTools() []Def {
	return []Def{
		{App: "directory", Tool: tool("directory_search", "Directory: search people",
			"Search the church member directory by name (first, last or goes-by). Shows only what each person lets you see.",
			map[string]any{"q": str("Part of a name, e.g. \"smith\".")}, "q"),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ Q string }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				if strings.TrimSpace(a.Q) == "" {
					return mcp.Result{}, argError("q is needed: part of a name.")
				}
				var out struct {
					People []dirPerson `json:"people"`
				}
				if err := env.Get(ctx, "/api/v1/me/people", url.Values{"q": {a.Q}, "limit": {fmt.Sprint(maxLines + 1)}}, &out); err != nil {
					return mcp.Result{}, err
				}
				var ls []string
				for _, p := range out.People {
					hh := ""
					if p.Household != nil {
						hh = fmt.Sprintf("— %s household (#%d)", p.Household.Name, p.Household.ID)
					}
					ls = append(ls, join(" ", fmt.Sprintf("#%d", p.ID), p.Name, hh, p.contact()))
				}
				return lines(fmt.Sprintf("People matching %q (ids work with directory_person and directory_household):", a.Q), ls,
					fmt.Sprintf("Nobody in the directory matches %q.", a.Q)), nil
			}},
		{App: "directory", Tool: tool("directory_person", "Directory: one person",
			"One person's directory entry by id (from directory_search): contact details, birthday, households, family and groups, as far as they let you see them.",
			map[string]any{"id": num("The person's id.")}, "id"),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ ID int64 }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				p, err := idPath("/api/v1/me/people", a.ID)
				if err != nil {
					return mcp.Result{}, err
				}
				var pp dirPerson
				if err := env.Get(ctx, p, nil, &pp); err != nil {
					return mcp.Result{}, err
				}
				var b strings.Builder
				b.WriteString(pp.Name + "\n")
				for _, f := range [][2]string{{"Email", pp.Email}, {"Phone", pp.Phone}, {"Birthday", pp.Birthday}, {"Anniversary", pp.Anniversary}} {
					if f[1] != "" {
						fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
					}
				}
				for _, h := range pp.Households {
					fmt.Fprintf(&b, "Household: %s (#%d)%s\n", h.Name, h.ID, map[bool]string{true: ", " + h.Role, false: ""}[h.Role != ""])
				}
				var fam []string
				for _, f := range pp.Family {
					fam = append(fam, fmt.Sprintf("%s (%s, #%d)", f.Name, f.Relation, f.ID))
				}
				if len(fam) > 0 {
					fmt.Fprintf(&b, "Family: %s\n", strings.Join(fam, "; "))
				}
				var gs []string
				for _, g := range pp.Groups {
					gs = append(gs, g.Name)
				}
				if len(gs) > 0 {
					fmt.Fprintf(&b, "Groups: %s\n", strings.Join(gs, ", "))
				}
				return mcp.Text(strings.TrimRight(b.String(), "\n")), nil
			}},
		{App: "directory", Tool: tool("directory_household", "Directory: household",
			"A household's members and addresses. Without an id: your own household(s).",
			map[string]any{"id": num("The household's id (from directory_search). Leave out for your own.")}),
			Run: func(ctx context.Context, env Env, raw json.RawMessage) (mcp.Result, error) {
				var a struct{ ID int64 }
				if err := parse(raw, &a); err != nil {
					return mcp.Result{}, err
				}
				ids := []int64{a.ID}
				if a.ID == 0 {
					var me struct {
						Households []struct {
							ID int64 `json:"id"`
						} `json:"households"`
					}
					if err := env.Get(ctx, "/api/v1/me", nil, &me); err != nil {
						return mcp.Result{}, err
					}
					ids = nil
					for _, h := range me.Households {
						ids = append(ids, h.ID)
					}
					if len(ids) == 0 {
						return mcp.Text("You aren't in a household in the directory yet."), nil
					}
				}
				var parts []string
				for _, id := range ids {
					p, err := idPath("/api/v1/me/households", id)
					if err != nil {
						return mcp.Result{}, err
					}
					var h dirHousehold
					if err := env.Get(ctx, p, nil, &h); err != nil {
						return mcp.Result{}, err
					}
					parts = append(parts, h.text())
				}
				return mcp.Text(strings.Join(parts, "\n\n")), nil
			}},
	}
}

func (h dirHousehold) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s household (#%d)\n", h.Name, h.ID)
	for _, a := range h.Addresses {
		fmt.Fprintf(&b, "Address%s: %s\n", map[bool]string{true: " (" + a.Label + ")", false: ""}[a.Label != ""], strings.Join(a.Lines, ", "))
	}
	for _, m := range h.Members {
		age := ""
		if m.Age > 0 {
			age = fmt.Sprintf("age %d", m.Age)
		}
		fmt.Fprintf(&b, "- %s\n", join(" ", fmt.Sprintf("#%d", m.ID), m.Name, statusNote(m.Role), age, m.contact()))
	}
	return strings.TrimRight(b.String(), "\n")
}
