package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

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

var trackerKinds = []string{"places", "products", "items", "supplies"}

func trackerTools() []Def {
	return []Def{
		{App: "tracker", Tool: tool("tracker_find", "Tracker: find",
			"Search the maintenance tracker: places (buildings, rooms), products (kinds of equipment), items (where each thing is) and supplies (stock levels). Searches all four unless kind is given.",
			map[string]any{
				"q":    str("What to look for, e.g. \"projector\" or \"fellowship hall\"."),
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
				if strings.TrimSpace(a.Q) == "" && !a.Low {
					return mcp.Result{}, argError("q is needed (or low: true for supplies running low).")
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
	}
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
			ls = append(ls, join(" ", fmt.Sprintf("#%d", it.ID), it.Name, n, join(" ", it.Manufacturer, it.Model), atPlace("— at ", it.Place.Path)))
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
func atPlace(prefix, path string) string {
	if path == "" {
		return ""
	}
	return prefix + path
}
