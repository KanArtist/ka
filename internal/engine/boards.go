package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// BoardIn is a saved board to write (PRD §7.7). Boards are definitions
// only: a filter and how to lay it out, never ticket membership or order.
type BoardIn struct {
	Store       string   `json:"store" doc:"The store to save the board in."`
	Key         string   `json:"key" doc:"The file name without .yml: lowercase letters, digits, - and _."`
	Title       string   `json:"title" doc:"The display name (the file's name: field)."`
	Description string   `json:"description,omitempty"`
	Filter      string   `json:"filter,omitempty" doc:"A ka query."`
	GroupBy     string   `json:"group_by,omitempty"`
	Sort        string   `json:"sort,omitempty"`
	Swimlanes   string   `json:"swimlanes,omitempty"`
	Columns     []string `json:"columns,omitempty" doc:"The columns (statuses, when grouped by status) to show, in order."`
	CardFields  []string `json:"card_fields,omitempty"`
}

var boardKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// boardRef splits "store/key" (or a bare key, meaning the first store that
// has it) and finds the board.
func (s *Service) boardRef(ref string) (*Store, string, *kaspec.Board) {
	storeName, key, ok := strings.Cut(ref, "/")
	if !ok {
		key, storeName = ref, ""
	}
	for _, st := range s.ix.stores {
		if storeName != "" && st.Name != storeName {
			continue
		}
		if ks := s.ix.ks[st]; ks != nil {
			if b := ks.Boards[key]; b != nil {
				return st, key, b
			}
		}
	}
	return nil, key, nil
}

// SaveBoard writes boards/<key>.yml in a store and commits it, like any
// other change (PRD §13.2, saved boards).
func (s *Service) SaveBoard(ctx context.Context, c Caller, in BoardIn) (*BoardOut, error) {
	if !boardKeyRe.MatchString(in.Key) {
		return nil, Errf(CodeInvalid, "use lowercase letters, digits, - and _ (for example agents-in-flight)", "board key %q is not valid", in.Key)
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, Errf(CodeInvalid, "give the board a title", "a board needs a title")
	}
	if _, err := ParseQuery(in.Filter); err != nil {
		return nil, err
	}
	st, err := s.writableStore(in.Store)
	if err != nil {
		return nil, err
	}
	var kv []kaspec.KV
	kv = append(kv, kaspec.KV{K: "name", V: strings.TrimSpace(in.Title)}, kaspec.KV{K: "description", V: strings.TrimSpace(in.Description)},
		kaspec.KV{K: "filter", V: strings.TrimSpace(in.Filter)}, kaspec.KV{K: "group_by", V: in.GroupBy}, kaspec.KV{K: "sort", V: in.Sort},
		kaspec.KV{K: "swimlanes", V: in.Swimlanes}, kaspec.KV{K: "columns", V: strList(in.Columns)}, kaspec.KV{K: "card_fields", V: strList(in.CardFields)})
	data := []byte(kaspec.EmitYAML(kv))
	if _, ds := kaspec.ParseBoard("boards/"+in.Key+".yml", data); len(ds) > 0 {
		return nil, Errf(CodeInvalid, "", "the board does not validate: %s", ds[0].Message)
	}
	verb := "add"
	if _, err := os.Stat(filepath.Join(st.Root, "boards", in.Key+".yml")); err == nil {
		verb = "update"
	}
	if err := s.commitFile(ctx, c, st, "boards/"+in.Key+".yml", data, "ka: "+verb+" board "+in.Key); err != nil {
		return nil, err
	}
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	if _, _, b := s.boardRef(st.Name + "/" + in.Key); b != nil {
		out := boardOut(st, in.Key, b)
		return &out, nil
	}
	return nil, Errf(CodeGit, "run ka check", "board %s/%s was written but did not load", st.Name, in.Key)
}

// DeleteBoard removes a saved board ("store/key", or a key) with a commit.
func (s *Service) DeleteBoard(ctx context.Context, c Caller, ref string) error {
	s.ix.mu.RLock()
	st, key, b := s.boardRef(ref)
	s.ix.mu.RUnlock()
	if b == nil {
		return Errf(CodeNotFound, "list boards with GET /api/v1/boards", "no board %q", ref)
	}
	if _, err := s.writableStore(st.Name); err != nil {
		return err
	}
	return s.commitFile(ctx, c, st, "boards/"+key+".yml", nil, "ka: remove board "+key)
}

func (s *Service) writableStore(name string) (*Store, error) {
	var names []string
	for _, st := range s.WS.Stores {
		if st.Name == name {
			if st.ReadOnly {
				return nil, Errf(CodeReadOnly, "add the store to your workspace to write to it", "store %s is read-only here", name)
			}
			return st, nil
		}
		if !st.ReadOnly {
			names = append(names, st.Name)
		}
	}
	return nil, Errf(CodeInvalid, "stores: "+strings.Join(names, ", "), "no writable store %q", name)
}

// commitFile writes (or, with nil data, removes) one store file outside the
// ticket operations (boards), commits it with the actor's trailers, reloads
// the index and pushes per the store's push mode.
func (s *Service) commitFile(ctx context.Context, c Caller, st *Store, rel string, data []byte, subject string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	unlock, err := st.lock(ctx)
	if err != nil {
		return err
	}
	if err := st.guard(); err != nil {
		unlock()
		return err
	}
	p := filepath.Join(st.Root, filepath.FromSlash(rel))
	before := st.git.Head(ctx)
	if fi, lerr := os.Lstat(p); lerr == nil && !fi.Mode().IsRegular() {
		unlock()
		return irregular(p) // never write through a link
	}
	if data == nil {
		err = os.Remove(p)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		err = os.WriteFile(p, data, 0o644)
	}
	var sha string
	if err == nil {
		sha, err = st.commitPaths(ctx, c, subject, nil, []string{rel})
	}
	if err != nil {
		st.rollback(ctx, []string{st.repoPath(rel)})
		unlock()
		return err
	}
	if sha != "" {
		st.own(before, sha)
	}
	unlock()
	if sha == "" {
		return nil // nothing changed
	}
	s.ix.refresh(st, []string{rel})
	s.publish(Event{Type: EvStoreChanged, At: s.now(), Store: st.Name, Actor: c.Actor, Data: map[string]string{"what": "boards", "path": rel}})
	if _, err := s.pushStore(ctx, st); err != nil {
		return Errf(CodeGit, "the change is committed locally; ka sync retries the push", "push %s: %v", st.Name, err)
	}
	return nil
}

func strList(v []string) []any {
	out := make([]any, 0, len(v))
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func boardOut(st *Store, key string, b *kaspec.Board) BoardOut {
	return BoardOut{ID: st.Name + "/" + key, Name: key, Title: b.Name, Store: st.Name, Description: b.Description, Filter: b.Filter,
		GroupBy: b.GroupBy, Sort: b.Sort, Swimlanes: b.Swimlanes, Columns: b.Columns, CardFields: b.CardFields}
}
