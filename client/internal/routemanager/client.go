package routemanager

import (
	"context"
	"fmt"
	"net/netip"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/iface"
	"github.com/netbirdio/netbird/route"
)

const minRangeBits = 7

type routerPeerStatus struct {
	connected bool
	relayed   bool
	direct    bool
}

type routesUpdate struct {
	updateSerial uint64
	routes       []*route.Route
}

type clientNetwork struct {
	ctx                 context.Context
	stop                context.CancelFunc
	statusRecorder      *peer.Status
	wgInterface         *iface.WGIface
	routes              map[string]*route.Route
	routeUpdate         chan routesUpdate
	peerStateUpdate     chan struct{}
	routePeersNotifiers map[string]chan struct{}
	chosenRoute         *route.Route
	network             netip.Prefix
	updateSerial        uint64
	isDefault           bool
}

func newClientNetworkWatcher(ctx context.Context, wgInterface *iface.WGIface, statusRecorder *peer.Status, network netip.Prefix) *clientNetwork {
	ctx, cancel := context.WithCancel(ctx)

	isDefault := network == netip.MustParsePrefix("0.0.0.0/0") || network == netip.MustParsePrefix("::/0")

	client := &clientNetwork{
		ctx:                 ctx,
		stop:                cancel,
		statusRecorder:      statusRecorder,
		wgInterface:         wgInterface,
		routes:              make(map[string]*route.Route),
		routePeersNotifiers: make(map[string]chan struct{}),
		routeUpdate:         make(chan routesUpdate),
		peerStateUpdate:     make(chan struct{}),
		network:             network,
		isDefault:           isDefault,
	}
	return client
}

func (c *clientNetwork) getRouterPeerStatuses() map[string]routerPeerStatus {
	routePeerStatuses := make(map[string]routerPeerStatus)
	for _, r := range c.routes {
		peerStatus, err := c.statusRecorder.GetPeer(r.Peer)
		if err != nil {
			log.Debugf("couldn't fetch peer state: %v", err)
			continue
		}
		routePeerStatuses[r.ID] = routerPeerStatus{
			connected: peerStatus.ConnStatus == peer.StatusConnected,
			relayed:   peerStatus.Relayed,
			direct:    peerStatus.Direct,
		}
	}
	return routePeerStatuses
}

// getBestRouteFromStatuses determines the most optimal route from the available routes
// within a clientNetwork, taking into account peer connection status, route metrics, and
// preference for non-relayed and direct connections.
//
// It follows these prioritization rules:
// * Connected peers: Only routes with connected peers are considered.
// * Metric: Routes with lower metrics (better) are prioritized.
// * Non-relayed: Routes without relays are preferred.
// * Direct connections: Routes with direct peer connections are favored.
// * Stability: In case of equal scores, the currently active route (if any) is maintained.
//
// It returns the ID of the selected optimal route.
func (c *clientNetwork) getBestRouteFromStatuses(routePeerStatuses map[string]routerPeerStatus) string {
	chosen := ""
	chosenScore := 0

	currID := ""
	if c.chosenRoute != nil {
		currID = c.chosenRoute.ID
	}

	for _, r := range c.routes {
		tempScore := 0
		peerStatus, found := routePeerStatuses[r.ID]
		if !found || !peerStatus.connected {
			continue
		}

		if r.Metric < route.MaxMetric {
			metricDiff := route.MaxMetric - r.Metric
			tempScore = metricDiff * 10
		}

		if !peerStatus.relayed {
			tempScore++
		}

		if peerStatus.direct {
			tempScore++
		}

		if tempScore > chosenScore || (tempScore == chosenScore && r.ID == currID) {
			chosen = r.ID
			chosenScore = tempScore
		}

		if chosen == "" && currID == "" {
			chosen = r.ID
			chosenScore = tempScore
		}
	}

	if chosen == "" {
		var peers []string
		for _, r := range c.routes {
			peers = append(peers, r.Peer)
		}

		log.Warnf("the network %s has not been assigned a routing peer as no peers from the list %s are currently connected", c.network, peers)

	} else if chosen != currID {
		log.Infof("new chosen route is %s with peer %s with score %d for network %s", chosen, c.routes[chosen].Peer, chosenScore, c.network)
	}

	return chosen
}

func (c *clientNetwork) watchPeerStatusChanges(ctx context.Context, peerKey string, peerStateUpdate chan struct{}, closer chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-closer:
			return
		case <-c.statusRecorder.GetPeerStateChangeNotifier(peerKey):
			state, err := c.statusRecorder.GetPeer(peerKey)
			if err != nil || state.ConnStatus == peer.StatusConnecting {
				continue
			}
			peerStateUpdate <- struct{}{}
			log.Debugf("triggered route state update for Peer %s, state: %s", peerKey, state.ConnStatus)
		}
	}
}

func (c *clientNetwork) startPeersStatusChangeWatcher() {
	for _, r := range c.routes {
		_, found := c.routePeersNotifiers[r.Peer]
		if !found {
			c.routePeersNotifiers[r.Peer] = make(chan struct{})
			go c.watchPeerStatusChanges(c.ctx, r.Peer, c.peerStateUpdate, c.routePeersNotifiers[r.Peer])
		}
	}
}

func (c *clientNetwork) removeRouteFromWireguardPeer(peerKey string) error {
	state, err := c.statusRecorder.GetPeer(peerKey)
	if err != nil {
		return fmt.Errorf("get peer state: %v", err)
	}
	if state.ConnStatus != peer.StatusConnected {
		return nil
	}

	err = c.wgInterface.RemoveAllowedIP(peerKey, c.network.String())
	if err != nil {
		return fmt.Errorf("remove allowed IP %s removed for peer %s, err: %v",
			c.network, c.chosenRoute.Peer, err)
	}
	return nil
}

func (c *clientNetwork) removeRouteFromPeerAndSystem() error {
	if c.chosenRoute != nil {
		if c.isDefault {
			if err := cleanupDefaultRouting(c.wgInterface.Name()); err != nil {
				return fmt.Errorf("cleanup default routing: %v", err)
			}
			log.Infof("Default routing cleanup complete")
		} else if err := removeFromRouteTableIfNonSystem(c.network, c.wgInterface.Address().IP.String()); err != nil {
			return fmt.Errorf("remove route %s from system, err: %v", c.network, err)
		}

		if err := c.removeRouteFromWireguardPeer(c.chosenRoute.Peer); err != nil {
			return fmt.Errorf("remove route: %v", err)
		}
	}
	return nil
}

func (c *clientNetwork) recalculateRouteAndUpdatePeerAndSystem() error {
	routerPeerStatuses := c.getRouterPeerStatuses()

	chosen := c.getBestRouteFromStatuses(routerPeerStatuses)

	// If no route is chosen, remove the route from the peer and system
	if chosen == "" {
		// TODO: fail-close on default route
		if err := c.removeRouteFromPeerAndSystem(); err != nil {
			return fmt.Errorf("remove route from peer and system: %v", err)
		}

		c.chosenRoute = nil

		return nil
	}

	// If the chosen route is the same as the current route, do nothing
	if c.chosenRoute != nil && c.chosenRoute.ID == chosen {
		if c.chosenRoute.IsEqual(c.routes[chosen]) {
			return nil
		}
	}

	if c.chosenRoute != nil {
		// If a previous route exists, remove it from the peer
		if err := c.removeRouteFromWireguardPeer(c.chosenRoute.Peer); err != nil {
			return fmt.Errorf("remove route from peer: %v", err)
		}
	} else {
		// otherwise add the route to the system
		if c.isDefault {
			if err := setupDefaultRouting(c.wgInterface.Name()); err != nil {
				return fmt.Errorf("setup default routing: %v", err)
			}
			log.Infof("Default routing setup complete")
		} else if err := addToRouteTableIfNoExists(c.network, c.wgInterface.Address().IP.String()); err != nil {
			return fmt.Errorf("route %s couldn't be added for peer %s, err: %v",
				c.network.String(), c.wgInterface.Address().IP.String(), err)
		}
	}

	c.chosenRoute = c.routes[chosen]
	if err := c.wgInterface.AddAllowedIP(c.chosenRoute.Peer, c.network.String()); err != nil {
		log.Errorf("couldn't add allowed IP %s added for peer %s, err: %v",
			c.network, c.chosenRoute.Peer, err)
	}

	return nil
}

func (c *clientNetwork) sendUpdateToClientNetworkWatcher(update routesUpdate) {
	go func() {
		c.routeUpdate <- update
	}()
}

func (c *clientNetwork) handleUpdate(update routesUpdate) {
	updateMap := make(map[string]*route.Route)

	for _, r := range update.routes {
		updateMap[r.ID] = r
	}

	for id, r := range c.routes {
		_, found := updateMap[id]
		if !found {
			close(c.routePeersNotifiers[r.Peer])
			delete(c.routePeersNotifiers, r.Peer)
		}
	}

	c.routes = updateMap
}

// peersStateAndUpdateWatcher is the main point of reacting on client network routing events.
// All the processing related to the client network should be done here. Thread-safe.
func (c *clientNetwork) peersStateAndUpdateWatcher() {
	for {
		select {
		case <-c.ctx.Done():
			log.Debugf("stopping watcher for network %s", c.network)
			err := c.removeRouteFromPeerAndSystem()
			if err != nil {
				log.Errorf("Couldn't remove route from peer and system for network %s: %v", c.network, err)
			}
			return
		case <-c.peerStateUpdate:
			err := c.recalculateRouteAndUpdatePeerAndSystem()
			if err != nil {
				log.Errorf("Couldn't recalculate route and update peer and system: %v", err)
			}
		case update := <-c.routeUpdate:
			if update.updateSerial < c.updateSerial {
				log.Warnf("Received a routes update with smaller serial number, ignoring it")
				continue
			}

			log.Debugf("Received a new client network route update for %s", c.network)

			c.handleUpdate(update)

			c.updateSerial = update.updateSerial

			err := c.recalculateRouteAndUpdatePeerAndSystem()
			if err != nil {
				log.Errorf("Couldn't recalculate route and update peer and system for network %s: %v", c.network, err)
			}

			c.startPeersStatusChangeWatcher()
		}
	}
}
