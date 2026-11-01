//go:build (!darwin && !linux && !windows) || (!amd64 && !arm64)

package desktop

import "errors"

func callNative(Request, string, string) ([]byte, error) {
	return nil, errors.New("native desktop requires a supported 64-bit platform")
}
