package gubernator

import (
	"context"
	"github.com/mailgun/holster"
	"github.com/sirupsen/logrus"
	"sync/atomic"
	"time"
)

// globalManager manages async hit queue and updates peers in
// the cluster periodically when a global rate limit we own updates.
type globalManager struct {
	stats          ServerStats
	asyncQueue     chan *RateLimitReq
	broadcastQueue chan *RateLimitReq
	wg             holster.WaitGroup
	conf           BehaviorConfig
	log            *logrus.Entry
	instance       *Instance
}

func newGlobalManager(conf BehaviorConfig, instance *Instance) *globalManager {
	gm := globalManager{
		log:            log.WithField("category", "global-manager"),
		asyncQueue:     make(chan *RateLimitReq, 0),
		broadcastQueue: make(chan *RateLimitReq, 0),
		instance:       instance,
		conf:           conf,
	}
	gm.runAsyncHits()
	gm.runBroadcasts()
	return &gm
}

func (gm *globalManager) QueueHit(r *RateLimitReq) {
	gm.asyncQueue <- r
}

func (gm *globalManager) QueueUpdate(r *RateLimitReq) {
	gm.broadcastQueue <- r
}

func (gm *globalManager) Stats(clear bool) ServerStats {
	if clear {
		defer func() {
			atomic.StoreInt64(&gm.stats.AsyncGlobalsCount, 0)
			atomic.StoreInt64(&gm.stats.BroadcastDuration, 0)
		}()
	}
	return ServerStats{
		AsyncGlobalsCount: atomic.LoadInt64(&gm.stats.AsyncGlobalsCount),
		BroadcastDuration: atomic.LoadInt64(&gm.stats.BroadcastDuration),
	}
}

// runAsyncHits collects async hit requests and queues them to
// be sent to their owning peers.
func (gm *globalManager) runAsyncHits() {
	var interval = NewInterval(gm.conf.GlobalSyncWait)
	hits := make(map[string]*RateLimitReq)

	gm.wg.Until(func(done chan struct{}) bool {
		select {
		case r := <-gm.asyncQueue:
			// Aggregate the hits into a single request
			key := r.HashKey()
			_, ok := hits[key]
			if ok {
				hits[key].Hits += r.Hits
			} else {
				hits[key] = r
			}

			// Send the hits if we reached our batch limit
			if len(hits) == gm.conf.GlobalBatchLimit {
				gm.sendHits(hits)
				hits = make(map[string]*RateLimitReq)
				return true
			}

			// If this is our first queued hit since last send
			// queue the next interval
			if len(hits) == 1 {
				interval.Next()
			}

		case <-interval.C:
			if len(hits) != 0 {
				gm.sendHits(hits)
				hits = make(map[string]*RateLimitReq)
			}
		case <-done:
			return false
		}
		return true
	})
}

// sendHits takes the hits collected by runAsyncHits and sends them to their
// owning peers
func (gm *globalManager) sendHits(hits map[string]*RateLimitReq) {
	type pair struct {
		client *PeerClient
		req    GetPeerRateLimitsReq
	}
	peerRequests := make(map[string]*pair)

	// Assign each request to a peer
	for _, r := range hits {
		peer, err := gm.instance.GetPeer(r.HashKey())
		if err != nil {
			gm.log.WithError(err).Errorf("while getting peer for hash key '%s'", r.HashKey())
			continue
		}

		p, ok := peerRequests[peer.host]
		if ok {
			p.req.Requests = append(p.req.Requests, r)
		} else {
			peerRequests[peer.host] = &pair{
				client: peer,
				req:    GetPeerRateLimitsReq{Requests: []*RateLimitReq{r}},
			}
		}
	}

	// Send the rate limit requests to their respective owning peers.
	for _, p := range peerRequests {
		ctx, cancel := context.WithTimeout(context.Background(), gm.conf.GlobalTimeout)
		_, err := p.client.GetPeerRateLimits(ctx, &p.req)
		cancel()

		if err != nil {
			gm.log.WithError(err).
				Errorf("error sending global hits to '%s'", p.client.host)
			continue
		}
	}
	atomic.AddInt64(&gm.stats.AsyncGlobalsCount, int64(len(hits)))
}

// runBroadcasts collects status changes for global rate limits and broadcasts the changes to each peer in the cluster.
func (gm *globalManager) runBroadcasts() {
	var interval = NewInterval(gm.conf.GlobalSyncWait)
	updates := make(map[string]*RateLimitReq)

	gm.wg.Until(func(done chan struct{}) bool {
		select {
		case r := <-gm.broadcastQueue:
			updates[r.HashKey()] = r

			// Send the hits if we reached our batch limit
			if len(updates) == gm.conf.GlobalBatchLimit {
				gm.updatePeers(updates)
				updates = make(map[string]*RateLimitReq)
				return true
			}

			// If this is our first queued hit since last send
			// queue the next interval
			if len(updates) == 1 {
				interval.Next()
			}

		case <-interval.C:
			if len(updates) != 0 {
				gm.updatePeers(updates)
				updates = make(map[string]*RateLimitReq)
			}
		case <-done:
			return false
		}
		return true
	})
}

// updatePeers broadcasts global rate limit statuses to all other peers
func (gm *globalManager) updatePeers(updates map[string]*RateLimitReq) {
	var req UpdatePeerGlobalsReq
	start := time.Now()

	for _, rl := range updates {
		// We are only sending the status of the rate limit so
		// we clear the behavior flag so we don't get queued for update again.
		rl.Behavior = 0
		rl.Hits = 0

		status, err := gm.instance.getRateLimit(rl)
		if err != nil {
			gm.log.WithError(err).Errorf("while sending global updates to peers for: '%s'", rl.HashKey())
			continue
		}
		// Build an UpdatePeerGlobalsReq
		req.Globals = append(req.Globals, &UpdatePeerGlobal{
			Key:    rl.HashKey(),
			Status: status,
		})
	}

	for _, peer := range gm.instance.GetPeerList() {
		// Exclude ourselves from the update
		if peer.isOwner {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), gm.conf.GlobalTimeout)
		_, err := peer.UpdatePeerGlobals(ctx, &req)
		cancel()

		if err != nil {
			gm.log.WithError(err).Errorf("error sending global updates to '%s'", peer.host)
			continue
		}
	}

	duration := int64(time.Now().Sub(start))
	if atomic.LoadInt64(&gm.stats.BroadcastDuration) < duration {
		atomic.StoreInt64(&gm.stats.BroadcastDuration, duration)
	}
}