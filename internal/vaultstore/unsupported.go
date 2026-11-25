//go:build !darwin && !linux && !windows

package vaultstore

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/client"
)

// Without a verified owner-and-ACL walk for this platform every protected
// operation fails closed. Windows needs DACL, reparse-point and service-SID
// checks that are not implemented.

func DefaultLayout() (Layout, error) { return Layout{}, ErrUnsupported }

func (l Layout) MarkerPath() string { return filepath.Join(l.Root, MarkerName) }

func Provision(Layout, ProvisionRequest) (Marker, error) { return Marker{}, ErrUnsupported }

func CheckProvision(Layout, string) error { return ErrUnsupported }

type Server struct{}

func OpenServer(Layout) (*Server, error)               { return nil, ErrUnsupported }
func (*Server) Close() error                           { return ErrUnsupported }
func (*Server) Marker() Marker                         { return Marker{} }
func (*Server) Address() string                        { return "" }
func (*Server) Frontend() Principal                    { return Principal{} }
func (*Server) TLSConfig() *tls.Config                 { return nil }
func (*Server) ApprovalKey() ed25519.PublicKey         { return nil }
func (*Server) Salt() []byte                           { return nil }
func (*Server) Account() client.StandaloneConfig       { return client.StandaloneConfig{} }
func (*Server) UpdateAdmin() (client.Admin, bool)      { return client.Admin{}, false }
func (*Server) TunnelRoot() string                     { return "" }
func (*Server) Exposures() []string                    { return nil }
func Import(Layout, io.Reader) (Receipt, error)        { return Receipt{}, ErrUnsupported }
func Rollback(Layout) (Bundle, error)                  { return Bundle{}, ErrUnsupported }
func Purge(Layout) error                               { return ErrUnsupported }
func ExportMigration(string) (Bundle, error)           { return Bundle{}, ErrUnsupported }
func Activate(string, ActivateOptions) (Marker, error) { return Marker{}, ErrUnsupported }
func RestoreLegacy(string, io.Reader) error            { return ErrUnsupported }
func CheckAmbient(string) error                        { return ErrUnsupported }
func FrontendWarnings() []string                       { return []string{ErrUnsupported.Error()} }

// LoadMarker permits legacy mode only when no activation marker exists. An
// activation copied from another platform is refused, never ignored.
func LoadMarker(home string, _ ...string) (Marker, error) {
	_, err := os.Lstat(filepath.Join(home, MarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, fmt.Errorf("no vault: %w", os.ErrNotExist)
	}
	if err != nil {
		return Marker{}, err
	}
	return Marker{}, ErrUnsupported
}
