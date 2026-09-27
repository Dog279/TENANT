package peering

import (
	"context"

	"tenant/internal/logs/catalog"
)

// The Security log's peering records (docs/design/logging-v2.md §10.1): a
// peer record gaining or losing a live token, and a refused token. The
// store has no context to attribute them from: a pairing or an unpairing is
// the operator's (an inbound pairing was approved at their prompt).

func paired(peer, instance string, dial bool) {
	direction := "inbound"
	if dial {
		direction = "outbound"
	}
	_ = catalog.PeerPaired.Audit(context.Background(),
		"peer", peer, "instance", instance, "direction", direction, "actor", "operator", "surface", "peering")
}

func unpaired(peer, how string) {
	_ = catalog.PeerUnpaired.Audit(context.Background(), "peer", peer, "how", how, "actor", "operator", "surface", "peering")
}
