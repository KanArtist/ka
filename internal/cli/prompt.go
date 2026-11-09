package cli

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/KanArtist/ka/internal/engine"
)

// Interactive prompts for missing arguments (PRD §14: when arguments are
// missing and stdin is a TTY, commands prompt). Prompts go to stderr, so
// stdout stays clean; they never appear with --json or when stdin is a
// pipe, which keeps agents and scripts on the exact-command path.

// canPrompt reports whether the command may ask the user.
func canPrompt(x *IO, c *common) bool {
	if c != nil && c.json {
		return false
	}
	return x.Interactive || (isTTY(x.In) && isTerminal(x.Err))
}

type prompter struct {
	x *IO
	r *bufio.Reader
}

func newPrompter(x *IO) *prompter {
	if x.reader == nil {
		x.reader = bufio.NewReader(x.In)
	}
	return &prompter{x: x, r: x.reader}
}

// line asks for one line; def is used for an empty answer. ok is false at
// end of input (Ctrl-D).
func (p *prompter) line(label, def string) (string, bool) {
	if def != "" {
		fmt.Fprintf(p.x.Err, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.x.Err, "%s: ", label)
	}
	s, err := p.r.ReadString('\n')
	s = strings.TrimSpace(s)
	if err != nil && s == "" {
		fmt.Fprintln(p.x.Err)
		return "", false
	}
	if s == "" {
		s = def
	}
	return s, true
}

// required asks until it gets a non-empty answer.
func (p *prompter) required(label string) (string, bool) {
	for {
		s, ok := p.line(label, "")
		if !ok || s != "" {
			return s, ok
		}
	}
}

// choose lists numbered options and returns the index picked (by number or
// by name). def is the index used for an empty answer (-1: none).
func (p *prompter) choose(label string, opts []string, def int) (int, bool) {
	for i, o := range opts {
		fmt.Fprintf(p.x.Err, "  %d) %s\n", i+1, o)
	}
	d := ""
	if def >= 0 && def < len(opts) {
		d = strconv.Itoa(def + 1)
	}
	for {
		s, ok := p.line(label, d)
		if !ok {
			return -1, false
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(opts) {
			return n - 1, true
		}
		for i, o := range opts {
			if strings.EqualFold(s, o) || strings.EqualFold(s, strings.Fields(o)[0]) {
				return i, true
			}
		}
		fmt.Fprintf(p.x.Err, "  pick 1-%d\n", len(opts))
	}
}

// text reads several lines, ending at an empty line or end of input.
func (p *prompter) text(label string) string {
	fmt.Fprintf(p.x.Err, "%s (end with an empty line):\n", label)
	var lines []string
	for {
		s, err := p.r.ReadString('\n')
		s = strings.TrimRight(s, "\r\n")
		if s == "" {
			break
		}
		lines = append(lines, s)
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// promptNew fills in a ticket to create. It returns an exit code to stop
// with, or -1 to go on and create it.
func promptNew(ctx context.Context, x *IO, c *common, in *engine.CreateIn) int {
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	sch, err := svc.Schema(caller, "")
	if err != nil {
		return fail(x, c, err)
	}
	p := newPrompter(x)
	title, ok := p.required("Title")
	if !ok {
		return 1
	}
	in.Title = title
	var writable []engine.ProjectSchema
	for _, pr := range sch.Projects {
		if !pr.ReadOnly && !pr.Archived {
			writable = append(writable, pr)
		}
	}
	if in.Project == "" && sch.DefaultProject == "" && len(writable) > 1 {
		names := make([]string, len(writable))
		for i, pr := range writable {
			names[i] = pr.Key
		}
		i, ok := p.choose("Project", names, 0)
		if !ok {
			return 1
		}
		in.Project = names[i]
	}
	key := in.Project
	if key == "" {
		key = sch.DefaultProject
	}
	var proj *engine.ProjectSchema
	for i := range writable {
		if writable[i].Key == key || (key == "" && len(writable) == 1) {
			proj = &writable[i]
		}
	}
	if in.Kind == "" && proj != nil && len(proj.Kinds) > 0 {
		opts := append([]string{"(none)"}, proj.Kinds...)
		i, ok := p.choose("Kind", opts, 0)
		if !ok {
			return 1
		}
		if i > 0 {
			in.Kind = opts[i]
		}
	}
	if in.Priority == "" {
		opts := []string{"(none)", "P0 urgent", "P1 high", "P2 normal", "P3 someday"}
		i, ok := p.choose("Priority", opts, 0)
		if !ok {
			return 1
		}
		if i > 0 {
			in.Priority = strings.Fields(opts[i])[0]
		}
	}
	if in.Body == "" {
		in.Body = p.text("Description (markdown, optional)")
	}
	return -1
}

// promptMove asks for a missing status, and for the resolution and summary
// a close needs.
func promptMove(x *IO, svc *engine.Service, caller engine.Caller, id, status, res, summary string) (string, string, string, bool) {
	d, err := svc.Get(engine.GetIn{ID: id})
	if err != nil {
		return status, res, summary, true // let the engine report it
	}
	sch, err := svc.Schema(caller, d.Project)
	if err != nil || len(sch.Projects) == 0 {
		return status, res, summary, true
	}
	p := newPrompter(x)
	statuses := sch.Projects[0].Statuses
	if status == "" {
		fmt.Fprintf(x.Err, "%s is %s. Move it to:\n", d.ID, d.Status)
		opts := make([]string, len(statuses))
		for i, s := range statuses {
			opts[i] = s.Name + " (" + s.Category + ")"
		}
		i, ok := p.choose("Status", opts, -1)
		if !ok {
			return "", "", "", false
		}
		status = statuses[i].Name
	}
	closing := false
	for _, s := range statuses {
		if s.Name == status && (s.Category == "completed" || s.Category == "canceled") {
			closing = true
			if res == "" && s.Category == "canceled" {
				i, ok := p.choose("Resolution", sch.Resolutions, 0)
				if !ok {
					return "", "", "", false
				}
				res = sch.Resolutions[i]
			}
		}
	}
	if closing && summary == "" {
		summary = p.text("Summary: what was done, or why not")
		if summary == "" {
			fmt.Fprintln(x.Err, "ka: closing needs a summary")
			return "", "", "", false
		}
	}
	return status, res, summary, true
}

// promptAnswer shows the open question and asks for a choice or text.
func promptAnswer(ctx context.Context, x *IO, c *common, in *engine.AnswerIn) int {
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	d, err := svc.Get(engine.GetIn{ID: in.ID})
	if err != nil {
		return fail(x, c, err)
	}
	var q *engine.CommentOut
	for i := range d.OpenQuestions {
		if in.Question == "" || d.OpenQuestions[i].Name == in.Question {
			q = &d.OpenQuestions[i]
		}
	}
	if q == nil {
		return -1 // the engine says there is no open question
	}
	fmt.Fprintf(x.Err, "%s asks on %s:\n\n  %s\n\n", q.Author, d.ID, strings.ReplaceAll(strings.TrimSpace(q.Body), "\n", "\n  "))
	p := newPrompter(x)
	if len(q.Options) > 0 {
		opts := append(append([]string{}, q.Options...), "write an answer instead")
		i, ok := p.choose("Answer", opts, -1)
		if !ok {
			return 1
		}
		if i < len(q.Options) {
			in.Choice = &i
			in.Question = q.Name
			return -1
		}
	}
	text := p.text("Answer")
	if text == "" {
		fmt.Fprintln(x.Err, "ka: no answer given")
		return 1
	}
	in.Text, in.Question = text, q.Name
	return -1
}
