package clipboard

import (
	"context"
	"os/exec"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "pbcopy"), nil
}

func read(ctx context.Context) (Value, error) {
	// Foundation exposes all copied Finder URLs, unlike AppleScript's single-file coercion.
	return readJSON(exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", clipboardReadScript))
}

const clipboardReadScript = `
ObjC.import('AppKit');
var p = $.NSPasteboard.generalPasteboard;
var classes = $.NSArray.arrayWithObject($.NSURL.class);
var urls = p.readObjectsForClassesOptions(classes, $.NSDictionary.dictionary);
var paths = [];
if (urls) {
  for (var i = 0; i < urls.count; i++) {
    var u = urls.objectAtIndex(i);
    if (!u.isFileURL) throw new Error('Clipboard contains a non-file URL');
    paths.push(ObjC.unwrap(u.path));
  }
}
var text = ObjC.unwrap(p.stringForType($.NSPasteboardTypeString));
if (!paths.length && typeof text !== 'string') throw new Error('Clipboard has no text or regular files');
JSON.stringify({paths: paths, text: paths.length ? '' : text});`
