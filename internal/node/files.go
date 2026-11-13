package node

import (
	"errors"
	"os"
)

// openRegularFile checks the opened handle, not a racy pre-open stat. Unix
// opens are nonblocking so a FIFO cannot hang a file RPC or task completion.
func openRegularFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	return openRegular(root, name, regularReadFlags, 0)
}

func openRegular(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(name, flags, mode)
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

func openDirectory(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, directoryReadFlags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.IsDir() {
		err = errors.New("path must be a directory")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Never pass O_TRUNC to open: validate the actual handle before changing data.
// Unix opens are nonblocking so an existing FIFO cannot wait for a reader.
func openRegularFileForWrite(root *os.Root, name string, truncate bool) (*os.File, error) {
	f, _, err := openRegular(root, name, regularWriteFlags, 0600)
	if err != nil {
		return nil, err
	}
	if truncate {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}
