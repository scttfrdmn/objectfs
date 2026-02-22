package redis

import (
	"context"
	"log"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

const invalidationChannel = "objectfs:invalidation"

// Invalidator uses Redis pub/sub to propagate cache-key invalidations across cluster nodes.
// Each node publishes a message when it evicts a key; all other nodes delete that key
// from their local cache upon receipt.
type Invalidator struct {
	client *goredis.Client
	nodeID string
	cache  *Cache
}

// NewInvalidator creates an Invalidator backed by the given Redis client.
// nodeID uniquely identifies this cluster node so self-published messages are skipped.
func NewInvalidator(client *goredis.Client, nodeID string, cache *Cache) *Invalidator {
	return &Invalidator{client: client, nodeID: nodeID, cache: cache}
}

// Publish broadcasts an invalidation for key to all nodes.
// The message payload is "<nodeID>:<key>".
func (inv *Invalidator) Publish(ctx context.Context, key string) error {
	return inv.client.Publish(ctx, invalidationChannel, inv.nodeID+":"+key).Err()
}

// Subscribe starts a goroutine that listens for invalidation messages on the channel
// and deletes the corresponding key from the local cache. It returns when ctx is cancelled.
func (inv *Invalidator) Subscribe(ctx context.Context) {
	sub := inv.client.Subscribe(ctx, invalidationChannel)
	go func() {
		defer func() { _ = sub.Close() }()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				inv.handle(msg.Payload)
			}
		}
	}()
}

// handle processes a single invalidation message payload.
func (inv *Invalidator) handle(payload string) {
	idx := strings.IndexByte(payload, ':')
	if idx < 0 {
		log.Printf("redis invalidation: malformed message %q", payload)
		return
	}
	senderID := payload[:idx]
	key := payload[idx+1:]
	if senderID == inv.nodeID {
		return // skip our own broadcasts — we already deleted locally
	}
	if inv.cache != nil {
		inv.cache.Delete(key)
	}
}
