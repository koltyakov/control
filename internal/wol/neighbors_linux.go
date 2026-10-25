package wol

import "context"

func readNeighbors(ctx context.Context) ([]neighbor, error) {
	data, err := neighborCommand(ctx, "ip", "-j", "neigh", "show")
	if err != nil {
		return nil, err
	}
	return parseLinuxNeighbors(data)
}
