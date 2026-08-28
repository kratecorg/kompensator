package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kratecorg/kompensator/internal/config"
	"github.com/kratecorg/kompensator/internal/repo"
)

// Shell completion is served by the binary itself: the generated script is a
// thin wrapper that hands the words typed so far to the hidden '__complete'
// command and turns its lines into candidates. That keeps the candidate logic
// in Go, where the command tree lives, and lets completion offer real
// environment, stack, project and node names read from the local checkout.

// compFlag is one flag of a command. Flags that take a value may offer
// candidates for it.
type compFlag struct {
	name   string
	value  bool
	values func(c *compCtx, pos []string) []string
}

// compArg is one positional of a command, completed by position.
type compArg struct {
	values func(c *compCtx, pos []string) []string
}

// compCmd is a command, its subcommands, flags and positionals.
type compCmd struct {
	name  string
	subs  []compCmd
	flags []compFlag
	args  []compArg
}

var compShells = []string{"bash", "zsh", "fish"}

func fixed(values ...string) func(*compCtx, []string) []string {
	return func(*compCtx, []string) []string { return values }
}

// repoFlag completes --repo with the repos the home configures.
func repoFlag() compFlag {
	return compFlag{name: "repo", value: true, values: func(c *compCtx, _ []string) []string { return c.repoNames() }}
}

func boolFlag(name string) compFlag { return compFlag{name: name} }

func valueFlag(name string) compFlag { return compFlag{name: name, value: true} }

// completionSpec mirrors the command tree main() dispatches on. It has to be
// kept in step with it by hand; the stdlib flag package exposes no tree to
// derive it from.
func completionSpec() compCmd {
	envArg := compArg{values: compEnvs}
	nodeArg := compArg{values: compNodes}
	free := compArg{}

	return compCmd{
		name: "kompensator",
		flags: []compFlag{
			{name: "home", value: true},
			boolFlag("json"),
		},
		subs: []compCmd{
			{
				name:  "reconcile",
				flags: []compFlag{boolFlag("force"), boolFlag("prune"), boolFlag("ignore-pause"), repoFlag()},
				args:  []compArg{envArg, {values: compStacks(0)}, {values: compProjects(1)}},
			},
			{
				name:  "status",
				flags: []compFlag{repoFlag()},
				args:  []compArg{envArg},
			},
			{
				name:  "pause",
				flags: []compFlag{valueFlag("reason"), valueFlag("timeout"), valueFlag("wait")},
			},
			{name: "resume"},
			{
				name: "verify",
				flags: []compFlag{
					valueFlag("repo-path"), repoFlag(), valueFlag("commit"), valueFlag("branch"),
					boolFlag("wait"), valueFlag("timeout"), valueFlag("interval"),
				},
				args: []compArg{envArg},
			},
			{name: "check", flags: []compFlag{boolFlag("update")}},
			{
				name: "controller",
				subs: []compCmd{
					{name: "init"},
					{name: "repo", subs: []compCmd{
						{name: "add", flags: []compFlag{valueFlag("branch")}, args: []compArg{free, free}},
					}},
				},
			},
			{
				name: "node",
				subs: []compCmd{
					{
						name: "add",
						flags: []compFlag{
							repoFlag(), boolFlag("status-writeback"),
							boolFlag("status-writeback-always"), valueFlag("schedule"),
						},
						args: []compArg{free, free},
					},
					{
						name:  "remove",
						flags: []compFlag{repoFlag(), boolFlag("keep-containers"), boolFlag("keep-home")},
						args:  []compArg{nodeArg},
					},
				},
			},
			{
				name: "env",
				subs: []compCmd{{name: "list", flags: []compFlag{repoFlag()}}},
			},
			{
				name: "stack",
				subs: []compCmd{
					{name: "list", flags: []compFlag{repoFlag()}},
					{
						name: "add",
						flags: []compFlag{
							{name: "proxy", value: true, values: fixed("traefik")},
							repoFlag(), boolFlag("dry-run"),
						},
						args: []compArg{free},
					},
				},
			},
			{
				name: "project",
				subs: []compCmd{
					{
						name: "add",
						flags: []compFlag{
							valueFlag("service"),
							{name: "strategy", value: true, values: fixed("blue-green", "recreate")},
							valueFlag("port"), boolFlag("route"), repoFlag(), boolFlag("dry-run"),
						},
						args: []compArg{{values: compStacks(-1)}, free},
					},
				},
			},
			{
				name: "secrets",
				subs: []compCmd{
					{name: "set", flags: []compFlag{repoFlag()}, args: []compArg{envArg, {values: compStacks(0)}}},
					{name: "set-key", flags: []compFlag{repoFlag()}, args: []compArg{envArg, {values: compStacks(0)}, free, free}},
					{name: "set-file", flags: []compFlag{repoFlag()}, args: []compArg{envArg, {values: compFileSecrets(0)}, free}},
					{name: "show", flags: []compFlag{repoFlag()}, args: []compArg{envArg, {values: compStacks(0)}}},
					{name: "edit", flags: []compFlag{repoFlag()}, args: []compArg{envArg, {values: compStacks(0)}}},
					{name: "rekey", flags: []compFlag{repoFlag()}, args: []compArg{envArg}},
				},
			},
			{name: "completion", args: []compArg{{values: fixed(compShells...)}}},
			{name: "version"},
			{name: "help"},
		},
	}
}

// compCtx carries what the words typed so far reveal about where to look for
// candidates, and caches the resolved checkouts.
type compCtx struct {
	home      string
	repo      string
	roots     []string
	rootsDone bool
}

// repoRoots returns the local deployment-repo checkouts to read candidates
// from. It never fetches: completion has to stay instant and offline, so it
// reflects the last reconcile's checkout rather than the remote.
func (c *compCtx) repoRoots() []string {
	if c.rootsDone {
		return c.roots
	}
	c.rootsDone = true
	h, err := resolveHome(c.home)
	if err != nil {
		return nil
	}
	cfg, err := config.Load(h)
	if err != nil {
		return nil
	}
	for _, r := range cfg.Repos {
		if c.repo != "" && r.Name != c.repo {
			continue
		}
		c.roots = append(c.roots, filepath.Join(config.ReposDir(h), r.Name))
	}
	return c.roots
}

func (c *compCtx) repoNames() []string {
	h, err := resolveHome(c.home)
	if err != nil {
		return nil
	}
	cfg, err := config.Load(h)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range cfg.Repos {
		out = append(out, r.Name)
	}
	return out
}

func compEnvs(c *compCtx, _ []string) []string {
	var out []string
	for _, root := range c.repoRoots() {
		names, _ := repo.ListEnvironments(root)
		out = append(out, names...)
	}
	return out
}

func compNodes(c *compCtx, _ []string) []string {
	var out []string
	for _, root := range c.repoRoots() {
		inv, err := repo.LoadInventory(root)
		if err != nil {
			continue
		}
		for _, n := range inv.Nodes {
			out = append(out, n.Name)
		}
	}
	return out
}

// compStacks completes a stack name. When envIdx points at an environment
// already on the line, only the stacks that environment places are offered;
// otherwise (envIdx < 0, or the env not typed yet) every stack in the repo is.
func compStacks(envIdx int) func(*compCtx, []string) []string {
	return func(c *compCtx, pos []string) []string {
		env := ""
		if envIdx >= 0 && envIdx < len(pos) {
			env = pos[envIdx]
		}
		var out []string
		for _, root := range c.repoRoots() {
			if env != "" {
				e, err := repo.LoadEnvironment(root, env)
				if err == nil {
					out = append(out, e.StackNames()...)
					continue
				}
			}
			names, _ := repo.ListStacks(root)
			out = append(out, names...)
		}
		return out
	}
}

// compProjects completes a project name within the stack at stackIdx.
func compProjects(stackIdx int) func(*compCtx, []string) []string {
	return func(c *compCtx, pos []string) []string {
		if stackIdx >= len(pos) {
			return nil
		}
		stack := pos[stackIdx]
		var out []string
		for _, root := range c.repoRoots() {
			s, err := repo.LoadStack(root, stack)
			if err != nil {
				continue
			}
			for _, p := range s.Projects {
				out = append(out, p.Name)
			}
		}
		return out
	}
}

// compFileSecrets completes the file secrets the environment at envIdx declares.
func compFileSecrets(envIdx int) func(*compCtx, []string) []string {
	return func(c *compCtx, pos []string) []string {
		if envIdx >= len(pos) {
			return nil
		}
		var out []string
		for _, root := range c.repoRoots() {
			e, err := repo.LoadEnvironment(root, pos[envIdx])
			if err != nil {
				continue
			}
			for _, s := range e.Secrets {
				out = append(out, s.Name)
			}
		}
		return out
	}
}

// cmdComplete is the hidden completion backend. args are the words typed after
// the program name, the last one being the word under the cursor (empty when
// the cursor sits after a space). It prints one candidate per line and never
// fails: a shell must not see errors while the user is typing.
func cmdComplete(args []string) int {
	for _, s := range completeWords(args) {
		fmt.Println(s)
	}
	return 0
}

func completeWords(args []string) []string {
	spec := completionSpec()
	globals := spec.flags

	cur := ""
	if len(args) > 0 {
		cur = args[len(args)-1]
		args = args[:len(args)-1]
	}

	c := &compCtx{}
	cmd := &spec
	flags := globals
	var pos []string
	var pending *compFlag

	for _, w := range args {
		if pending != nil {
			c.record(pending.name, w)
			pending = nil
			continue
		}
		if isFlagWord(w) {
			name, val, hasVal := splitFlagWord(w)
			f := findFlag(flags, name)
			switch {
			case f == nil:
				// Unknown flag: assume it is a switch and keep going.
			case hasVal:
				c.record(f.name, val)
			case f.value:
				pending = f
			}
			continue
		}
		if sub := findCmd(cmd.subs, w); sub != nil && len(pos) == 0 {
			cmd = sub
			flags = append(append([]compFlag(nil), globals...), sub.flags...)
			continue
		}
		pos = append(pos, w)
	}

	// A flag still waiting for its value takes precedence over everything else.
	if pending != nil {
		if pending.values == nil {
			return nil
		}
		return finish(pending.values(c, pos), cur)
	}

	if strings.HasPrefix(cur, "-") {
		// Go's flag package takes one dash or two, so complete in whichever
		// form is already being typed.
		dashes := "--"
		if strings.HasPrefix(cur, "-") && !strings.HasPrefix(cur, "--") && len(cur) > 1 {
			dashes = "-"
		}
		prefix := dashes + strings.TrimLeft(cur, "-")
		var out []string
		for _, f := range flags {
			out = append(out, dashes+f.name)
		}
		return finish(out, prefix)
	}

	var out []string
	if len(pos) == 0 {
		for _, s := range cmd.subs {
			out = append(out, s.name)
		}
	}
	if i := len(pos); i < len(cmd.args) && cmd.args[i].values != nil {
		out = append(out, cmd.args[i].values(c, pos)...)
	}
	return finish(out, cur)
}

// record keeps the flag values that decide where candidates come from.
func (c *compCtx) record(name, value string) {
	switch name {
	case "home":
		c.home = value
	case "repo":
		c.repo = value
	}
}

func isFlagWord(w string) bool { return strings.HasPrefix(w, "-") && w != "-" }

// splitFlagWord strips the leading dashes and splits an inline -flag=value.
func splitFlagWord(w string) (name, value string, hasValue bool) {
	w = strings.TrimLeft(w, "-")
	if i := strings.IndexByte(w, '='); i >= 0 {
		return w[:i], w[i+1:], true
	}
	return w, "", false
}

func findFlag(flags []compFlag, name string) *compFlag {
	for i := range flags {
		if flags[i].name == name {
			return &flags[i]
		}
	}
	return nil
}

func findCmd(cmds []compCmd, name string) *compCmd {
	for i := range cmds {
		if cmds[i].name == name {
			return &cmds[i]
		}
	}
	return nil
}

// finish keeps the candidates that extend cur, sorted and deduplicated.
func finish(candidates []string, cur string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range candidates {
		if s == "" || seen[s] || !strings.HasPrefix(s, cur) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// cmdCompletion prints the completion script for a shell.
func cmdCompletion(args []string) int {
	shell := arg(args, 0)
	script, ok := completionScript(shell, filepath.Base(os.Args[0]))
	if !ok {
		fmt.Fprintf(os.Stderr, "Usage: kompensator completion <%s>\n", strings.Join(compShells, "|"))
		return 2
	}
	fmt.Print(script)
	return 0
}

func completionScript(shell, prog string) (string, bool) {
	var tmpl string
	switch shell {
	case "bash":
		tmpl = bashCompletion
	case "zsh":
		tmpl = zshCompletion
	case "fish":
		tmpl = fishCompletion
	default:
		return "", false
	}
	return strings.ReplaceAll(tmpl, "@PROG@", prog), true
}

const bashCompletion = `# bash completion for @PROG@
# Install: @PROG@ completion bash > /etc/bash_completion.d/@PROG@
#      or: source <(@PROG@ completion bash)   in ~/.bashrc
_@PROG@_completion() {
    local IFS=$'\n'
    local words candidate
    words=("${COMP_WORDS[@]:1:COMP_CWORD}")
    COMPREPLY=()
    while read -r candidate; do
        [[ -n $candidate ]] && COMPREPLY+=("$candidate")
    done < <(@PROG@ __complete "${words[@]}" 2>/dev/null)
}
complete -o default -o bashdefault -F _@PROG@_completion @PROG@
`

const zshCompletion = `#compdef @PROG@
# zsh completion for @PROG@
# Install: @PROG@ completion zsh > "${fpath[1]}/_@PROG@"   (then restart zsh)
#      or: source <(@PROG@ completion zsh)   in ~/.zshrc, after compinit
_@PROG@_completion() {
    local -a args candidates
    args=("${(@)words[2,$CURRENT]}")
    (( ${#args} < CURRENT - 1 )) && args+=('')
    candidates=("${(@f)$(@PROG@ __complete "${args[@]}" 2>/dev/null)}")
    if (( ${#candidates} == 0 )) || [[ -z ${candidates[1]} ]]; then
        _files
        return
    fi
    compadd -- "${candidates[@]}"
}
compdef _@PROG@_completion @PROG@
`

const fishCompletion = `# fish completion for @PROG@
# Install: @PROG@ completion fish > ~/.config/fish/completions/@PROG@.fish
function __@PROG@_completion
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    set -e tokens[1]
    @PROG@ __complete $tokens $current 2>/dev/null
end
complete -c @PROG@ -f -a '(__@PROG@_completion)'
`
