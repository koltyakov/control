package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const completionUsageText = "usage: control completion bash|zsh|fish|powershell [--login]"

// Completion is derived from the help text rather than a second command list.
// The help constants are the user-visible command surface, and their role
// sections (usage, fleetUsage, adminUsage) already decide what each credential
// may see. Tests check the parsed commands against the dispatch switch in run.
var (
	// completionExtras are dispatched commands that are harmless for every role
	// but are not listed as help lines. Each is completed for every role.
	completionExtras = []string{"help", "version", "upgrade"}
	// completionAliases copy the command-level flags of another command.
	completionAliases = map[string]string{"upgrade": "update"}
	// completionInternal are dispatched commands that help deliberately omits:
	// installer entry points and help spellings handled by flag parsing.
	completionInternal = []string{"install-self", "enroll", "--help", "-h"}
)

var (
	completionWordPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	completionFlagPattern = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
	completionFlagInText  = regexp.MustCompile(`(?:^|[^A-Za-z0-9-])(--[a-z][a-z0-9-]*)`)
)

// completionNode is one command path. Names and flags only ever contain
// [a-z0-9-], which keeps every generated script free of quoting hazards.
type completionNode struct {
	path     string
	children []*completionNode
	flags    []string
}

func (n *completionNode) child(name string) *completionNode {
	for _, c := range n.children {
		if c.path == joinCompletionPath(n.path, name) {
			return c
		}
	}
	c := &completionNode{path: joinCompletionPath(n.path, name)}
	n.children = append(n.children, c)
	return c
}

func (n *completionNode) name() string {
	return n.path[strings.LastIndex(n.path, " ")+1:]
}

func (n *completionNode) addFlags(flags ...string) {
	for _, option := range flags {
		if !slices.Contains(n.flags, option) {
			n.flags = append(n.flags, option)
		}
	}
}

func (n *completionNode) childNames() []string {
	names := make([]string, 0, len(n.children))
	for _, c := range n.children {
		names = append(names, c.name())
	}
	return names
}

// walk visits nodes depth-first in help order, starting with the root.
func (n *completionNode) walk(visit func(*completionNode)) {
	visit(n)
	for _, c := range n.children {
		c.walk(visit)
	}
}

func joinCompletionPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + " " + name
}

// completionSpec is the command tree plus the global flags that consume the
// following word, which the shells skip before the command.
type completionSpec struct {
	root        *completionNode
	valueGlobal []string
}

func completionFlagsIn(text string) []string {
	var flags []string
	for _, m := range completionFlagInText.FindAllStringSubmatch(text, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

// parseCompletionHelp reads the command lines of help text. A command line is
// indented by exactly two spaces. Following lowercase words form its
// subcommand path; an a|b|c group or an optional [word] ends the path. Deeper
// indented lines starting with -- add flags to the previous command line, and
// "..." continues the previous line's subcommand.
func parseCompletionHelp(help string) completionSpec {
	spec := completionSpec{root: &completionNode{}}
	var last []*completionNode
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if rest, ok := strings.CutPrefix(line, "Global flags:"); ok {
			fields := strings.Fields(rest)
			for i, field := range fields {
				if !completionFlagPattern.MatchString(field) {
					continue
				}
				spec.root.addFlags(field)
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					spec.valueGlobal = append(spec.valueGlobal, field)
				}
			}
			last = nil
			continue
		}
		switch {
		case indent == 2 && trimmed != "":
			fields := strings.Fields(trimmed)
			if !completionWordPattern.MatchString(fields[0]) {
				last = nil
				continue
			}
			nodes := []*completionNode{spec.root.child(fields[0])}
			for _, field := range fields[1:] {
				if field == "..." {
					if len(last) > 0 && strings.HasPrefix(last[0].path+" ", fields[0]+" ") {
						nodes = last
					}
					break
				}
				optional := strings.HasPrefix(field, "[") && strings.HasSuffix(field, "]")
				names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(field, "["), "]"), "|")
				if !allCompletionWords(names) {
					break
				}
				parent := nodes[0]
				nodes = nil
				if optional {
					nodes = append(nodes, parent)
				}
				for _, name := range names {
					nodes = append(nodes, parent.child(name))
				}
				if optional || len(names) > 1 {
					break
				}
			}
			for _, n := range nodes {
				n.addFlags(completionFlagsIn(trimmed)...)
			}
			last = nodes
		case indent > 2 && strings.HasPrefix(trimmed, "--"):
			for _, n := range last {
				n.addFlags(completionFlagsIn(trimmed)...)
			}
		case indent > 2:
			// A wrapped description keeps the previous command line.
		default:
			last = nil
		}
	}
	for _, name := range completionExtras {
		n := spec.root.child(name)
		if target, ok := completionAliases[name]; ok {
			for _, c := range spec.root.children {
				if c.path == target {
					n.addFlags(c.flags...)
				}
			}
		}
	}
	return spec
}

func allCompletionWords(names []string) bool {
	for _, name := range names {
		if !completionWordPattern.MatchString(name) {
			return false
		}
	}
	return len(names) > 0
}

// validate rejects any word a script could interpret, as a second guard on top
// of the parser's patterns.
func (s completionSpec) validate() error {
	var err error
	s.root.walk(func(n *completionNode) {
		for _, word := range strings.Fields(n.path) {
			if !completionWordPattern.MatchString(word) {
				err = fmt.Errorf("unsafe completion word %q", word)
			}
		}
		for _, option := range n.flags {
			if !completionFlagPattern.MatchString(option) {
				err = fmt.Errorf("unsafe completion flag %q", option)
			}
		}
	})
	for _, option := range s.valueGlobal {
		if !completionFlagPattern.MatchString(option) {
			err = fmt.Errorf("unsafe completion flag %q", option)
		}
	}
	return err
}

// valueGlobalForms includes the single-dash spellings that flag.Parse accepts.
func (s completionSpec) valueGlobalForms() []string {
	var forms []string
	for _, option := range s.valueGlobal {
		forms = append(forms, option, strings.TrimPrefix(option, "-"))
	}
	return forms
}

func completionCLI(w io.Writer, args []string, loginHelp func() string) error {
	f := flag.NewFlagSet("completion", flag.ContinueOnError)
	login := f.Bool("login", false, "include the commands help shows for the saved login; contacts the gateway")
	f.SetOutput(io.Discard)
	var shells []string
	for {
		if err := f.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_, err = fmt.Fprintln(w, completionUsageText)
				return err
			}
			return fmt.Errorf("%w\n%s", err, completionUsageText)
		}
		if f.NArg() == 0 {
			break
		}
		shells = append(shells, f.Arg(0))
		args = f.Args()[1:]
	}
	if len(shells) != 1 {
		return errors.New(completionUsageText)
	}
	// By default the script covers only commands help shows to every
	// credential, so printing it never contacts the gateway.
	help := usage
	if *login {
		help = loginHelp()
	}
	script, err := completionScript(shells[0], help)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, script)
	return err
}

func completionScript(shell, help string) (string, error) {
	spec := parseCompletionHelp(help)
	if err := spec.validate(); err != nil {
		return "", err
	}
	switch shell {
	case "bash":
		return bashCompletion(spec), nil
	case "zsh":
		return zshCompletion(spec), nil
	case "fish":
		return fishCompletion(spec), nil
	case "powershell", "pwsh":
		return powershellCompletion(spec), nil
	}
	return "", fmt.Errorf("unknown shell %q\n%s", shell, completionUsageText)
}

func bashCompletion(s completionSpec) string {
	var b strings.Builder
	b.WriteString(`# bash completion for control. Load it with:
#   eval "$(control completion bash)"
# or save the output as ~/.local/share/bash-completion/completions/control.

__control_node() {
  __control_subs=''
  __control_flags=''
  case "$1" in
`)
	s.root.walk(func(n *completionNode) {
		fmt.Fprintf(&b, "    '%s') __control_subs='%s'; __control_flags='%s' ;;\n",
			n.path, strings.Join(n.childNames(), " "), strings.Join(n.flags, " "))
	})
	fmt.Fprintf(&b, `  esac
}

__control_has() {
  local word="$1" item
  shift
  for item in "$@"; do
    [ "$item" = "$word" ] && return 0
  done
  return 1
}

_control() {
  local IFS=$' \t\n' cur word key='' frozen=0 i=1 __control_subs __control_flags
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"
  while [ "$i" -lt "$COMP_CWORD" ]; do
    word="${COMP_WORDS[i]}"
    [ "$word" = "--" ] && return 0
    if [ -z "$key" ] && [ "$frozen" = 0 ]; then
      case "$word" in
        %s)
          if [ "${COMP_WORDS[i+1]-}" = "=" ]; then i=$((i + 3)); else i=$((i + 2)); fi
          continue ;;
        =) i=$((i + 2)); continue ;;
        -*) i=$((i + 1)); continue ;;
      esac
    fi
    if [ "$frozen" = 0 ]; then
      __control_node "$key"
      if __control_has "$word" $__control_subs; then
        key="${key:+$key }$word"
        i=$((i + 1))
        continue
      fi
      [ -z "$key" ] && return 0
      frozen=1
    fi
    i=$((i + 1))
  done
  [ "$i" -gt "$COMP_CWORD" ] && return 0
  __control_node "$key"
  case "$cur" in
    -*) COMPREPLY=($(compgen -W "$__control_flags" -- "$cur")) ;;
    *) [ "$frozen" = 0 ] && COMPREPLY=($(compgen -W "$__control_subs" -- "$cur")) ;;
  esac
  return 0
}

complete -o default -F _control control
`, strings.Join(s.valueGlobalForms(), "|"))
	return b.String()
}

func zshCompletion(s completionSpec) string {
	var b strings.Builder
	b.WriteString(`#compdef control
# zsh completion for control. Load it after compinit with:
#   source <(control completion zsh)
# or save the output as _control in a directory on $fpath.

__control_node() {
  reply_subs=()
  reply_flags=()
  case $1 in
`)
	s.root.walk(func(n *completionNode) {
		fmt.Fprintf(&b, "    ('%s') reply_subs=(%s); reply_flags=(%s) ;;\n",
			n.path, strings.Join(n.childNames(), " "), strings.Join(n.flags, " "))
	})
	fmt.Fprintf(&b, `  esac
}

_control() {
  emulate -L zsh
  local -a reply_subs reply_flags
  local key='' word
  integer i=2 frozen=0
  while (( i < CURRENT )); do
    word=${words[i]}
    if [[ $word == -- ]]; then
      _files
      return
    fi
    if [[ -z $key ]] && (( ! frozen )); then
      case $word in
        (%s) (( i += 2 )); continue ;;
        (-*) (( i += 1 )); continue ;;
      esac
    fi
    if (( ! frozen )); then
      __control_node "$key"
      if (( ${reply_subs[(Ie)$word]} )); then
        key=${key:+$key }$word
        (( i += 1 ))
        continue
      fi
      [[ -z $key ]] && return 1
      frozen=1
    fi
    (( i += 1 ))
  done
  if (( i > CURRENT )); then
    _files
    return
  fi
  __control_node "$key"
  if [[ ${words[CURRENT]} == -* ]]; then
    compadd -- "${reply_flags[@]}"
  elif (( ! frozen && ${#reply_subs} )); then
    compadd -- "${reply_subs[@]}"
  else
    _files
  fi
}

if [[ ${funcstack[1]-} == _control ]]; then
  _control "$@"
else
  compdef _control control
fi
`, strings.Join(s.valueGlobalForms(), "|"))
	return b.String()
}

func fishCompletion(s completionSpec) string {
	var b strings.Builder
	b.WriteString(`# fish completion for control. Load it with:
#   control completion fish | source
# or save the output as ~/.config/fish/completions/control.fish.

function __control_node --argument-names key
    set -g __control_subs
    set -g __control_flags
    if test -z "$key"
`)
	fmt.Fprintf(&b, "        set -g __control_subs %s\n        set -g __control_flags %s\n        return\n    end\n    switch $key\n",
		strings.Join(s.root.childNames(), " "), strings.Join(s.root.flags, " "))
	s.root.walk(func(n *completionNode) {
		if n.path == "" {
			return
		}
		fmt.Fprintf(&b, "        case '%s'\n            set -g __control_subs %s\n            set -g __control_flags %s\n",
			n.path, strings.Join(n.childNames(), " "), strings.Join(n.flags, " "))
	})
	var globals []string
	for _, option := range s.valueGlobalForms() {
		globals = append(globals, "x"+option)
	}
	fmt.Fprintf(&b, `    end
end

# Returns 0 with __control_key set, 1 when files fit the cursor (a flag
# value or arguments after --), and 2 after an unknown command.
function __control_walk
    set -g __control_key ''
    set -g __control_frozen 0
    set -l words (commandline -opc)
    set -l n (count $words)
    set -l i 2
    while test $i -le $n
        set -l word $words[$i]
        if test "$word" = --
            return 1
        end
        if test -z "$__control_key"; and test $__control_frozen -eq 0
            switch "x$word"
                case %s
                    set i (math $i + 2)
                    continue
                case 'x-*'
                    set i (math $i + 1)
                    continue
            end
        end
        if test $__control_frozen -eq 0
            __control_node $__control_key
            if contains -- $word $__control_subs
                set -g __control_key (string trim -- "$__control_key $word")
                set i (math $i + 1)
                continue
            end
            if test -z "$__control_key"
                return 2
            end
            set -g __control_frozen 1
        end
        set i (math $i + 1)
    end
    if test $i -gt (math $n + 1)
        return 1
    end
    __control_node $__control_key
    return 0
end

function __control_complete
    __control_walk; or return
    if string match -q -- '-*' (commandline -ct)
        printf '%%s\n' $__control_flags
    else if test $__control_frozen -eq 0
        printf '%%s\n' $__control_subs
    end
end

function __control_wants_files
    __control_walk
    set -l walked $status
    if test $walked -eq 1
        return 0
    end
    if test $walked -ne 0
        return 1
    end
    if string match -q -- '-*' (commandline -ct)
        return 1
    end
    test $__control_frozen -eq 1; or test (count $__control_subs) -eq 0
end

complete -c control -e
complete -c control -f -a '(__control_complete)'
complete -c control -n __control_wants_files -F
`, strings.Join(globals, " "))
	return b.String()
}

func powershellCompletion(s completionSpec) string {
	quote := func(words []string) string {
		quoted := make([]string, 0, len(words))
		for _, word := range words {
			quoted = append(quoted, "'"+word+"'")
		}
		return "@(" + strings.Join(quoted, ", ") + ")"
	}
	var b strings.Builder
	b.WriteString(`# PowerShell completion for control. Load it with:
#   control completion powershell | Out-String | Invoke-Expression
# or add that line to your $PROFILE.

Register-ArgumentCompleter -Native -CommandName @('control', 'control.exe') -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $subs = @{
`)
	s.root.walk(func(n *completionNode) {
		fmt.Fprintf(&b, "        '%s' = %s\n", n.path, quote(n.childNames()))
	})
	b.WriteString("    }\n    $flags = @{\n")
	s.root.walk(func(n *completionNode) {
		fmt.Fprintf(&b, "        '%s' = %s\n", n.path, quote(n.flags))
	})
	fmt.Fprintf(&b, `    }
    $valueFlags = %s
    $words = @($commandAst.CommandElements |
        Where-Object { $_.Extent.EndOffset -lt $cursorPosition } |
        ForEach-Object { $_.Extent.Text })
    $key = ''
    $frozen = $false
    $i = 1
    while ($i -lt $words.Count) {
        $word = $words[$i]
        if ($word -eq '--') { return }
        if ($key -eq '' -and -not $frozen) {
            if ($valueFlags -ccontains $word) { $i += 2; continue }
            if ($word.StartsWith('-')) { $i += 1; continue }
        }
        if (-not $frozen) {
            if ($subs[$key] -ccontains $word) {
                $key = ($key + ' ' + $word).Trim()
                $i += 1
                continue
            }
            if ($key -eq '') { return }
            $frozen = $true
        }
        $i += 1
    }
    if ($i -gt $words.Count) { return }
    if ("$wordToComplete".StartsWith('-')) {
        $candidates = $flags[$key]
    } elseif (-not $frozen) {
        $candidates = $subs[$key]
    } else {
        return
    }
    foreach ($candidate in $candidates) {
        if ($candidate.StartsWith("$wordToComplete", [System.StringComparison]::Ordinal)) {
            [System.Management.Automation.CompletionResult]::new($candidate, $candidate, 'ParameterValue', $candidate)
        }
    }
}
`, quote(s.valueGlobalForms()))
	return b.String()
}
