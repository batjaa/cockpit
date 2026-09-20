package main

import (
	"context"
	"html/template"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type mapGraphPoint struct {
	GraphNode
	X, Y int
	URL  string
}
type mapGraphEdge struct{ X1, Y1, X2, Y2 int }
type mapMirrorPreview struct {
	Title, Location, Markdown, Error string
	MirrorStateView
}
type mapDecisionExport struct {
	DecisionView
	Location string
}
type MapExtras struct {
	MoveDestinations []WorkstreamListRow
	GraphPoints      []mapGraphPoint
	GraphEdges       []mapGraphEdge
	GraphHeight      int
	MirrorPreviews   []mapMirrorPreview
	DecisionExports  []mapDecisionExport
}

func mapViewFuncs() template.FuncMap {
	return template.FuncMap{
		"mapNav": mapNav,
		"mapDateTime": func(location *time.Location, t time.Time) string {
			if location == nil {
				location = time.Local
			}
			return t.In(location).Format("2006-01-02 15:04 MST")
		},
		"mapShortLabel": func(s string) string {
			r := []rune(s)
			if len(r) > 26 {
				return string(r[:25]) + "…"
			}
			return s
		},
		"mapActive": func(w *WorkstreamDetailView) bool { return w != nil && !w.Archived && w.Lifecycle == "active" },
	}
}

// mapNav preserves bookmarkable state on every server-rendered navigation.
// Empty values remove a key; changing selection drops child-specific cursors.
func mapNav(q MapQuery, changes ...string) string {
	v := url.Values{"workstream": {q.WorkstreamID}, "item": {q.ItemID}, "tab": {q.Tab}, "filter": {q.Filter}, "q": {q.Search}, "workstreams_cursor": {q.WorkstreamsCursor}, "category": {q.ItemsCategory}, "items_cursor": {q.ItemsCursor}, "timeline_cursor": {q.TimelineCursor}, "detached_cursor": {q.DetachedCursor}, "decisions_cursor": {q.DecisionsCursor}, "reasons_cursor": {q.ReasonsCursor}, "unfinished_cursor": {q.UnfinishedCursor}}
	for i := 0; i+1 < len(changes); i += 2 {
		if changes[i] == "workstream" && changes[i+1] != q.WorkstreamID {
			for _, k := range []string{"item", "category", "items_cursor", "timeline_cursor", "detached_cursor", "decisions_cursor", "reasons_cursor", "unfinished_cursor"} {
				v.Del(k)
			}
		}
		v.Set(changes[i], changes[i+1])
	}
	for key := range v {
		if v.Get(key) == "" {
			v.Del(key)
		}
	}
	return "/map?" + v.Encode()
}

func (s *server) prepareMapExtras(ctx context.Context, view MapPageView) MapExtras {
	x := MapExtras{GraphHeight: 240}
	if view.Selected == nil {
		return x
	}
	if view.SelectedItem != nil && !view.Selected.Archived && view.Selected.Lifecycle == "active" {
		if destinations, err := s.workstreamStore().ListMapPage(ctx, MapQuery{Filter: "active"}); err == nil {
			x.MoveDestinations = destinations.Workstreams.Rows
		}
	}
	// Group all bounded graph nodes into fixed category bands, independent of
	// which item-list page happens to be selected.
	points := map[string]mapGraphPoint{}
	y := 40
	for _, category := range categoryDefinitions {
		start := y
		for _, node := range view.Graph.Nodes {
			if node.ParentID != category.key {
				continue
			}
			p := mapGraphPoint{GraphNode: node, X: 500, Y: y, URL: mapNav(view.Query, "item", node.ID, "tab", "graph")}
			x.GraphPoints = append(x.GraphPoints, p)
			points[node.ID] = p
			y += 44
		}
		if y == start {
			y += 44
		}
		p := mapGraphPoint{GraphNode: GraphNode{ID: category.key, Label: category.label, Kind: "category", ParentID: view.Selected.ID}, X: 260, Y: (start + y - 44) / 2, URL: mapNav(view.Query, "tab", "overview", "category", category.key, "items_cursor", "") + "#category-" + category.key}
		x.GraphPoints = append(x.GraphPoints, p)
		points[p.ID] = p
		y += 32
	}
	root := mapGraphPoint{GraphNode: GraphNode{ID: view.Selected.ID, Label: view.Selected.Name, Kind: "workstream"}, X: 20, Y: (y - 32) / 2, URL: mapNav(view.Query, "item", "", "tab", "overview")}
	x.GraphPoints = append(x.GraphPoints, root)
	points[root.ID] = root
	for _, p := range x.GraphPoints {
		if parent, ok := points[p.ParentID]; ok {
			x.GraphEdges = append(x.GraphEdges, mapGraphEdge{parent.X + 200, parent.Y, p.X, p.Y})
		}
	}
	x.GraphHeight = y
	if view.Query.Tab != "obsidian" {
		return x
	}
	x.MirrorPreviews = append(x.MirrorPreviews, s.mapPreview(ctx, view.Selected.Name, view.Selected.Mirror, view.Selected.Revision))
	if item := view.SelectedItem; item != nil {
		x.MirrorPreviews = append(x.MirrorPreviews, s.mapPreview(ctx, item.Title, item.Mirror, item.Revision))
	}
	for _, decision := range view.Selected.Decisions {
		x.DecisionExports = append(x.DecisionExports, mapDecisionExport{DecisionView: decision, Location: s.mapMirrorLocation("decisions/" + decision.ID + ".md")})
	}
	return x
}

func (s *server) mapMirrorLocation(relative string) string {
	if s.mirror != nil && filepath.IsAbs(s.mirror.root) {
		return filepath.Join(s.mirror.root, filepath.FromSlash(relative))
	}
	return relative
}

func (s *server) mapPreview(ctx context.Context, title string, state MirrorStateView, revision int64) mapMirrorPreview {
	p := mapMirrorPreview{Title: title, MirrorStateView: state}
	doc, err := s.workstreamStore().RenderMirrorSnapshot(ctx, state.MirrorKey, revision)
	if err != nil {
		p.Error = "The record changed while preparing this preview. Reload to see the latest saved revision."
		return p
	}
	contents, err := formatMirrorDocument(doc)
	if err != nil {
		p.Error = "The Markdown preview could not be prepared. Your local record is saved."
		return p
	}
	p.Markdown = string(contents)
	p.Location = s.mapMirrorLocation(doc.RelativePath)
	// No filesystem read, write, application launch, or provider request occurs
	// here. This is the same formatter used by the background publisher.
	p.Location = strings.TrimSpace(p.Location)
	return p
}
