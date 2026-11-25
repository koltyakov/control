//go:build !linux && !darwin && !windows

package shellpty

import "context"

// Terminal is a running shell attached to a pseudo-terminal. This platform
// has no backend, so Start always fails.
type Terminal struct{}

// Start reports ErrUnsupportedPlatform after validating its arguments.
func Start(_ context.Context, name string, cols, rows uint16, term, dir string) (*Terminal, error) {
	if _, _, err := validateStart(name, cols, rows, term, dir); err != nil {
		return nil, err
	}
	return nil, ErrUnsupportedPlatform
}

// Read is unreachable because Start never returns a Terminal.
func (*Terminal) Read([]byte) (int, error) { return 0, ErrUnsupportedPlatform }

// Write is unreachable because Start never returns a Terminal.
func (*Terminal) Write([]byte) (int, error) { return 0, ErrUnsupportedPlatform }

// Resize is unreachable because Start never returns a Terminal.
func (*Terminal) Resize(uint16, uint16) error { return ErrUnsupportedPlatform }

// Wait is unreachable because Start never returns a Terminal.
func (*Terminal) Wait() (int, error) { return -1, ErrUnsupportedPlatform }

// Done is unreachable because Start never returns a Terminal.
func (*Terminal) Done() <-chan struct{} { return nil }

// Close is unreachable because Start never returns a Terminal.
func (*Terminal) Close() error { return nil }
