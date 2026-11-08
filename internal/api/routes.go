package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/presence"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// Caller headers, accepted on every operation.
type CallerHeaders struct {
	Actor   string `header:"X-Ka-Actor" doc:"Act as this actor; default: the daemon's human. Agents use agent/<name> and are recorded as acting on behalf of the human."`
	Session string `header:"X-Ka-Session" doc:"A session ID, recorded in the Ka-Session trailer."`
}

func (s *Server) caller(h CallerHeaders) engine.Caller { return s.svc.CallerFor(h.Actor, h.Session) }

type TicketID struct {
	ID string `path:"id" doc:"A ticket ID, such as KA-6FVER." example:"KA-6FVER"`
}

type body[T any] struct {
	Body T
}

// Every handler's error goes through problem(), so engine codes reach the client.
func reg[I, O any](s *Server, op huma.Operation, h func(context.Context, *I) (*O, error)) {
	huma.Register(s.api, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := h(ctx, in)
		if err != nil {
			return nil, problem(err)
		}
		return out, nil
	})
}

func get(id, path, summary, tag string) huma.Operation {
	return huma.Operation{OperationID: id, Method: http.MethodGet, Path: "/api/v1" + path, Summary: summary, Tags: []string{tag}}
}

func post(id, path, summary, tag string) huma.Operation {
	return huma.Operation{OperationID: id, Method: http.MethodPost, Path: "/api/v1" + path, Summary: summary, Tags: []string{tag}}
}

// etag quotes a blob SHA as an entity tag; unquote accepts either form.
func etag(sha string) string { return `"` + sha + `"` }

func unquote(tag string) string {
	tag = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "W/"))
	return strings.Trim(tag, `"`)
}

func (s *Server) routes() {
	s.workspaceRoutes()
	s.ticketRoutes()
	s.questionRoutes()
	s.adminRoutes()
}

// ---- workspace -----------------------------------------------------------------

type storeList struct {
	Stores []engine.StoreOut `json:"stores"`
}

type projectList struct {
	Projects []engine.ProjectSchema `json:"projects"`
}

type boardList struct {
	Boards []engine.BoardOut `json:"boards"`
}

type boardTickets struct {
	Board   engine.BoardOut   `json:"board"`
	Tickets *engine.SearchOut `json:"tickets"`
}

type actorList struct {
	Actors []engine.ActorOut `json:"actors"`
}

func (s *Server) workspaceRoutes() {
	reg(s, get("getWorkspace", "/workspace", "The workspace: stores, projects, default project and the daemon's human", "workspace"),
		func(ctx context.Context, in *struct{}) (*body[*engine.WorkspaceOut], error) {
			return &body[*engine.WorkspaceOut]{s.svc.WorkspaceInfo()}, nil
		})
	reg(s, get("listStores", "/stores", "Stores with their sync state", "workspace"),
		func(ctx context.Context, in *struct{}) (*body[storeList], error) {
			return &body[storeList]{storeList{s.svc.Stores(ctx)}}, nil
		})
	reg(s, get("getSchema", "/schema", "Projects, statuses, kinds, labels, fields, priorities and actors (ka_schema)", "workspace"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Project string `query:"project" doc:"Limit to one project."`
		}) (*body[*engine.SchemaOut], error) {
			out, err := s.svc.Schema(s.caller(in.CallerHeaders), in.Project)
			return &body[*engine.SchemaOut]{out}, err
		})
	reg(s, get("listProjects", "/projects", "Projects and their vocabularies", "workspace"),
		func(ctx context.Context, in *struct{ CallerHeaders }) (*body[projectList], error) {
			out, err := s.svc.Schema(s.caller(in.CallerHeaders), "")
			if err != nil {
				return nil, err
			}
			return &body[projectList]{projectList{out.Projects}}, nil
		})
	reg(s, get("getProject", "/projects/{key}", "One project and its vocabulary", "workspace"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Key string `path:"key"`
		}) (*body[engine.ProjectSchema], error) {
			out, err := s.svc.Schema(s.caller(in.CallerHeaders), in.Key)
			if err != nil {
				return nil, err
			}
			if len(out.Projects) == 0 {
				return nil, engine.Errf(engine.CodeUnknownProject, "list projects with GET /api/v1/projects", "no project %q", in.Key)
			}
			return &body[engine.ProjectSchema]{out.Projects[0]}, nil
		})
	reg(s, get("listBoards", "/boards", "Saved boards", "workspace"),
		func(ctx context.Context, in *struct{}) (*body[boardList], error) {
			return &body[boardList]{boardList{s.svc.Boards()}}, nil
		})
	reg(s, get("getBoard", "/boards/{name}", "A saved board and the tickets its filter matches", "workspace"),
		func(ctx context.Context, in *struct {
			Name  string `path:"name"`
			Limit int    `query:"limit" minimum:"0" doc:"Maximum tickets; 0 means all."`
		}) (*body[boardTickets], error) {
			for _, b := range s.svc.Boards() {
				if b.Name != in.Name {
					continue
				}
				var sorts []string
				if b.Sort != "" {
					sorts = []string{b.Sort}
				}
				limit := in.Limit
				if limit == 0 {
					limit = -1
				}
				res, err := s.svc.Search(engine.SearchIn{Query: b.Filter, Sort: sorts, Limit: limit, All: true})
				if err != nil {
					return nil, err
				}
				return &body[boardTickets]{boardTickets{b, res}}, nil
			}
			return nil, engine.Errf(engine.CodeNotFound, "list boards with GET /api/v1/boards", "no board %q", in.Name)
		})
	reg(s, get("listActors", "/actors", "Humans and agents from the actor registries, with roles", "workspace"),
		func(ctx context.Context, in *struct{}) (*body[actorList], error) {
			return &body[actorList]{actorList{s.svc.Actors()}}, nil
		})
}

// ---- tickets -------------------------------------------------------------------

type searchIn struct {
	Q     string   `query:"q" doc:"A ka query: project:jk kind:bug -label:infra is:ready sort:-updated words" example:"is:ready"`
	Sort  []string `query:"sort" doc:"Sort keys (priority, -updated, created, ...)."`
	Limit int      `query:"limit" minimum:"-1" doc:"Maximum results; 0 means the default (20), -1 means all."`
	All   bool     `query:"all" doc:"Include closed tickets when the query does not say."`
}

func (s *Server) search(ctx context.Context, in *searchIn) (*body[*engine.SearchOut], error) {
	out, err := s.svc.Search(engine.SearchIn{Query: in.Q, Sort: in.Sort, Limit: in.Limit, All: in.All})
	return &body[*engine.SearchOut]{out}, err
}

type ticketOut struct {
	ETag string `header:"ETag" doc:"The blob SHA of ticket.md, for If-Match."`
	Body *engine.Detail
}

type ticketPatch struct {
	Set    map[string]any      `json:"set,omitempty" doc:"Scalar and custom fields to set; null clears. Keys: title, status, priority, kind, assignee, owner, parent, duplicate_of, fields.<name>."`
	Add    map[string][]string `json:"add,omitempty" doc:"Values to add to labels, blocked_by, related, links or acceptance."`
	Remove map[string][]string `json:"remove,omitempty" doc:"Values to remove."`
	Body   *string             `json:"body,omitempty" doc:"The new markdown body. Requires If-Match."`
}

type batchUpdateResult struct {
	Results []*engine.WriteOut `json:"results"`
	Error   *engine.Error      `json:"error,omitempty" doc:"The first failure; earlier updates were still applied."`
}

type batchCreate struct {
	Tickets []engine.CreateIn `json:"tickets" minItems:"1"`
}

type commentList struct {
	Comments []engine.CommentOut `json:"comments"`
}

type checkResult struct {
	*engine.WriteOut
	Acceptance string `json:"acceptance" doc:"Done/total, such as 2/3."`
}

type rawTicket struct {
	ETag        string `header:"ETag"`
	ContentType string `header:"Content-Type"`
	Body        []byte
}

func (s *Server) ticketRoutes() {
	reg(s, get("listTickets", "/tickets", "Search tickets", "tickets"), s.search)
	reg(s, get("search", "/search", "Search tickets (the same as GET /tickets)", "tickets"), s.search)

	op := post("createTicket", "/tickets", "Create a ticket", "tickets")
	op.DefaultStatus = http.StatusCreated
	reg(s, op, func(ctx context.Context, in *struct {
		CallerHeaders
		Body engine.CreateIn
	}) (*body[*engine.CreateOut], error) {
		out, err := s.svc.Create(ctx, s.caller(in.CallerHeaders), []engine.CreateIn{in.Body})
		if err != nil && (out == nil || len(out.Created) == 0) {
			return nil, err
		}
		return &body[*engine.CreateOut]{out}, nil
	})
	op = post("createTickets", "/tickets/batch", "Create several tickets: one commit each, one push (ka_create)", "tickets")
	op.DefaultStatus = http.StatusCreated
	reg(s, op, func(ctx context.Context, in *struct {
		CallerHeaders
		Body batchCreate
	}) (*body[*engine.CreateOut], error) {
		out, err := s.svc.Create(ctx, s.caller(in.CallerHeaders), in.Body.Tickets)
		if err != nil && (out == nil || len(out.Created) == 0) {
			return nil, err
		}
		return &body[*engine.CreateOut]{out}, nil
	})

	reg(s, get("getTicket", "/tickets/{id}", "A ticket with relations and a comment digest", "tickets"),
		func(ctx context.Context, in *struct {
			TicketID
			Include []string `query:"include" enum:"comments,children,relations" doc:"comments: every comment instead of the digest."`
		}) (*ticketOut, error) {
			d, err := s.svc.Get(engine.GetIn{ID: in.ID, Include: in.Include})
			if err != nil {
				return nil, err
			}
			tag, _ := s.svc.ETag(d.ID)
			return &ticketOut{ETag: etag(tag), Body: d}, nil
		})

	reg(s, huma.Operation{OperationID: "updateTicket", Method: http.MethodPatch, Path: "/api/v1/tickets/{id}", Tags: []string{"tickets"},
		Summary:     "Change fields",
		Description: "Field operations replay safely and need no precondition. Replacing the body requires If-Match with the ETag from GET."},
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			IfMatch string `header:"If-Match" doc:"The ETag of the version being edited; required when body is set."`
			Body    ticketPatch
		}) (*body[*engine.WriteOut], error) {
			if in.Body.Body != nil && in.IfMatch == "" {
				return nil, newProblem(codePreconditionRequired, "GET the ticket and send its ETag as If-Match", "replacing the body requires If-Match")
			}
			outs, err := s.svc.Update(ctx, s.caller(in.CallerHeaders), []engine.UpdateIn{{ID: in.ID, Set: in.Body.Set, Add: in.Body.Add, Remove: in.Body.Remove, Body: in.Body.Body, IfMatch: unquote(in.IfMatch)}})
			if err != nil {
				return nil, err
			}
			return &body[*engine.WriteOut]{outs[0]}, nil
		})
	reg(s, huma.Operation{OperationID: "updateTickets", Method: http.MethodPatch, Path: "/api/v1/tickets", Tags: []string{"tickets"},
		Summary: "Change fields on several tickets: one commit each, one push (ka_update)"},
		func(ctx context.Context, in *struct {
			CallerHeaders
			Body struct {
				Updates []engine.UpdateIn `json:"updates" minItems:"1"`
			}
		}) (*body[batchUpdateResult], error) {
			outs, err := s.svc.Update(ctx, s.caller(in.CallerHeaders), in.Body.Updates)
			if err != nil && len(outs) == 0 {
				return nil, err
			}
			if outs == nil {
				outs = []*engine.WriteOut{}
			}
			return &body[batchUpdateResult]{batchUpdateResult{outs, engine.AsError(err)}}, nil
		})

	reg(s, get("getTicketSource", "/tickets/{id}/source", "The raw ticket.md", "tickets"),
		func(ctx context.Context, in *TicketID) (*rawTicket, error) {
			if _, err := s.svc.Get(engine.GetIn{ID: in.ID}); err != nil {
				return nil, err
			}
			p, _, err := s.svc.TicketPath(in.ID)
			if err != nil {
				return nil, err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, engine.Errf(engine.CodeNotFound, "", "%s: %v", in.ID, err)
			}
			return &rawTicket{ETag: etag(engine.BlobSHA(data)), ContentType: "text/markdown; charset=utf-8", Body: data}, nil
		})
	reg(s, huma.Operation{OperationID: "replaceTicketSource", Method: http.MethodPut, Path: "/api/v1/tickets/{id}/source", Tags: []string{"tickets"},
		Summary:     "Save an edited ticket.md",
		Description: "The edit is validated and recorded as an update of the changed fields. Requires If-Match. Closing a ticket this way is refused: use transition."},
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			IfMatch string `header:"If-Match" required:"true"`
			RawBody []byte `contentType:"text/markdown"`
		}) (*body[*engine.WriteOut], error) {
			if in.IfMatch == "" {
				return nil, newProblem(codePreconditionRequired, "GET the source and send its ETag as If-Match", "saving ticket.md requires If-Match")
			}
			out, err := s.svc.ReplaceTicket(ctx, s.caller(in.CallerHeaders), in.ID, in.RawBody, unquote(in.IfMatch))
			return &body[*engine.WriteOut]{out}, err
		})

	reg(s, post("claimTicket", "/tickets/{id}/claim", "Claim a ticket; always pushes (ka_claim)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			Body struct {
				Steal bool `json:"steal,omitempty" doc:"Take over a stale claim."`
			}
		}) (*body[*engine.ClaimOut], error) {
			out, err := s.svc.Claim(ctx, s.caller(in.CallerHeaders), in.ID, in.Body.Steal)
			return &body[*engine.ClaimOut]{out}, err
		})
	reg(s, post("releaseTicket", "/tickets/{id}/release", "Release a claim (ka_release)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			Body struct {
				Status string `json:"status,omitempty" doc:"Move to this status as well."`
				Force  bool   `json:"force,omitempty" doc:"Release someone else's claim."`
			}
		}) (*body[*engine.WriteOut], error) {
			out, err := s.svc.Release(ctx, s.caller(in.CallerHeaders), in.ID, in.Body.Status, in.Body.Force)
			return &body[*engine.WriteOut]{out}, err
		})
	reg(s, post("transitionTicket", "/tickets/{id}/transition", "Change status; closing requires a summary (ka_transition)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			Body struct {
				Status     string `json:"status" minLength:"1"`
				Resolution string `json:"resolution,omitempty"`
				Summary    string `json:"summary,omitempty" doc:"Required when closing; committed as a resolution comment."`
			}
		}) (*body[*engine.WriteOut], error) {
			out, err := s.svc.Transition(ctx, s.caller(in.CallerHeaders), engine.TransitionIn{ID: in.ID, Status: in.Body.Status, Resolution: in.Body.Resolution, Summary: in.Body.Summary})
			return &body[*engine.WriteOut]{out}, err
		})
	reg(s, get("listComments", "/tickets/{id}/comments", "Every comment on a ticket, oldest first", "tickets"),
		func(ctx context.Context, in *TicketID) (*body[commentList], error) {
			d, err := s.svc.Get(engine.GetIn{ID: in.ID, Include: []string{"comments"}})
			if err != nil {
				return nil, err
			}
			cs := d.Comments
			if cs == nil {
				cs = []engine.CommentOut{}
			}
			return &body[commentList]{commentList{cs}}, nil
		})
	op = post("createComment", "/tickets/{id}/comments", "Add a note, progress or decision comment (ka_comment)", "tickets")
	op.DefaultStatus = http.StatusCreated
	reg(s, op, func(ctx context.Context, in *struct {
		CallerHeaders
		TicketID
		Body struct {
			Type       string `json:"type,omitempty" enum:"note,progress,decision" doc:"Default: note."`
			Body       string `json:"body" minLength:"1"`
			Supersedes string `json:"supersedes,omitempty" doc:"A decision comment's filename this one replaces."`
			Retracts   string `json:"retracts,omitempty"`
		}
	}) (*body[*engine.WriteOut], error) {
		out, err := s.svc.Comment(ctx, s.caller(in.CallerHeaders), engine.CommentIn{ID: in.ID, Type: in.Body.Type, Body: in.Body.Body, Supersedes: in.Body.Supersedes, Retracts: in.Body.Retracts})
		return &body[*engine.WriteOut]{out}, err
	})
	reg(s, post("checkAcceptance", "/tickets/{id}/acceptance", "Mark acceptance items done or not done (ka_check)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			TicketID
			Body struct {
				Items []engine.CheckItem `json:"items" minItems:"1"`
			}
		}) (*body[checkResult], error) {
			w, acc, err := s.svc.CheckItems(ctx, s.caller(in.CallerHeaders), engine.CheckIn{ID: in.ID, Items: in.Body.Items})
			if err != nil {
				return nil, err
			}
			return &body[checkResult]{checkResult{w, acc}}, nil
		})
	reg(s, post("next", "/next", "The best ready ticket, claimed unless peek (ka_next)", "tickets"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Body engine.NextIn
		}) (*body[*engine.NextOut], error) {
			out, err := s.svc.Next(ctx, s.caller(in.CallerHeaders), in.Body)
			return &body[*engine.NextOut]{out}, err
		})
}

// ---- questions -----------------------------------------------------------------

// MaxAskWait caps ka_ask's wait over HTTP.
const MaxAskWait = 5 * time.Minute

func (s *Server) questionRoutes() {
	op := post("askQuestion", "/tickets/{id}/questions", "Ask a human a question or for an approval (ka_ask)", "questions")
	op.DefaultStatus = http.StatusCreated
	reg(s, op, func(ctx context.Context, in *struct {
		CallerHeaders
		TicketID
		Body struct {
			Question    string   `json:"question" minLength:"1"`
			Options     []string `json:"options,omitempty"`
			To          string   `json:"to,omitempty" doc:"An actor or role; default: the ticket's owner."`
			Kind        string   `json:"kind,omitempty" enum:"question,approval"`
			WaitSeconds int      `json:"wait_seconds,omitempty" minimum:"0" maximum:"300" doc:"Block until answered, up to this long."`
		}
	}) (*body[*engine.AskOut], error) {
		out, err := s.svc.Ask(ctx, s.caller(in.CallerHeaders), engine.AskIn{ID: in.ID, Question: in.Body.Question, Options: in.Body.Options,
			To: in.Body.To, Kind: in.Body.Kind, Wait: min(time.Duration(in.Body.WaitSeconds)*time.Second, MaxAskWait)})
		return &body[*engine.AskOut]{out}, err
	})
	op = post("answerQuestion", "/tickets/{id}/answers", "Answer a question, or approve or reject a request", "questions")
	op.DefaultStatus = http.StatusCreated
	reg(s, op, func(ctx context.Context, in *struct {
		CallerHeaders
		TicketID
		Body struct {
			Question string `json:"question,omitempty" doc:"The question comment's filename; default: the newest open question."`
			Choice   *int   `json:"choice,omitempty" doc:"The chosen option's index."`
			Text     string `json:"text,omitempty"`
			Approved *bool  `json:"approved,omitempty" doc:"For approvals: true approves, false rejects."`
		}
	}) (*body[*engine.WriteOut], error) {
		out, err := s.svc.Answer(ctx, s.caller(in.CallerHeaders), engine.AnswerIn{ID: in.ID, Question: in.Body.Question, Choice: in.Body.Choice, Text: in.Body.Text, Approved: in.Body.Approved})
		return &body[*engine.WriteOut]{out}, err
	})
	reg(s, get("getInbox", "/inbox", "Answers to the caller's questions and open questions for the caller (ka_inbox)", "questions"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Since string `query:"since" doc:"Only answers newer than this: RFC 3339 or a duration such as 24h."`
		}) (*body[*engine.InboxOut], error) {
			since := in.Since
			if d, err := kaspec.ParseDuration(since); err == nil && since != "" {
				since = time.Now().Add(-d).UTC().Format("2006-01-02T15:04:05Z")
			}
			return &body[*engine.InboxOut]{s.svc.Inbox(s.caller(in.CallerHeaders), since)}, nil
		})
}

// ---- sync and maintenance --------------------------------------------------------

type checkReport struct {
	Errors      int                 `json:"errors"`
	Warnings    int                 `json:"warnings"`
	Diagnostics []kaspec.Diagnostic `json:"diagnostics"`
}

type daemonInfo struct {
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	Started   string `json:"started"`
	Workspace string `json:"workspace"`
}

type presenceList struct {
	Sessions []presence.Session `json:"sessions" doc:"Live agent sessions on this daemon, most recently active first. Ephemeral: never written to git."`
}

func (s *Server) adminRoutes() {
	reg(s, get("listPresence", "/presence", "Connected agent sessions and their last action (live state)", "workspace"),
		func(ctx context.Context, in *struct{}) (*body[presenceList], error) {
			return &body[presenceList]{presenceList{s.opts.Presence.List()}}, nil
		})
	reg(s, get("getSync", "/sync", "Each store's sync state, without fetching", "sync"),
		func(ctx context.Context, in *struct{}) (*body[storeList], error) {
			return &body[storeList]{storeList{s.svc.Stores(ctx)}}, nil
		})
	reg(s, post("sync", "/sync", "Fetch, replay and push every store (ka_sync)", "sync"),
		func(ctx context.Context, in *struct {
			Body struct {
				Force bool `json:"force,omitempty"`
			}
		}) (*body[*engine.SyncOut], error) {
			return &body[*engine.SyncOut]{s.svc.Sync(ctx, in.Body.Force)}, nil
		})
	op := post("gitHook", "/hooks/git", "Fetch now: for forge push webhooks, or a client that wants fresh data", "sync")
	op.DefaultStatus = http.StatusAccepted
	op.Description = "Queues an immediate fetch of one store (?store=) or all of them; the changes arrive as events. Any body is ignored."
	reg(s, op, func(ctx context.Context, in *struct {
		Store string `query:"store" doc:"The store's workspace name; default: every store."`
	}) (*struct{}, error) {
		s.svc.Kick(in.Store)
		return &struct{}{}, nil
	})
	reg(s, get("getActivity", "/activity", "Recent history from git, as events (newest first)", "events"),
		func(ctx context.Context, in *struct {
			Limit  int    `query:"limit" minimum:"0" maximum:"500" doc:"Default 100."`
			Before string `query:"before" doc:"Only entries older than this (RFC 3339): the last event's at, to page back."`
		}) (*body[*engine.ActivityOut], error) {
			out, err := s.svc.Activity(ctx, engine.ActivityIn{Limit: in.Limit, Before: in.Before})
			return &body[*engine.ActivityOut]{out}, err
		})
	reg(s, get("resolveLink", "/links/resolve", "Resolve a code link to a local path and a forge URL (ka_resolve_link)", "workspace"),
		func(ctx context.Context, in *struct {
			Link string `query:"link" required:"true" example:"repo://jk/src/cache.go#L10"`
		}) (*body[*engine.LinkOut], error) {
			out, err := s.svc.ResolveLink(in.Link)
			return &body[*engine.LinkOut]{out}, err
		})
	reg(s, get("check", "/check", "Validate every store against the Store Spec (ka check)", "sync"),
		func(ctx context.Context, in *struct{}) (*body[checkReport], error) {
			ds := s.svc.Validate(time.Now())
			out := checkReport{Diagnostics: ds}
			if out.Diagnostics == nil {
				out.Diagnostics = []kaspec.Diagnostic{}
			}
			for _, d := range ds {
				switch d.Severity {
				case kaspec.SevError:
					out.Errors++
				case kaspec.SevWarning:
					out.Warnings++
				}
			}
			return &body[checkReport]{out}, nil
		})
	reg(s, post("fix", "/fix", "Repair timestamps, actor aliases and formatting; one commit per kind (ka fix)", "sync"),
		func(ctx context.Context, in *struct {
			CallerHeaders
			Body struct {
				DryRun bool `json:"dry_run,omitempty"`
			}
		}) (*body[*engine.FixOut], error) {
			out, err := s.svc.Fix(ctx, s.caller(in.CallerHeaders), in.Body.DryRun)
			return &body[*engine.FixOut]{out}, err
		})
	reg(s, get("getDaemon", "/daemon", "The daemon's version, PID and address", "daemon"),
		func(ctx context.Context, in *struct{}) (*body[daemonInfo], error) {
			s.mu.RLock()
			addr := s.addr
			s.mu.RUnlock()
			return &body[daemonInfo]{daemonInfo{s.opts.Version, os.Getpid(), addr, s.started.UTC().Format(time.RFC3339), s.svc.WS.Source}}, nil
		})
	op = post("shutdownDaemon", "/daemon/shutdown", "Stop the daemon (ka daemon stop)", "daemon")
	op.DefaultStatus = http.StatusAccepted
	reg(s, op, func(ctx context.Context, in *struct{}) (*struct{}, error) {
		if s.opts.Shutdown == nil {
			return nil, engine.Errf(engine.CodeInvalid, "", "this server cannot be stopped over HTTP")
		}
		go s.opts.Shutdown()
		return &struct{}{}, nil
	})
}
