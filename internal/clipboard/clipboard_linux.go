package clipboard

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if path, err := exec.LookPath("wl-copy"); err == nil {
			return exec.CommandContext(ctx, path), nil
		}
	}
	if os.Getenv("DISPLAY") != "" {
		if path, err := exec.LookPath("xclip"); err == nil {
			return exec.CommandContext(ctx, path, "-selection", "clipboard"), nil
		}
		if path, err := exec.LookPath("xsel"); err == nil {
			return exec.CommandContext(ctx, path, "--clipboard", "--input"), nil
		}
	}
	return nil, errors.New("clipboard unavailable; install wl-copy, xclip, or xsel in a desktop session")
}

func read(ctx context.Context) (Value, error) {
	var types, files, text *exec.Cmd
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if binary, err := exec.LookPath("wl-paste"); err == nil {
			types = exec.CommandContext(ctx, binary, "--list-types")
			files = exec.CommandContext(ctx, binary, "--no-newline", "--type", "text/uri-list")
			text = exec.CommandContext(ctx, binary, "--no-newline", "--type", "text")
		}
	}
	if types == nil && os.Getenv("DISPLAY") != "" {
		if binary, err := exec.LookPath("xclip"); err == nil {
			types = exec.CommandContext(ctx, binary, "-selection", "clipboard", "-o", "-t", "TARGETS")
			files = exec.CommandContext(ctx, binary, "-selection", "clipboard", "-o", "-t", "text/uri-list")
			text = exec.CommandContext(ctx, binary, "-selection", "clipboard", "-o")
		} else if binary, err := exec.LookPath("xsel"); err == nil {
			b, err := output(exec.CommandContext(ctx, binary, "--clipboard", "--output"))
			return Value{Text: string(b)}, err
		}
	}
	if types == nil {
		return Value{}, errors.New("clipboard unavailable; install wl-paste or xclip in a desktop session")
	}
	b, err := output(types)
	if err != nil {
		return Value{}, err
	}
	for _, mime := range strings.Fields(string(b)) {
		if mime == "text/uri-list" {
			b, err := output(files)
			if err != nil {
				return Value{}, err
			}
			paths, err := parseURIs(string(b))
			return Value{Paths: paths}, err
		}
	}
	b, err = output(text)
	return Value{Text: string(b)}, err
}

func parseURIs(text string) ([]string, error) {
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !strings.HasPrefix(u.Path, "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("clipboard URI must be a local file")
		}
		paths = append(paths, u.Path)
	}
	if len(paths) == 0 {
		return nil, errors.New("clipboard has no file references")
	}
	return paths, nil
}
