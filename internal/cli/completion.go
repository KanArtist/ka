package cli

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// Shell completion (PRD §14: ka completion <shell>). The scripts call the
// hidden `ka __complete WORDS...`, which lists candidates for the last word:
// commands, a command's flags (read from its own -h output, so they never
// drift), and ticket IDs for commands that take one.

// takesID lists commands whose first argument is a ticket ID.
var takesID = map[string]bool{"show": true, "edit": true, "claim": true, "release": true, "move": true, "comment": true,
	"check-item": true, "ask": true, "answer": true, "approve": true, "reject": true, "open": true}

func cmdComplete(ctx context.Context, x *IO, args []string) int {
	cur := ""
	if len(args) > 0 {
		cur = args[len(args)-1]
		args = args[:len(args)-1]
	}
	var out []string
	switch {
	case len(args) == 0:
		for _, c := range commands {
			if !c.hidden && strings.HasPrefix(c.name, cur) {
				out = append(out, c.name)
			}
		}
		for _, s := range []string{"help", "completion"} {
			if strings.HasPrefix(s, cur) {
				out = append(out, s)
			}
		}
	case strings.HasPrefix(cur, "-"):
		out = commandFlags(ctx, args[0], cur)
	case args[0] == "completion":
		for _, s := range []string{"bash", "zsh", "fish", "powershell"} {
			if strings.HasPrefix(s, cur) {
				out = append(out, s)
			}
		}
	case takesID[args[0]] && positional(args[1:]) == 0:
		out = ticketIDs(ctx, x, cur)
	case args[0] == "move" && positional(args[1:]) == 1:
		out = statuses(ctx, x, cur)
	}
	sort.Strings(out)
	for _, s := range out {
		fmt.Fprintln(x.Out, s)
	}
	return 0
}

// positional counts non-flag words (a rough count: flag values are not
// told apart from positionals, which is fine for completion).
func positional(words []string) int {
	n := 0
	for _, w := range words {
		if !strings.HasPrefix(w, "-") {
			n++
		}
	}
	return n
}

func commandFlags(ctx context.Context, name, cur string) []string {
	for _, c := range commands {
		if c.name != name {
			continue
		}
		var buf bytes.Buffer
		c.run(ctx, &IO{Out: &buf, Err: &buf, In: strings.NewReader(""), Dir: "."}, []string{"-h"})
		var out []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if !strings.HasPrefix(line, "  -") {
				continue
			}
			f := strings.Fields(line)[0]
			flag := "-" + strings.TrimPrefix(f, "-")
			if strings.HasPrefix("-"+flag, cur) {
				out = append(out, "-"+flag)
			} else if strings.HasPrefix(flag, cur) {
				out = append(out, flag)
			}
		}
		return out
	}
	return nil
}

func openQuiet(ctx context.Context, x *IO) *engine.Service {
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Offline: true})
	if err != nil {
		return nil
	}
	svc, err := engine.New(ctx, ws)
	if err != nil {
		return nil
	}
	return svc
}

func ticketIDs(ctx context.Context, x *IO, cur string) []string {
	svc := openQuiet(ctx, x)
	if svc == nil {
		return nil
	}
	res, err := svc.Search(engine.SearchIn{Limit: -1})
	if err != nil {
		return nil
	}
	var out []string
	up := strings.ToUpper(cur)
	for _, t := range res.Tickets {
		if strings.HasPrefix(t.ID, up) {
			out = append(out, t.ID)
		}
		if len(out) >= 300 {
			break
		}
	}
	return out
}

func statuses(ctx context.Context, x *IO, cur string) []string {
	svc := openQuiet(ctx, x)
	if svc == nil {
		return nil
	}
	sch, err := svc.Schema(engine.Caller{}, "")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range sch.Projects {
		for _, s := range p.Statuses {
			if !seen[s.Name] && strings.HasPrefix(s.Name, cur) {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
	}
	return out
}

func cmdCompletion(ctx context.Context, x *IO, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(x.Err, "usage: ka completion bash|zsh|fish|powershell")
		return 2
	}
	script, ok := completionScripts[args[0]]
	if !ok {
		fmt.Fprintf(x.Err, "ka: no completion for %q; use bash, zsh, fish or powershell\n", args[0])
		return 2
	}
	fmt.Fprint(x.Out, script)
	return 0
}

var completionScripts = map[string]string{
	"bash": `# ka completion for bash. Load it with:
#   source <(ka completion bash)
# or save it to /etc/bash_completion.d/ka (or ~/.local/share/bash-completion/completions/ka).
_ka() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  local IFS=$'\n'
  COMPREPLY=($(ka __complete "${COMP_WORDS[@]:1:COMP_CWORD-1}" "$cur" 2>/dev/null))
}
complete -o default -F _ka ka
`,
	"zsh": `#compdef ka
# ka completion for zsh. Load it with:
#   source <(ka completion zsh)
# or save it as _ka in a directory on $fpath.
_ka() {
  local -a items
  items=("${(@f)$(ka __complete "${words[@]:1:CURRENT-2}" "${words[CURRENT]}" 2>/dev/null)}")
  if (( ${#items} )) && [[ -n "${items[1]}" ]]; then
    compadd -a items
  else
    _files
  fi
}
compdef _ka ka
`,
	"fish": `# ka completion for fish. Load it with:
#   ka completion fish | source
# or save it to ~/.config/fish/completions/ka.fish.
function __ka_complete
    set -l words (commandline -opc)
    set -e words[1]
    ka __complete $words (commandline -ct) 2>/dev/null
end
complete -c ka -f -a '(__ka_complete)'
`,
	"powershell": `# ka completion for PowerShell. Load it with:
#   ka completion powershell | Out-String | Invoke-Expression
# or add that line to your $PROFILE.
Register-ArgumentCompleter -Native -CommandName ka -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.ToString() })
    if ($wordToComplete -ne '' -and $words.Count -gt 0) { $words = $words[0..($words.Count - 2)] }
    $words += $wordToComplete
    ka __complete @words 2>$null | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`,
}
