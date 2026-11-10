package node

import (
	"errors"
	"os"
)

// openRegularFile checks the opened handle, not a racy pre-open stat. Unix
// opens are nonblocking so a FIFO cannot hang a file RPC or task completion.
func openRegularFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("path must be a regular file")
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
