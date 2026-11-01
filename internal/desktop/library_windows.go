//go:build windows && (amd64 || arm64)

package desktop

import "golang.org/x/sys/windows"

func openNativeLibrary(path string) (func(string) (uintptr, error), func(), error) {
	handle, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	if err != nil {
		return nil, nil, err
	}
	return func(name string) (uintptr, error) { return windows.GetProcAddress(handle, name) },
		func() { _ = windows.FreeLibrary(handle) }, nil
}
