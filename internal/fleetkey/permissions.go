// Package fleetkey maps stored credential roles to the permissions checked by
// the gateway and its clients. Account and operator roles are never issuable
// through a named key's permission list.
package fleetkey

type Permissions uint8

const (
	Discover Permissions = 1 << iota
	Enroll
	Run
	ManageFleet
	ManageGateway
)

var named = map[string]Permissions{
	"discovery":                Discover,
	"common":                   Discover | Enroll,
	"orchestrator":             Discover | Run,
	"orchestrator-enroll":      Discover | Enroll | Run,
	"fleet-manager":            Discover | ManageFleet,
	"fleet-manager-enroll":     Discover | Enroll | ManageFleet,
	"fleet-manager-run":        Discover | Run | ManageFleet,
	"fleet-manager-enroll-run": Discover | Enroll | Run | ManageFleet,
}

var labels = [...]struct {
	bit  Permissions
	name string
}{
	{Discover, "discover"}, {Enroll, "enroll"}, {Run, "run"},
	{ManageFleet, "manage-fleet"}, {ManageGateway, "manage-gateway"},
}

func ForRole(role string) Permissions {
	switch role {
	case "superuser":
		return Discover | Enroll | Run | ManageFleet | ManageGateway
	case "user":
		return Discover | Enroll | Run | ManageFleet
	}
	return named[role]
}

func (p Permissions) Has(required Permissions) bool { return p&required == required }

func (p Permissions) Names() []string {
	var names []string
	for _, label := range labels {
		if p.Has(label.bit) {
			names = append(names, label.name)
		}
	}
	return names
}

func NamedRole(names []string) (string, bool) {
	var p Permissions
	for _, name := range names {
		var bit Permissions
		for _, label := range labels {
			if label.name == name {
				bit = label.bit
				break
			}
		}
		if bit == 0 || p.Has(bit) {
			return "", false
		}
		p |= bit
	}
	for role, allowed := range named {
		if p == allowed {
			return role, true
		}
	}
	return "", false
}
