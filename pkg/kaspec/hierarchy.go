package kaspec

import (
	"fmt"
	"slices"
	"strings"
)

// The container kinds (spec §4.5). Initiatives and epics group work; they
// are tickets like any other, linked through parent.
const (
	KindInitiative = "initiative"
	KindEpic       = "epic"
)

// ContainersMinKa is the first ka release that knows the container kinds.
const ContainersMinKa = "1.3.0"

// IsContainerKind reports whether a kind names a container.
func IsContainerKind(kind string) bool { return kind == KindInitiative || kind == KindEpic }

// HierarchyProblem checks the nesting rules (spec §4.5) for a ticket of
// kind child that has a parent of kind parent. It returns "" when the
// nesting is allowed, else what is wrong:
//
//   - an initiative has no parent;
//   - an epic's parent, when it has one, is an initiative;
//   - any other ticket's parent may be an epic, an initiative or another
//     ticket.
//
// Together these mean a container is never the child of a non-container.
func HierarchyProblem(child, parent string) string {
	switch child {
	case KindInitiative:
		return "an initiative cannot have a parent"
	case KindEpic:
		if parent != KindInitiative {
			return fmt.Sprintf("an epic's parent must be an initiative, not %s", KindPhrase(parent))
		}
	}
	return ""
}

// HierarchyFix is the fix text for a HierarchyProblem.
func HierarchyFix(child string) string {
	switch child {
	case KindInitiative:
		return "clear parent; initiatives are top level"
	case KindEpic:
		return "set parent to an initiative, or clear it"
	}
	return ""
}

func KindPhrase(k string) string {
	switch {
	case k == "":
		return "a ticket without a kind"
	case strings.ContainsRune("aeiou", rune(k[0])):
		return "an " + k
	}
	return "a " + k
}

// ContainersGuarded reports whether a store requires a ka that knows the
// container kinds.
func ContainersGuarded(sf *StoreFile) bool {
	return sf != nil && VersionAtLeastRelease(sf.RequiresKa, ContainersMinKa)
}

type hnode struct {
	s   *Store
	te  *TicketEntry
	cat string // status category; "" when unknown

	work, workOpen, open int // descendants (containers only)
}

// hierarchy checks the container rules across the workspace: nesting
// (hierarchy_invalid), stores holding containers that older ka cannot read
// (containers_unguarded), and container statuses that disagree with their
// descendants (container_status).
func (c *checker) hierarchy() {
	all := map[string]*hnode{}
	var ids []string
	unguarded := map[*Store]string{}
	for _, s := range c.w.Stores {
		for _, k := range s.Keys {
			pr := s.Projects[k]
			for _, id := range pr.IDs {
				te := pr.Tickets[id]
				t := te.Ticket
				if t == nil || t.ID != id {
					continue
				}
				n := &hnode{s: s, te: te}
				if pr.Config != nil {
					if st, ok := pr.Config.StatusByName(t.Status); ok {
						n.cat = st.Category
					}
				}
				all[id] = n
				ids = append(ids, id)
				if IsContainerKind(t.Kind) && unguarded[s] == "" && !ContainersGuarded(s.File) {
					unguarded[s] = id
				}
			}
		}
	}
	for _, s := range c.w.Stores {
		if id := unguarded[s]; id != "" {
			c.diags = append(c.diags, diag("containers_unguarded", s.p(".ka/store.yml"), 0,
				"once every client runs ka "+ContainersMinKa+" or newer, run `ka store require "+ContainersMinKa+"` (or add `requires_ka: "+ContainersMinKa+"` to .ka/store.yml)",
				"%s is %s, but the store does not require ka %s; older ka cannot read initiatives and epics", id, KindPhrase(all[id].te.Ticket.Kind), ContainersMinKa))
		}
	}
	slices.Sort(ids)
	lookup := func(id string) *hnode {
		if id == "" {
			return nil
		}
		if n, ok := all[id]; ok {
			return n
		}
		if cid, _, ok := c.resolve(id); ok { // a former prefix
			return all[cid]
		}
		return nil
	}
	closed := func(cat string) bool { return cat == "completed" || cat == "canceled" }
	for _, id := range ids {
		n := all[id]
		t := n.te.Ticket
		if p := lookup(t.Parent); p != nil && p != n {
			if why := HierarchyProblem(t.Kind, p.te.Ticket.Kind); why != "" {
				c.diags = append(c.diags, diag("hierarchy_invalid", n.s.p(n.te.Dir+"/ticket.md"), lineOf(t, "parent"), HierarchyFix(t.Kind),
					"%s: parent %s is %s", why, t.Parent, KindPhrase(p.te.Ticket.Kind)))
			}
		}
		// Count this ticket in every container above it. The walk stops at a
		// ticket already seen, so a parent cycle (reported as cycle) ends it.
		work := !IsContainerKind(t.Kind)
		open := n.cat != "" && !closed(n.cat)
		seen := map[*hnode]bool{n: true}
		for p := lookup(t.Parent); p != nil && !seen[p]; p = lookup(p.te.Ticket.Parent) {
			seen[p] = true
			if !IsContainerKind(p.te.Ticket.Kind) {
				continue
			}
			if work {
				p.work++
				if open {
					p.workOpen++
				}
			}
			if open {
				p.open++
			}
		}
	}
	for _, id := range ids {
		n := all[id]
		t := n.te.Ticket
		if !IsContainerKind(t.Kind) || n.cat == "" {
			continue
		}
		tp := n.s.p(n.te.Dir + "/ticket.md")
		switch {
		case !closed(n.cat) && n.work > 0 && n.workOpen == 0:
			c.diags = append(c.diags, diag("container_status", tp, lineOf(t, "status"), "close it, or file the work that remains under it",
				"%s is %s, but all %d tickets under it are closed", KindPhrase(t.Kind), t.Status, n.work))
		case closed(n.cat) && n.open > 0:
			c.diags = append(c.diags, diag("container_status", tp, lineOf(t, "status"), "reopen it, or close or move the open tickets under it",
				"%s is %s, but %d tickets under it are still open", KindPhrase(t.Kind), t.Status, n.open))
		}
	}
}
