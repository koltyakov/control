package node

import (
	"path/filepath"
	"strings"
)

// browserScreenshotArgs resolves a direct screenshot's explicit output path
// against this invocation, not the persistent daemon's original directory.
// Match upstream 0.41.2 positional/flag rules without restricting command
// forwarding. Leave batches and ambiguous/new syntax untouched.
func browserScreenshotArgs(args []string, dir string) []string {
	command := false
	var positional []int
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--session", "--restore-save", "--restore-check-url", "--restore-check-text", "--restore-check-fn",
			"--namespace", "--headers", "--executable-path", "--cdp", "--cdp-headers", "--extension",
			"--init-script", "--enable", "--profile", "--state", "--proxy", "--proxy-bypass", "--args",
			"--user-agent", "-p", "--provider", "--device", "--session-name", "--color-scheme",
			"--download-path", "--max-output", "--allowed-domains", "--action-policy", "--confirm-actions",
			"--config", "--engine", "--input-mode", "--screenshot-dir", "--screenshot-quality",
			"--screenshot-format", "--idle-timeout", "--ca-cert", "--model":
			i++
		case "--json", "--headed", "--webgpu", "--no-webmcp", "--debug", "--ignore-https-errors",
			"--allow-file-access", "--hide-scrollbars", "--auto-connect", "--pin-tab", "--no-pin-tab",
			"--no-ca-cert", "--use-system-ca", "--annotate", "--content-boundaries", "--confirm-interactive",
			"--no-auto-dialog", "-v", "--verbose", "-q", "--quiet", "--offline", "--quick", "--fix":
			if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
				i++
			}
		case "--full", "-f", "--if-changed":
			if !command {
				return args
			}
		case "--threshold":
			if !command {
				return args
			}
			i++
		default:
			if strings.HasPrefix(arg, "--restore=") {
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return args
			}
			if !command {
				if arg != "screenshot" {
					return args
				}
				command = true
			} else {
				positional = append(positional, i)
			}
		}
	}
	if len(positional) == 0 {
		return args
	}
	index := positional[0]
	if len(positional) > 1 {
		index = positional[1]
	} else {
		path := args[index]
		relative := strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
		selector := !relative && (strings.HasPrefix(path, ".") || strings.HasPrefix(path, "#") || strings.HasPrefix(path, "@"))
		image := strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg") || strings.HasSuffix(path, ".webp")
		if selector || (!relative && !strings.Contains(path, "/") && !image) {
			return args
		}
	}
	if filepath.IsAbs(args[index]) {
		return args
	}
	path, err := filepath.Abs(filepath.Join(dir, args[index]))
	if err != nil {
		return args
	}
	result := append([]string(nil), args...)
	result[index] = path
	return result
}
