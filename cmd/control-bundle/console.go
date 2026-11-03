package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Strip comments, including JSDoc types, only from the copies embedded by Go.
// Keep top-level names and classic-script scope; do not bundle or mangle them.
func consoleOverlay(dir string) (string, error) {
	pattern, err := filepath.Abs("internal/gateway/consoleweb/*.js")
	if err != nil {
		return "", err
	}
	files, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no console JavaScript found at %s", pattern)
	}
	replacements := make(map[string]string, len(files))
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		result := api.Transform(string(source), api.TransformOptions{
			Loader: api.LoaderJS, Target: api.ESNext, Sourcefile: file,
			LegalComments: api.LegalCommentsNone, IgnoreAnnotations: true, Charset: api.CharsetUTF8,
			MinifyWhitespace: true,
		})
		if len(result.Errors) != 0 {
			messages := api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
			return "", fmt.Errorf("strip console comments: %s", strings.Join(messages, "\n"))
		}
		output := filepath.Join(dir, filepath.Base(file))
		if err := os.WriteFile(output, result.Code, 0600); err != nil {
			return "", err
		}
		replacements[file] = output
	}
	data, err := json.Marshal(struct{ Replace map[string]string }{replacements})
	if err != nil {
		return "", err
	}
	overlay := filepath.Join(dir, "overlay.json")
	return overlay, os.WriteFile(overlay, data, 0600)
}
