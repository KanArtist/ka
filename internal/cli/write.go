package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/render"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

func cmdNew(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("new", "-t TITLE [flags]", x)
	var in engine.CreateIn
	var labels, accept, blocked, links, fields multi
	fl.StringVar(&in.Title, "t", "", "title")
	fl.StringVar(&in.Title, "title", "", "title")
	fl.StringVar(&in.Project, "project", "", "project key")
	fl.StringVar(&in.Kind, "kind", "", "kind")
	fl.StringVar(&in.Priority, "priority", "", "P0-P3")
	fl.StringVar(&in.Status, "status", "", "status (default: the project's initial)")
	fl.StringVar(&in.Assignee, "assignee", "", "assignee")
	fl.StringVar(&in.Parent, "parent", "", "parent ticket")
	fl.StringVar(&in.Body, "m", "", "description (markdown; - reads stdin)")
	fl.StringVar(&in.Problem, "problem", "", "the Problem section")
	fl.StringVar(&in.Approach, "approach", "", "the Approach section")
	fl.Var(&labels, "label", "label (repeatable)")
	fl.Var(&accept, "accept", "acceptance criterion (repeatable)")
	fl.Var(&blocked, "blocked-by", "blocking ticket (repeatable)")
	fl.Var(&links, "link", "code link or URL (repeatable)")
	fl.Var(&fields, "field", "custom field NAME=VALUE (repeatable)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if in.Title == "" {
		in.Title = strings.Join(pos, " ")
	}
	if in.Title == "" {
		return usageErr(x, fl, "a title is required (-t)")
	}
	if in.Body == "-" {
		in.Body = readText(x, "-")
	}
	in.Labels, in.Acceptance, in.BlockedBy, in.Links = labels, accept, blocked, links
	if len(fields) > 0 {
		in.Fields = map[string]any{}
		for _, f := range fields {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				return usageErr(x, fl, "--field takes NAME=VALUE")
			}
			in.Fields[k] = v
		}
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Create(ctx, caller, []engine.CreateIn{in})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Created(out))
}

func cmdEdit(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("edit", "ID [--set K=V]... [--add K=V]... [--remove K=V]...", x)
	var sets, adds, removes multi
	fl.Var(&sets, "set", "set a field: title=..., priority=P1, fields.estimate=M; K= clears (repeatable)")
	fl.Var(&adds, "add", "add to a list: labels=cache, blocked_by=JK-..., acceptance=... (repeatable)")
	fl.Var(&removes, "remove", "remove from a list (repeatable)")
	body := fl.String("body", "", "replace the description (- reads stdin)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "edit needs one ID")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	id := strings.ToUpper(pos[0])
	if len(sets)+len(adds)+len(removes) == 0 && *body == "" {
		return editInEditor(ctx, x, c, svc, caller, id)
	}
	in := engine.UpdateIn{ID: id, Set: map[string]any{}, Add: map[string][]string{}, Remove: map[string][]string{}}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return usageErr(x, fl, "--set takes K=V")
		}
		if v == "" {
			in.Set[k] = nil
		} else {
			in.Set[k] = v
		}
	}
	for _, pair := range []struct {
		src multi
		dst map[string][]string
	}{{adds, in.Add}, {removes, in.Remove}} {
		for _, s := range pair.src {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				return usageErr(x, fl, "--add/--remove take K=V")
			}
			pair.dst[k] = append(pair.dst[k], v)
		}
	}
	if *body != "" {
		b := readText(x, *body)
		in.Body = &b
	}
	outs, err := svc.Update(ctx, caller, []engine.UpdateIn{in})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, outs[0], render.Write("updated", outs[0]))
}

func editInEditor(ctx context.Context, x *IO, c *common, svc *engine.Service, caller engine.Caller, id string) int {
	path, _, err := svc.TicketPath(id)
	if err != nil {
		return fail(x, c, err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return fail(x, c, err)
	}
	tmp, err := os.CreateTemp("", id+"-*.md")
	if err != nil {
		return fail(x, c, err)
	}
	defer os.Remove(tmp.Name())
	tmp.Write(orig)
	tmp.Close()
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	for {
		parts := strings.Fields(editor)
		cmd := exec.Command(parts[0], append(parts[1:], tmp.Name())...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fail(x, c, fmt.Errorf("editor: %v", err))
		}
		edited, _ := os.ReadFile(tmp.Name())
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(x.Out, "no changes")
			return 0
		}
		w, err := svc.ReplaceTicket(ctx, caller, id, edited)
		if err == nil {
			return emit(x, c, w, render.Write("updated", w))
		}
		fmt.Fprintln(x.Err, "ka: "+render.Error(engine.AsError(err)))
		if !isTTY(os.Stdin) {
			return 1
		}
		fmt.Fprint(x.Err, "edit again? [Y/n] ")
		var ans string
		fmt.Fscanln(os.Stdin, &ans)
		if strings.HasPrefix(strings.ToLower(ans), "n") {
			return 1
		}
	}
}

func cmdClaim(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("claim", "ID [--steal]", x)
	steal := fl.Bool("steal", false, "take over a stale claim")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "claim needs one ID")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Claim(ctx, caller, pos[0], *steal)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write("claimed", &out.WriteOut))
}

func cmdRelease(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("release", "ID [--status S]", x)
	status := fl.String("status", "", "move to this status")
	force := fl.Bool("force", false, "release someone else's claim")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "release needs one ID")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Release(ctx, caller, pos[0], *status, *force)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write("released", out))
}

func cmdMove(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("move", "ID STATUS [-m SUMMARY] [--resolution R]", x)
	m := fl.String("m", "", "summary (required when closing; - reads stdin)")
	res := fl.String("resolution", "", "done, canceled, duplicate or wontfix")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		return usageErr(x, fl, "move needs an ID and a status")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Transition(ctx, caller, engine.TransitionIn{ID: pos[0], Status: pos[1], Resolution: *res, Summary: readText(x, *m)})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write("moved", out))
}

func cmdComment(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("comment", "ID [-m TEXT] [--type T]", x)
	m := fl.String("m", "", "text (default: stdin)")
	typ := fl.String("type", "note", "note, progress or decision")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) < 1 {
		return usageErr(x, fl, "comment needs an ID")
	}
	text := *m
	if text == "" && len(pos) > 1 {
		text = strings.Join(pos[1:], " ")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Comment(ctx, caller, engine.CommentIn{ID: pos[0], Type: *typ, Body: readText(x, text)})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write("commented", out))
}

func cmdCheckItem(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("check-item", "ID INDEX|TEXT [--undo]", x)
	undo := fl.Bool("undo", false, "mark not done")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) < 2 {
		return usageErr(x, fl, "check-item needs an ID and an item index or text")
	}
	it := engine.CheckItem{Done: !*undo}
	if n, err := strconv.Atoi(pos[1]); err == nil && len(pos) == 2 {
		it.Index = &n
	} else {
		it.Text = strings.Join(pos[1:], " ")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, prog, err := svc.CheckItems(ctx, caller, engine.CheckIn{ID: pos[0], Items: []engine.CheckItem{it}})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write("acceptance "+prog, out))
}

func cmdAsk(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("ask", "ID -m QUESTION [--option O]... [--to WHO] [--approval] [--wait D]", x)
	m := fl.String("m", "", "the question")
	to := fl.String("to", "", "addressee")
	approval := fl.Bool("approval", false, "request approval")
	wait := fl.String("wait", "", "wait this long for an answer, e.g. 10m")
	var opts multi
	fl.Var(&opts, "option", "a choice (repeatable)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "ask needs one ID")
	}
	in := engine.AskIn{ID: pos[0], Question: readText(x, *m), Options: opts, To: *to}
	if *approval {
		in.Kind = "approval"
	}
	if *wait != "" {
		d, err := kaspec.ParseDuration(*wait)
		if err != nil {
			return usageErr(x, fl, "bad --wait: "+err.Error())
		}
		in.Wait = d
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Ask(ctx, caller, in)
	if err != nil {
		return fail(x, c, err)
	}
	text := fmt.Sprintf("%s asked [%s]", out.ID, out.Question)
	if out.Answered {
		text += "\n" + render.Comment(*out.Answer)
	}
	return emit(x, c, out, text)
}

func cmdAnswer(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("answer", "ID [--choice N] [-m TEXT] [--question NAME]", x)
	choice := fl.Int("choice", -1, "option index")
	m := fl.String("m", "", "answer text")
	q := fl.String("question", "", "question comment filename (default: the newest open question)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) < 1 {
		return usageErr(x, fl, "answer needs an ID")
	}
	text := *m
	if text == "" && len(pos) > 1 {
		text = strings.Join(pos[1:], " ")
	}
	in := engine.AnswerIn{ID: pos[0], Question: *q, Text: text}
	if *choice >= 0 {
		in.Choice = choice
	}
	return answer(ctx, x, c, in, "answered")
}

func answer(ctx context.Context, x *IO, c *common, in engine.AnswerIn, verb string) int {
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Answer(ctx, caller, in)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Write(verb, out))
}

func cmdApprove(ctx context.Context, x *IO, args []string) int { return approve(ctx, x, args, true) }
func cmdReject(ctx context.Context, x *IO, args []string) int  { return approve(ctx, x, args, false) }

func approve(ctx context.Context, x *IO, args []string, yes bool) int {
	name := map[bool]string{true: "approve", false: "reject"}[yes]
	fl, c := newFlags(name, "ID [-m TEXT]", x)
	m := fl.String("m", "", "comment")
	q := fl.String("question", "", "request comment filename (default: the newest open one)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, name+" needs one ID")
	}
	return answer(ctx, x, c, engine.AnswerIn{ID: pos[0], Question: *q, Text: *m, Approved: &yes}, name+"d")
}

func cmdSync(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("sync", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out := svc.Sync(ctx, !c.noPush && !c.offline)
	code := emit(x, c, out, render.Sync(out))
	if len(out.Errors) > 0 {
		return 1
	}
	return code
}

func cmdFix(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("fix", "[--dry-run]", x)
	dry := fl.Bool("dry-run", false, "report only")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Fix(ctx, caller, *dry)
	if err != nil {
		return fail(x, c, err)
	}
	var b strings.Builder
	for _, cat := range out.Categories {
		fmt.Fprintf(&b, "%s: %d file(s)", cat.Name, len(cat.Files))
		if cat.Commit != "" {
			fmt.Fprintf(&b, " (commit %s)", cat.Commit)
		}
		b.WriteString("\n")
		for _, f := range cat.Files {
			b.WriteString("  " + f + "\n")
		}
	}
	if b.Len() == 0 {
		b.WriteString("nothing to fix\n")
	}
	return emit(x, c, out, b.String())
}

func cmdInit(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("init", "[--project KEY] [DIR]", x)
	var o engine.InitOptions
	fl.StringVar(&o.Project, "project", "", "create a first project with this key")
	fl.StringVar(&o.Prefix, "prefix", "", "its ID prefix (default: derived from the key)")
	fl.StringVar(&o.Name, "name", "", "store name (default: the directory name)")
	fl.StringVar(&o.Extends, "extends", "", "parent store URL (e.g. ../org)")
	agents := fl.Bool("agents-md", true, "write AGENTS.md with the hand-writing protocol")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	o.Dir = x.Dir
	if len(pos) > 0 {
		o.Dir = pos[0]
		if !filepath.IsAbs(o.Dir) {
			o.Dir = filepath.Join(x.Dir, o.Dir)
		}
	}
	created, err := engine.InitStore(ctx, o)
	if err != nil {
		return fail(x, c, err)
	}
	if *agents {
		p := filepath.Join(o.Dir, "AGENTS.md")
		if _, err := os.Stat(p); err != nil {
			os.WriteFile(p, []byte("# Agents\n\n"+engine.AgentsMD(o.Project, prefixOr(o))), 0o644)
			created = append(created, "AGENTS.md")
		}
	}
	var b strings.Builder
	for _, f := range created {
		b.WriteString("created " + f + "\n")
	}
	if len(created) == 0 {
		b.WriteString("already a store; nothing to do\n")
	} else {
		b.WriteString("commit these files to publish the store\n")
	}
	return emit(x, c, map[string]any{"created": created}, b.String())
}

func prefixOr(o engine.InitOptions) string {
	if o.Prefix != "" {
		return o.Prefix
	}
	if o.Project != "" {
		return kaspec.DefaultPrefix(o.Project)
	}
	return ""
}

func cmdMergeDriver(ctx context.Context, x *IO, args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(x.Err, "usage: ka merge-driver %O %A %B [%P]")
		return 2
	}
	p := ""
	if len(args) > 3 {
		p = args[3]
	}
	return engine.RunMergeDriver(ctx, args[0], args[1], args[2], p)
}
