package node

import (
	"errors"
	"net/http"

	"github.com/koltyakov/control/internal/secrets"
)

// headerSecret is one headerSecrets entry of http.request or http.download.
type headerSecret struct {
	Secret string `json:"secret"`
	Prefix string `json:"prefix"`
}

// applyHeaderSecrets checks every reference against its use policy for the URL
// the client will connect to, then sets the headers. A refused or conflicting
// reference sets no header, so the caller sends nothing. Secret-bearing
// requests must not follow redirects: the policy covers req.URL only.
func applyHeaderSecrets(req *http.Request, resolver *secrets.Resolver, refs map[string]headerSecret) error {
	list := make([]secrets.Reference, 0, len(refs))
	for k, ref := range refs {
		if req.Header.Values(k) != nil {
			return errors.New("header cannot have both a literal value and a secret reference")
		}
		list = append(list, secrets.Reference{Name: ref.Secret, Use: secrets.UseHTTP, URL: req.URL})
	}
	values, err := resolver.Resolve(list)
	if err != nil {
		return err
	}
	for k, ref := range refs {
		req.Header.Set(k, ref.Prefix+values[ref.Secret])
	}
	return nil
}
