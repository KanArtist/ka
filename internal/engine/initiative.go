package engine

import (
	"context"
	"slices"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// FromLabelIn is the input of InitiativeFromLabel.
type FromLabelIn struct {
	Label     string `json:"label" doc:"The label to turn into an initiative."`
	Project   string `json:"project,omitempty" doc:"The project to convert, which also holds the initiative (default: the workspace's)."`
	Title     string `json:"title,omitempty" doc:"The initiative's title (default: the label). An initiative with this title in the project is reused."`
	KeepLabel bool   `json:"keep_label,omitempty" doc:"Leave the label on the converted tickets (default: remove it)."`
	DryRun    bool   `json:"dry_run,omitempty" doc:"Report what would change without writing."`
}

// Actions in a FromLabelOut.
const (
	LabelParented = "parented" // the ticket's parent is now the initiative
	LabelViaEpic  = "via_epic" // the ticket's epic is (now) under the initiative
	LabelAlready  = "already"  // already under the initiative
	LabelSkipped  = "skipped"  // left alone; see reason
	LabelEpic     = "epic"     // an unlabelled epic put under the initiative for its labelled tickets
)

// FromLabelTicket is what happened to one ticket.
type FromLabelTicket struct {
	ID           string `json:"id"`
	Ref          string `json:"ref"`
	Title        string `json:"title"`
	Kind         string `json:"kind,omitempty"`
	Action       string `json:"action" enum:"parented,via_epic,already,skipped,epic" doc:"parented: its parent is now the initiative; via_epic: its epic is under the initiative; already: it was under it; skipped: left alone (see reason); epic: an epic put under the initiative for its labelled tickets."`
	Via          string `json:"via,omitempty" doc:"For via_epic: the epic."`
	Reason       string `json:"reason,omitempty" doc:"For skipped: why."`
	LabelRemoved bool   `json:"label_removed,omitempty"`
}

// FromLabelOut reports InitiativeFromLabel.
type FromLabelOut struct {
	Initiative Ref               `json:"initiative" doc:"The initiative (no id in a dry run that would create it)."`
	Created    bool              `json:"created" doc:"A new initiative was (or would be) created; false when one with the title existed."`
	Label      string            `json:"label"`
	Tickets    []FromLabelTicket `json:"tickets"`
	DryRun     bool              `json:"dry_run,omitempty"`
	Pushed     bool              `json:"pushed"`
	Local      bool              `json:"local_only,omitempty"`
	// PushPending: committed; a debounced push follows within seconds.
	PushPending bool   `json:"push_pending,omitempty"`
	Error       *Error `json:"error,omitempty" doc:"The first failure; earlier changes were still made."`
}

// InitiativeFromLabel turns a label into an initiative: it creates the
// initiative (or reuses one with the same title in the project) and puts
// every ticket of the project carrying the label under it. A labelled ticket
// without a parent gets the initiative as parent; one whose parent is an
// epic without a parent puts that epic under the initiative instead; any
// other parent is left alone and reported. The label is removed from the
// converted tickets unless KeepLabel. Everything is ordinary create and
// update operations (one commit each, one push), so it replays safely, and
// running it again changes nothing.
func (s *Service) InitiativeFromLabel(ctx context.Context, c Caller, in FromLabelIn) (*FromLabelOut, error) {
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return nil, Errf(CodeInvalid, "pass the label to convert", "label is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = label
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()

	// Sync first, so an initiative another clone created for this label is
	// reused rather than duplicated. Two clones converting in the same
	// moment can still both create one; ka check does not flag that, and the
	// later run's tickets can be moved with ka edit --set parent=...
	s.ix.mu.RLock()
	pi, err := s.resolveProject(in.Project)
	s.ix.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !in.DryRun && s.mayPush(pi.Store) {
		if remote, _ := pi.Store.upstream(ctx); remote != "" {
			pi.Store.Sync(ctx, s.hooks(pi.Store)) // offline: plan from what we have
		}
	}

	s.ix.mu.RLock()
	pi, out, plan, err := s.planFromLabel(c, in, label, title)
	s.ix.mu.RUnlock()
	if err != nil || in.DryRun {
		return out, err
	}

	st := pi.Store
	wrote := false
	fail := func(err error) (*FromLabelOut, error) {
		if !wrote {
			return nil, err
		}
		out.Error = AsError(err)
		return out, nil
	}
	if plan.create != nil {
		if _, err := s.do(ctx, st, plan.create, false); err != nil {
			return fail(err)
		}
		wrote = true
		out.Initiative.ID = plan.create.ID
		out.Initiative.Ref = kaspec.Ref(plan.create.ID, kaspec.DisplaySlug(plan.create.New))
	}
	for _, u := range plan.updates {
		if u.setParent {
			u.in.Set = map[string]any{"parent": out.Initiative.ID}
		}
		o, err := s.updateOp(c, pi, u.in)
		if err == nil {
			_, err = s.do(ctx, st, o, false)
		}
		if err != nil {
			return fail(err)
		}
		wrote = true
	}
	if !wrote {
		out.Pushed, out.Local = true, true
		return out, nil
	}
	res, err := s.push(ctx, st)
	if res != nil {
		out.Pushed, out.Local, out.PushPending = res.Pushed || !res.HasRemote, !res.HasRemote, res.PushPending
		if n, ok := res.Renamed[out.Initiative.ID]; ok {
			out.Initiative.ID = n
			out.Initiative.Ref = s.ix.refOf(n)
		}
	}
	if err != nil {
		out.Error = AsError(err)
	}
	return out, nil
}

type fromLabelPlan struct {
	create  *Op
	updates []fromLabelUpdate
}

type fromLabelUpdate struct {
	in        UpdateIn
	setParent bool // set parent to the initiative (its ID is known once created)
}

// planFromLabel works out what InitiativeFromLabel changes (index read lock held).
func (s *Service) planFromLabel(c Caller, in FromLabelIn, label, title string) (*ProjectInfo, *FromLabelOut, *fromLabelPlan, error) {
	pi, err := s.resolveProject(in.Project)
	if err != nil {
		return nil, nil, nil, err
	}
	if pi.Store.ReadOnly {
		return nil, nil, nil, Errf(CodeReadOnly, "add the store to your workspace to write to it", "project %s lives in a store this workspace only reads", pi.Key)
	}
	if !slices.Contains(pi.Cfg.Kinds, kaspec.KindInitiative) {
		return nil, nil, nil, Errf(CodeUnknownKind, "add initiative to the project's kinds (it is in the built-in defaults)", "project %s has no initiative kind", pi.Key)
	}
	if ks := s.ix.ks[pi.Store]; ks == nil || !kaspec.ContainersGuarded(ks.File) {
		return nil, nil, nil, unguardedErr(pi.Store.Name)
	}
	out := &FromLabelOut{Label: label, DryRun: in.DryRun, Tickets: []FromLabelTicket{}}
	plan := &fromLabelPlan{}

	// Reuse an initiative with this title, so running again changes nothing.
	var init *Rec
	var labelled []*Rec
	for _, id := range sortedKeys(s.ix.tickets) {
		r := s.ix.tickets[id]
		if r.Project != pi {
			continue
		}
		if r.T.Kind == kaspec.KindInitiative && strings.EqualFold(r.T.Title, title) {
			if init == nil || (closedCategory(category(pi.Cfg, init.T.Status)) && !closedCategory(category(pi.Cfg, r.T.Status))) {
				init = r
			}
		}
		if slices.ContainsFunc(r.T.Labels, func(l string) bool { return strings.EqualFold(l, label) }) {
			labelled = append(labelled, r)
		}
	}
	initID := ""
	if init != nil {
		initID = init.ID
		out.Initiative = init.ref()
	} else {
		out.Created = true
		o, err := s.createOp(c, pi, CreateIn{Title: title, Kind: kaspec.KindInitiative,
			Body: "Converted from the label `" + label + "` (`ka initiative from-label`)."})
		if err != nil {
			return nil, nil, nil, err
		}
		plan.create = o
		out.Initiative = Ref{Title: o.New.Title, Status: o.New.Status}
		if !in.DryRun {
			out.Initiative.ID = o.ID
		}
	}

	type edit struct {
		parent bool
		unlbl  string // the label as spelled on the ticket, to remove
	}
	edits := map[string]*edit{}
	var order []string
	want := func(id string) *edit {
		if ch, ok := edits[id]; ok {
			return ch
		}
		ch := &edit{}
		edits[id] = ch
		order = append(order, id)
		return ch
	}
	var epics []FromLabelTicket
	for _, r := range labelled {
		if r.ID == initID {
			continue // the initiative itself
		}
		item := FromLabelTicket{ID: r.ID, Ref: r.Ref(), Title: r.T.Title, Kind: r.T.Kind}
		p := s.ix.tickets[s.ix.canon(r.T.Parent)]
		switch {
		case r.T.Kind == kaspec.KindInitiative:
			item.Action, item.Reason = LabelSkipped, "it is an initiative itself"
		case r.T.Parent == "":
			item.Action = LabelParented
			want(r.ID).parent = true
		case initID != "" && p != nil && p.ID == initID:
			item.Action = LabelAlready
		case p != nil && p.T.Kind == kaspec.KindEpic && r.T.Kind != kaspec.KindEpic:
			item.Via = p.ID
			switch {
			case initID != "" && s.ix.canon(p.T.Parent) == initID:
				item.Action = LabelAlready
			case p.T.Parent != "":
				item.Action, item.Reason = LabelSkipped, "its epic "+p.Ref()+" is under "+s.ix.refOf(s.ix.canon(p.T.Parent))
			default:
				item.Action = LabelViaEpic
				if _, seen := edits[p.ID]; !seen && !slices.ContainsFunc(labelled, func(x *Rec) bool { return x == p }) {
					epics = append(epics, FromLabelTicket{ID: p.ID, Ref: p.Ref(), Title: p.T.Title, Kind: p.T.Kind, Action: LabelEpic})
				}
				want(p.ID).parent = true
			}
		default:
			item.Action, item.Reason = LabelSkipped, "it already has parent "+s.ix.refOf(s.ix.canon(r.T.Parent))
		}
		if item.Action != LabelSkipped && !in.KeepLabel {
			i := slices.IndexFunc(r.T.Labels, func(l string) bool { return strings.EqualFold(l, label) })
			want(r.ID).unlbl = r.T.Labels[i]
			item.LabelRemoved = true
		}
		out.Tickets = append(out.Tickets, item)
	}
	out.Tickets = append(out.Tickets, epics...)
	for _, id := range order {
		ch := edits[id]
		u := fromLabelUpdate{in: UpdateIn{ID: id}, setParent: ch.parent}
		if ch.unlbl != "" {
			u.in.Remove = map[string][]string{"labels": {ch.unlbl}}
		}
		plan.updates = append(plan.updates, u)
	}
	return pi, out, plan, nil
}
