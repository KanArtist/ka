package engine

import (
	"context"
	"sort"
)

// Reads that the REST API exposes and the CLI and MCP get through other
// shapes (ka store ls, ka_schema).

// WorkspaceOut describes the workspace a service runs on.
type WorkspaceOut struct {
	Name           string   `json:"name,omitempty"`
	Source         string   `json:"source"` // the file or directory the workspace was found from
	DefaultProject string   `json:"default_project,omitempty"`
	Stores         []string `json:"stores"`
	Projects       []string `json:"projects"`
	You            string   `json:"you"`
}

// WorkspaceInfo describes the workspace.
func (s *Service) WorkspaceInfo() *WorkspaceOut {
	out := &WorkspaceOut{Source: s.WS.Source, DefaultProject: s.WS.Project, You: s.human, Projects: s.Projects(), Stores: []string{}}
	if s.WS.File != nil {
		out.Name = s.WS.File.Name
	}
	for _, st := range s.WS.Stores {
		out.Stores = append(out.Stores, st.Name)
	}
	return out
}

// StoreOut is one store and its sync state.
type StoreOut struct {
	SyncState
	Root          string `json:"root"`
	URL           string `json:"url,omitempty"`
	Managed       bool   `json:"managed"`
	ReadOnly      bool   `json:"read_only,omitempty"`
	PushMode      string `json:"push_mode" doc:"immediate, manual or debounce:<delay>"`
	FetchInterval string `json:"fetch_interval" doc:"How often the daemon fetches."`
}

// Stores lists the workspace's stores with their sync state.
func (s *Service) Stores(ctx context.Context) []StoreOut {
	out := make([]StoreOut, 0, len(s.WS.Stores))
	for _, st := range s.WS.Stores {
		out = append(out, StoreOut{SyncState: st.State(ctx), Root: st.Root, URL: st.URL, Managed: st.Managed, ReadOnly: st.ReadOnly,
			PushMode: st.PushSetting(), FetchInterval: st.fetchEvery().String()})
	}
	return out
}

// BoardOut is a saved board.
type BoardOut struct {
	ID          string   `json:"id" doc:"store/key: unique across the workspace."`
	Name        string   `json:"name" doc:"The key (file name without .yml)."`
	Title       string   `json:"title" doc:"The display name."`
	Store       string   `json:"store"`
	Description string   `json:"description,omitempty"`
	Filter      string   `json:"filter,omitempty"`
	GroupBy     string   `json:"group_by,omitempty"`
	Sort        string   `json:"sort,omitempty"`
	Swimlanes   string   `json:"swimlanes,omitempty"`
	Columns     []string `json:"columns,omitempty"`
	CardFields  []string `json:"card_fields,omitempty"`
}

// Boards lists saved boards from every store, namespaced by store (PRD
// §9.3: boards are a union; the same key in two stores is two boards).
func (s *Service) Boards() []BoardOut {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := []BoardOut{}
	for _, st := range s.ix.stores {
		ks := s.ix.ks[st]
		if ks == nil {
			continue
		}
		names := make([]string, 0, len(ks.Boards))
		for n := range ks.Boards {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if b := ks.Boards[n]; b != nil {
				out = append(out, boardOut(st, n, b))
			}
		}
	}
	return out
}

// ActorOut is one actor from the merged actor registry.
type ActorOut struct {
	Handle  string   `json:"handle"`
	Kind    string   `json:"kind"` // human or agent
	Name    string   `json:"name,omitempty"`
	Emails  []string `json:"emails,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Roles   []string `json:"roles,omitempty"`
}

// Actors lists humans and agents with the roles they hold.
func (s *Service) Actors() []ActorOut {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	a := s.ix.actors
	out := []ActorOut{}
	if a == nil {
		return out
	}
	roles := map[string][]string{}
	for role, members := range a.Roles {
		for _, m := range members {
			roles[m] = append(roles[m], role)
		}
	}
	for h, v := range a.Humans {
		out = append(out, ActorOut{Handle: h, Kind: "human", Name: v.Name, Emails: v.Emails, Aliases: v.Aliases, Roles: sorted(roles[h])})
	}
	for h, name := range a.Agents {
		out = append(out, ActorOut{Handle: h, Kind: "agent", Name: name, Roles: sorted(roles[h])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// ETag returns the blob SHA of a ticket's ticket.md as the index has it.
func (s *Service) ETag(id string) (string, error) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	r, err := s.rec(id)
	if err != nil {
		return "", err
	}
	return BlobSHA(r.Raw), nil
}

func sorted(v []string) []string {
	sort.Strings(v)
	return v
}
