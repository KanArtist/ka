package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// RestorePreserved re-applies an operation that a sync could not replay
// (PRD §10.4: nothing is lost silently; §13.2: a "restore" action). The
// operation is read from the system comment that preserved it and applied
// again now, attributed to the caller. Its preconditions are checked
// against the current ticket, so a claim someone else holds still fails.
func (s *Service) RestorePreserved(ctx context.Context, c Caller, id, comment string) (*WriteOut, error) {
	s.ix.catchUp()
	id = strings.ToUpper(id)
	s.ix.mu.RLock()
	r, err := s.rec(id)
	var body string
	if err == nil {
		for _, ce := range r.Comments {
			if ce.Name == comment && ce.Comment != nil {
				if ce.Comment.Type != "system" || ce.Comment.Author != SystemActor {
					s.ix.mu.RUnlock()
					return nil, Errf(CodeInvalid, "only comments ka wrote when it preserved an operation can be restored", "comment %s is not a preserved operation", comment)
				}
				body = ce.Comment.Body
			}
		}
	}
	s.ix.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if body == "" {
		return nil, Errf(CodeNotFound, "list the ticket's comments", "no comment %s on %s", comment, id)
	}
	o, err := preservedOp(body)
	if err != nil {
		return nil, err
	}
	if o.ID != id {
		return nil, Errf(CodeInvalid, "", "the preserved operation is for %s, not %s", o.ID, id)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	st, _, err := s.storeOf(&id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	stamp(o, c, now)
	if o.Comment != nil {
		// A fresh file, so the restored comment cannot collide with the
		// original name; the text and its author stay as written.
		o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
		if o.Comment.Created == "" {
			o.Comment.Created = now
		}
	}
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	w := writeOut(id, res)
	w.Comment = o.CommentName
	return w, nil
}

// preservedOp extracts the operation JSON from a preserved-operation comment.
func preservedOp(body string) (*Op, error) {
	const open = "```json\n"
	i := strings.Index(body, open)
	if i < 0 {
		return nil, Errf(CodeInvalid, "this comment preserves a conflicting value, not an operation; copy the value into the ticket instead", "nothing to restore")
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "\n```")
	if j < 0 {
		return nil, Errf(CodeInvalid, "", "the preserved operation is cut off")
	}
	var o Op
	if err := json.Unmarshal([]byte(rest[:j]), &o); err != nil || o.Op == "" || o.ID == "" {
		return nil, Errf(CodeInvalid, "", "the preserved operation does not parse")
	}
	return &o, nil
}
