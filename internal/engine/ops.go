package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// Op is one replayable operation (PRD §7.9, §10.2). Everything needed to
// re-apply it against a different upstream state is inside the value,
// including timestamps and generated names, so a replay is deterministic.
type Op struct {
	Op string `json:"op"` // create, update, claim, release, transition, comment, check
	ID string `json:"id"`
	At string `json:"at"`

	// Identity travels in commit trailers, not in Ka-Op.
	Actor      string `json:"-"`
	OnBehalfOf string `json:"-"`
	Session    string `json:"-"`

	New    *kaspec.Ticket                `json:"new,omitempty"`    // create
	Set    map[string]*string            `json:"set,omitempty"`    // update: scalar fields; nil clears
	Fields map[string]*kaspec.FieldValue `json:"fields,omitempty"` // update: custom fields; nil clears
	Add    map[string][]string           `json:"add,omitempty"`    // update: labels, blocked_by, related, links, acceptance
	Remove map[string][]string           `json:"remove,omitempty"` // update
	Body   *string                       `json:"body,omitempty"`   // update

	Steal  bool   `json:"steal,omitempty"`  // claim
	Status string `json:"status,omitempty"` // claim (start status), release, transition

	Comment     *kaspec.Comment `json:"comment,omitempty"` // comment, transition (resolution)
	CommentName string          `json:"comment_name,omitempty"`

	Check []CheckItem `json:"check,omitempty"`

	// IfMatch, when set, requires ticket.md to still have this blob SHA
	// (BlobSHA). It guards editor saves against lost updates; it is checked
	// only on first application and never travels in Ka-Op.
	IfMatch string `json:"-"`
}

// CheckItem marks one acceptance item, by index or by text.
type CheckItem struct {
	Index *int   `json:"index,omitempty"`
	Text  string `json:"text,omitempty"`
	Done  bool   `json:"done"`
}

// Encode renders the Ka-Op trailer value.
func (o *Op) Encode() string {
	b, _ := json.Marshal(o)
	return o.Op + " " + string(b)
}

// DecodeOp parses a Ka-Op trailer value.
func DecodeOp(s string) (*Op, error) {
	_, js, ok := strings.Cut(s, " ")
	if !ok {
		return nil, fmt.Errorf("malformed Ka-Op %q", s)
	}
	var o Op
	if err := json.Unmarshal([]byte(js), &o); err != nil {
		return nil, fmt.Errorf("malformed Ka-Op: %v", err)
	}
	return &o, nil
}

// BlobSHA is git's SHA-1 object ID for a file's bytes. It identifies a
// version of ticket.md for If-Match.
func BlobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// change is one file written (or removed, when Data is nil) by an operation.
type change struct {
	Path string // relative to the store root, slash-separated
	Data []byte
}

// applyEnv is what an operation needs from the world besides the files.
type applyEnv interface {
	root() string
	// project returns the project directory (relative to the store root) and
	// effective config for a ticket ID's prefix in this store.
	project(id string) (dir string, cfg *kaspec.Config, err error)
	// ref reports whether a referenced ID exists, and whether its prefix
	// belongs to the workspace at all (unknown prefixes are external).
	ref(id string) (exists, known bool)
}

// loadedTicket is a ticket read from disk.
type loadedTicket struct {
	dir      string // relative to the store root
	t        *kaspec.Ticket
	raw      []byte
	comments []string // comment filenames, sorted
}

func readTicket(env applyEnv, id string) (*loadedTicket, *kaspec.Config, error) {
	pdir, cfg, err := env.project(id)
	if err != nil {
		return nil, nil, err
	}
	dir := pdir + "/tickets/" + id
	raw, err := os.ReadFile(filepath.Join(env.root(), filepath.FromSlash(dir), "ticket.md"))
	if err != nil {
		return nil, cfg, Errf(CodeNotFound, "check the ID with ka_search", "ticket %s does not exist", id)
	}
	t, ds := kaspec.ParseTicket(dir+"/ticket.md", raw)
	if t == nil || kaspec.HasErrors(ds, false) {
		msg := "ticket.md does not parse"
		for _, d := range ds {
			if d.Severity == kaspec.SevError {
				msg = d.String()
				break
			}
		}
		return nil, cfg, Errf("invalid_ticket", "run `ka check` and repair the file (or `ka fix`)", "%s: %s", id, msg)
	}
	lt := &loadedTicket{dir: dir, t: t, raw: raw}
	ents, _ := os.ReadDir(filepath.Join(env.root(), filepath.FromSlash(dir), "comments"))
	for _, e := range ents {
		if kaspec.CommentFileRe.MatchString(e.Name()) {
			lt.comments = append(lt.comments, e.Name())
		}
	}
	sort.Strings(lt.comments)
	return lt, cfg, nil
}

// apply runs an operation against the store's working tree and returns the
// files to write and a commit subject. It checks the operation's
// preconditions against the current state, so the same function serves both
// first application and replay after a rebase.
func apply(env applyEnv, o *Op) ([]change, string, error) {
	if o.Op == "create" {
		return applyCreate(env, o)
	}
	lt, cfg, err := readTicket(env, o.ID)
	if err != nil {
		return nil, "", err
	}
	t := lt.t
	if o.IfMatch != "" && o.IfMatch != BlobSHA(lt.raw) {
		return nil, "", Errf(CodeConflict, "reload the ticket, reapply your edit and save again", "%s changed since you loaded it", o.ID)
	}
	if t.MovedTo != "" {
		return nil, "", Errf(CodeMoved, "operate on "+t.MovedTo+" instead", "%s was moved to %s", o.ID, t.MovedTo)
	}
	before := string(kaspec.FormatTicket(t))
	var subject string
	var extra []change

	switch o.Op {
	case "update":
		subject, err = applyUpdate(env, cfg, t, o)
	case "claim":
		if t.Claim != nil && t.Claim.By != o.Actor {
			stale := claimStale(t.Claim, cfg, o.At)
			if !(stale && o.Steal) {
				e := Errf(CodeClaimLost, "pick another ticket (ka_next), or pass steal_stale once the claim is stale", "%s is claimed by %s since %s", o.ID, t.Claim.By, t.Claim.At).
					With("holder", t.Claim.By).With("since", t.Claim.At)
				if stale {
					e.Fix = "the claim is stale: retry with steal_stale=true to take it"
				}
				return nil, "", e
			}
		}
		if cat := category(cfg, t.Status); cat == "completed" || cat == "canceled" {
			return nil, "", Errf("ticket_closed", "reopen it first with ka_transition", "%s is %s", o.ID, t.Status)
		}
		t.Claim = &kaspec.Claim{By: o.Actor, Session: o.Session, At: o.At}
		t.Assignee = o.Actor
		if o.Status != "" {
			t.Status = o.Status
		}
		subject = fmt.Sprintf("%s: claimed by %s", o.ID, o.Actor)
	case "release":
		if t.Claim == nil {
			return nil, "", Errf(CodeNotClaimed, "", "%s is not claimed", o.ID)
		}
		if t.Claim.By != o.Actor && !o.Steal {
			return nil, "", Errf(CodeNotYourClaim, "only the holder can release; pass force to override", "%s is claimed by %s", o.ID, t.Claim.By).With("holder", t.Claim.By)
		}
		if t.Assignee == t.Claim.By {
			t.Assignee = ""
		}
		t.Claim = nil
		if o.Status != "" {
			t.Status = o.Status
		}
		subject = fmt.Sprintf("%s: released by %s", o.ID, o.Actor)
	case "transition":
		st, ok := cfg.StatusByName(o.Status)
		if !ok {
			return nil, "", unknownStatus(cfg, o.Status)
		}
		from := t.Status
		t.Status = o.Status
		if st.Category != "started" {
			t.Claim = nil
		}
		subject = fmt.Sprintf("%s: %s → %s", o.ID, from, o.Status)
	case "comment":
		subject = fmt.Sprintf("%s: %s", o.ID, commentSubject(o.Comment))
	case "check":
		if err := applyChecks(t, o); err != nil {
			return nil, "", err
		}
		done := 0
		for _, a := range t.Acceptance {
			if a.Done {
				done++
			}
		}
		subject = fmt.Sprintf("%s: acceptance %d/%d", o.ID, done, len(t.Acceptance))
	default:
		return nil, "", fmt.Errorf("unknown operation %q", o.Op)
	}
	if err != nil {
		return nil, "", err
	}

	if o.Comment != nil {
		if o.CommentName == "" {
			return nil, "", fmt.Errorf("operation has a comment without a name")
		}
		if slices.Contains(lt.comments, o.CommentName) {
			return nil, "", Errf(CodeIDTaken, "", "comment %s already exists", o.CommentName)
		}
		c := *o.Comment
		if c.Answers != "" && !slices.Contains(lt.comments, c.Answers) {
			return nil, "", Errf(CodeNoQuestion, "", "%s has no comment %s to answer", o.ID, c.Answers)
		}
		if c.Supersedes != "" && !slices.Contains(lt.comments, c.Supersedes) {
			return nil, "", Errf(CodeNotFound, "", "%s has no comment %s", o.ID, c.Supersedes)
		}
		if c.Retracts != "" && !slices.Contains(lt.comments, c.Retracts) {
			return nil, "", Errf(CodeNotFound, "", "%s has no comment %s", o.ID, c.Retracts)
		}
		data := kaspec.FormatComment(&c)
		if _, ds := kaspec.ParseComment("comment", data); kaspec.HasErrors(ds, false) {
			return nil, "", invalidFrom(ds)
		}
		if cfg.CommentsMaxBytes > 0 && int64(len(data)) > cfg.CommentsMaxBytes {
			return nil, "", Errf(CodeInvalid, "shorten it or attach the detail as a file", "comment is %d bytes; the limit is %d", len(data), cfg.CommentsMaxBytes)
		}
		extra = append(extra, change{Path: lt.dir + "/comments/" + o.CommentName, Data: data})
	}

	// Only touch ticket.md if something in it changed.
	if string(kaspec.FormatTicket(t)) != before {
		if t.Updated < o.At {
			t.Updated = o.At
		}
		data := kaspec.FormatTicket(t)
		if err := validateTicket(env, cfg, lt.dir, t, data); err != nil {
			return nil, "", err
		}
		extra = append(extra, change{Path: lt.dir + "/ticket.md", Data: data})
	}
	subject = refSubject(o.ID, t, subject)
	if len(extra) == 0 {
		return nil, subject, nil
	}
	return extra, subject, nil
}

// refSubject starts a commit subject with the ticket's reference
// (JK-7QK3M-compiler-warnings: ...) instead of its bare ID (spec §10).
func refSubject(id string, t *kaspec.Ticket, subject string) string {
	if rest, ok := strings.CutPrefix(subject, id+":"); ok {
		return kaspec.Ref(id, kaspec.DisplaySlug(t)) + ":" + rest
	}
	return subject
}

func applyChecks(t *kaspec.Ticket, o *Op) error {
	for _, it := range o.Check {
		i := -1
		if it.Index != nil {
			i = *it.Index
		} else {
			i = slices.IndexFunc(t.Acceptance, func(a kaspec.AcceptanceItem) bool { return a.Text == it.Text })
			if i < 0 {
				i = slices.IndexFunc(t.Acceptance, func(a kaspec.AcceptanceItem) bool {
					return strings.Contains(strings.ToLower(a.Text), strings.ToLower(it.Text))
				})
			}
		}
		if len(t.Acceptance) == 0 {
			return Errf(CodeInvalid, "add items with ka_update add.acceptance", "%s has no acceptance items", o.ID)
		}
		if i < 0 || i >= len(t.Acceptance) {
			return Errf(CodeInvalid, "use an index from 0 to "+fmt.Sprint(len(t.Acceptance)-1)+" or the item's text", "%s has no acceptance item %s", o.ID, describeItem(it))
		}
		t.Acceptance[i].Done = it.Done
	}
	return nil
}

func describeItem(it CheckItem) string {
	if it.Index != nil {
		return fmt.Sprintf("#%d", *it.Index)
	}
	return fmt.Sprintf("%q", it.Text)
}

func commentSubject(c *kaspec.Comment) string {
	switch c.Type {
	case "question":
		if c.To != "" {
			return "question for " + c.To
		}
		return "question"
	case "answer":
		return "answer"
	case "approval":
		if c.Approved != nil && *c.Approved {
			return "approved"
		}
		return "rejected"
	default:
		return c.Type + " comment"
	}
}

func applyCreate(env applyEnv, o *Op) ([]change, string, error) {
	pdir, cfg, err := env.project(o.ID)
	if err != nil {
		return nil, "", err
	}
	dir := pdir + "/tickets/" + o.ID
	if _, err := os.Stat(filepath.Join(env.root(), filepath.FromSlash(dir))); err == nil {
		return nil, "", Errf(CodeIDTaken, "", "%s already exists", o.ID)
	}
	t := *o.New
	t.ID = o.ID
	data := kaspec.FormatTicket(&t)
	if err := validateTicket(env, cfg, dir, &t, data); err != nil {
		return nil, "", err
	}
	return []change{{Path: dir + "/ticket.md", Data: data}}, refSubject(o.ID, &t, fmt.Sprintf("%s: created %q", o.ID, t.Title)), nil
}

// setKeys are the ticket fields ka_update can set as scalars.
var setKeys = []string{"title", "slug", "priority", "kind", "assignee", "owner", "parent", "duplicate_of", "milestone", "due"}

// listKeys are the list fields ka_update can add to and remove from.
var listKeys = []string{"labels", "blocked_by", "related", "links", "acceptance"}

func applyUpdate(env applyEnv, cfg *kaspec.Config, t *kaspec.Ticket, o *Op) (string, error) {
	var parts []string
	keys := make([]string, 0, len(o.Set))
	for k := range o.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ""
		if o.Set[k] != nil {
			v = *o.Set[k]
		}
		switch k {
		case "title":
			t.Title = v
		case "slug":
			if v != "" && !cfg.Slugs {
				return "", slugsOff()
			}
			t.Slug = v
		case "priority":
			t.Priority = v
		case "kind":
			t.Kind = v
		case "assignee":
			t.Assignee = v
		case "owner":
			t.Owner = v
		case "parent":
			t.Parent = v
		case "duplicate_of":
			t.DuplicateOf = v
		case "milestone":
			t.Milestone = v
		case "due":
			t.Due = v
		case "status":
			return "", Errf(CodeInvalid, "use ka_transition to change status", "status cannot be set by update")
		default:
			return "", Errf(CodeInvalid, "settable fields: "+strings.Join(setKeys, ", ")+"; custom fields go in fields", "%q cannot be set", k)
		}
		if v == "" {
			parts = append(parts, "-"+k)
		} else if k == "title" || k == "slug" {
			parts = append(parts, "title")
		} else {
			parts = append(parts, k+"="+v)
		}
	}
	fkeys := make([]string, 0, len(o.Fields))
	for k := range o.Fields {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	for _, k := range fkeys {
		if o.Fields[k] == nil {
			delete(t.Fields, k)
			parts = append(parts, "-fields."+k)
			continue
		}
		if t.Fields == nil {
			t.Fields = map[string]kaspec.FieldValue{}
		}
		t.Fields[k] = *o.Fields[k]
		parts = append(parts, "fields."+k)
	}
	for _, k := range sortedKeys(o.Add) {
		for _, v := range o.Add[k] {
			if err := listEdit(t, k, v, true); err != nil {
				return "", err
			}
			parts = append(parts, "+"+k+" "+v)
		}
	}
	for _, k := range sortedKeys(o.Remove) {
		for _, v := range o.Remove[k] {
			if err := listEdit(t, k, v, false); err != nil {
				return "", err
			}
			parts = append(parts, "-"+k+" "+v)
		}
	}
	if len(o.Check) > 0 {
		if err := applyChecks(t, o); err != nil {
			return "", err
		}
		parts = append(parts, "acceptance")
	}
	if o.Body != nil {
		t.Body = *o.Body
		parts = append(parts, "description")
	}
	s := strings.Join(parts, ", ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return fmt.Sprintf("%s: %s", o.ID, s), nil
}

func listEdit(t *kaspec.Ticket, key, v string, add bool) error {
	edit := func(l []string) []string {
		if add {
			if !slices.Contains(l, v) {
				l = append(l, v)
			}
			return l
		}
		return slices.DeleteFunc(l, func(s string) bool { return s == v })
	}
	switch key {
	case "labels":
		t.Labels = edit(t.Labels)
	case "blocked_by":
		t.BlockedBy = edit(t.BlockedBy)
	case "related":
		t.Related = edit(t.Related)
	case "links":
		t.Links = edit(t.Links)
	case "acceptance":
		i := slices.IndexFunc(t.Acceptance, func(a kaspec.AcceptanceItem) bool { return a.Text == v })
		if add && i < 0 {
			t.Acceptance = append(t.Acceptance, kaspec.AcceptanceItem{Text: v})
		} else if !add && i >= 0 {
			t.Acceptance = slices.Delete(t.Acceptance, i, i+1)
		}
	default:
		return Errf(CodeInvalid, "list fields: "+strings.Join(listKeys, ", "), "%q is not a list field", key)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func category(cfg *kaspec.Config, status string) string {
	if st, ok := cfg.StatusByName(status); ok {
		return st.Category
	}
	return ""
}

func claimStale(c *kaspec.Claim, cfg *kaspec.Config, now string) bool {
	if c == nil || cfg.ClaimsStaleAfter <= 0 {
		return false
	}
	at, err1 := parseTS(c.At)
	n, err2 := parseTS(now)
	if err1 != nil || err2 != nil {
		return false
	}
	return n.Sub(at) > cfg.ClaimsStaleAfter
}

func unknownStatus(cfg *kaspec.Config, s string) *Error {
	names := cfg.StatusNames()
	e := Errf(CodeUnknownStatus, "use one of: "+strings.Join(names, ", "), "%q is not a status", s)
	if n := nearest(s, names); n != "" {
		e.Fix = "did you mean " + n + "? Statuses: " + strings.Join(names, ", ")
	}
	return e
}

func invalidFrom(ds []kaspec.Diagnostic) *Error {
	for _, d := range ds {
		if d.Severity == kaspec.SevError {
			return &Error{Code: CodeInvalid, Message: d.Message, Fix: d.Fix}
		}
	}
	return &Error{Code: CodeInvalid, Message: "invalid"}
}

// validateTicket applies the write-time rules: the file must parse cleanly
// and every value must belong to the project's vocabulary (L4).
func validateTicket(env applyEnv, cfg *kaspec.Config, dir string, t *kaspec.Ticket, data []byte) error {
	if _, ds := kaspec.ParseTicket(dir+"/ticket.md", data); kaspec.HasErrors(ds, false) {
		return invalidFrom(ds)
	}
	if _, ok := cfg.StatusByName(t.Status); !ok {
		return unknownStatus(cfg, t.Status)
	}
	if t.Kind != "" && !slices.Contains(cfg.Kinds, t.Kind) {
		e := Errf(CodeUnknownKind, "use one of: "+strings.Join(cfg.Kinds, ", "), "%q is not a kind", t.Kind)
		if n := nearest(t.Kind, cfg.Kinds); n != "" {
			e.Fix = "did you mean " + n + "? Kinds: " + strings.Join(cfg.Kinds, ", ")
		}
		return e
	}
	if cfg.LabelsClosed {
		for _, l := range t.Labels {
			if !slices.Contains(cfg.LabelsKnown, l) {
				return Errf(CodeUnknownLabel, "labels are closed in this project; use one of: "+strings.Join(cfg.LabelsKnown, ", "), "%q is not a known label", l)
			}
		}
	}
	for _, k := range sortedKeys(t.Fields) {
		def, ok := cfg.Fields[k]
		if !ok {
			fix := "declare it under fields: in .ka/config.yml"
			if len(cfg.FieldOrder) > 0 {
				fix = "declared fields: " + strings.Join(cfg.FieldOrder, ", ")
			}
			return Errf(CodeUnknownField, fix, "custom field %q is not declared", k)
		}
		if err := kaspec.ValidateFieldValue(def, t.Fields[k]); err != nil {
			return Errf(CodeInvalidField, describeField(k, def), "fields.%s %v", k, err)
		}
	}
	refs := map[string][]string{"parent": nonEmpty(t.Parent), "blocked_by": t.BlockedBy, "related": t.Related, "duplicate_of": nonEmpty(t.DuplicateOf)}
	for _, k := range []string{"parent", "blocked_by", "related", "duplicate_of"} {
		for _, r := range refs[k] {
			if r == t.ID {
				return Errf(CodeInvalid, "", "%s cannot reference itself in %s", t.ID, k)
			}
			if exists, known := env.ref(r); known && !exists {
				return Errf(CodeDanglingRef, "check the ID with ka_search", "%s: %s does not exist", k, r)
			}
		}
	}
	return nil
}

func describeField(k string, def kaspec.FieldDef) string {
	if def.Type == "enum" {
		return k + " is one of: " + strings.Join(def.Values, ", ")
	}
	return k + " has type " + def.Type
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// nearest suggests the closest candidate for a typo.
func nearest(s string, cands []string) string {
	best, bestD := "", 3
	ls := strings.ToLower(s)
	for _, c := range cands {
		if strings.Contains(c, ls) && len(ls) >= 3 {
			return c
		}
		if d := lev(ls, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func lev(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func slugsOff() error {
	return Errf(CodeInvalid, "turn slugs on with `slugs: true` in the project's config, after every client of the store runs ka "+kaspec.SlugsMinKa+" or newer and the store sets requires_ka (spec §4.3)",
		"this project does not store slugs yet")
}
