package wol

import "context"

func readNeighbors(ctx context.Context) ([]neighbor, error) {
	data, err := neighborCommand(ctx, "/usr/sbin/arp", "-an")
	if err != nil {
		return nil, err
	}
	return parseUnixNeighbors(data), nil
}
