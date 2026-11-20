package api

import (
	"context"

	"github.com/KanArtist/ka/internal/engine"
)

// ---- initiatives and epics ---------------------------------------------------

func (s *Server) hierarchyRoutes() {
	reg(s, get("getTree", "/tree", "An initiative or epic with every ticket below it and progress rollups; without id, the project's initiatives and top-level epics (ka_tree)", "tickets"),
		func(ctx context.Context, in *struct {
			ID         string `query:"id" doc:"An initiative, epic or any parent ticket (ID or reference)."`
			Project    string `query:"project" doc:"Without id: the project (default: the workspace's; else every project)."`
			Depth      int    `query:"depth" minimum:"0" doc:"Levels of children to show below each top node; 0 means all."`
			HideClosed bool   `query:"hide_closed" doc:"Leave out closed tickets (rollups still count them)."`
		}) (*body[*engine.TreeOut], error) {
			out, err := s.svc.Tree(engine.TreeIn{ID: in.ID, Project: in.Project, Depth: in.Depth, HideClosed: in.HideClosed})
			return &body[*engine.TreeOut]{out}, err
		})
	reg(s, post("initiativeFromLabel", "/initiatives/from-label", "Turn a label into an initiative over the tickets carrying it (ka initiative from-label)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Body engine.FromLabelIn
		}) (*body[*engine.FromLabelOut], error) {
			out, err := s.svc.InitiativeFromLabel(ctx, s.caller(in.CallerHeaders), in.Body)
			return &body[*engine.FromLabelOut]{out}, err
		})
}
