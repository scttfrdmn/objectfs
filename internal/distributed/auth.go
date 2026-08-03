package distributed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// Message authentication for the gossip protocol (#206).
//
// Every gossip datagram is wrapped in an authEnvelope carrying an HMAC-SHA256 over the exact bytes
// of the inner message. A receiver that cannot verify the MAC drops the datagram before it is
// parsed, so an unauthenticated peer cannot join the cluster, cannot announce cache ownership, and
// cannot be the source of bytes that a reading process sees as file content.
//
// Why HMAC with a shared cluster secret rather than TLS or mTLS: the deployment model is a single
// tenant on a trusted HPC network, and the cost of distributing and rotating per-node certificates
// across compute nodes is what would make the feature never get switched on. A secret distributed
// the same way the rest of the node configuration is distributed is proportionate to the threat —
// a host on the cluster network that is not supposed to be in the cluster — and it is one that an
// operator can actually deploy.
//
// What this does not do: it does not provide confidentiality (gossip payloads are membership
// metadata and cache keys, sent in the clear), and it does not distinguish one cluster member from
// another, because every member holds the same key. A compromised node can impersonate any other
// node. Defending against that needs per-node keys and is a different threat model; it is not
// pretended here.

const (
	// authVersion is the envelope version. It exists so that a future change to the MAC
	// construction can be rejected explicitly rather than as a verification failure, which would
	// otherwise be indistinguishable from a wrong secret.
	authVersion = 1

	// minSecretLen is the shortest cluster secret accepted, in bytes.
	//
	// HMAC-SHA256's security does not improve past a block-length key, but a short secret is
	// almost always a human-chosen one, and the gossip port is reachable by anything on the
	// network. 32 bytes is what `openssl rand -hex 32` produces and what the documentation
	// tells operators to run.
	minSecretLen = 32

	// replayWindow bounds how far a message's timestamp may be from the receiver's clock.
	//
	// It does two jobs: it rejects a captured datagram replayed later, and it bounds the size of
	// the nonce cache, which only has to remember one window's worth of message IDs. 30s is wide
	// enough for the NTP skew of a normal cluster and narrow enough that the cache stays small at
	// the default gossip interval.
	replayWindow = 30 * time.Second
)

// Errors from secret loading and message verification. These are distinguished because they send an
// operator to different places: a missing secret is a deployment step that was not done, a bad MAC
// is a wrong secret or an intruder, and a replay is neither.
var (
	// ErrNoClusterSecret means clustering is enabled and no secret was found. Fail closed: a
	// cluster that silently runs unauthenticated is the defect this exists to prevent.
	ErrNoClusterSecret = errors.New("no cluster secret configured")

	// ErrSecretTooShort means a secret was found but is too short to be a generated one.
	ErrSecretTooShort = errors.New("cluster secret too short")

	// ErrSecretPermissions means the secret file is readable by users other than its owner.
	ErrSecretPermissions = errors.New("cluster secret file is readable by other users")

	// ErrUnauthenticated means the MAC did not verify. The message is dropped unparsed.
	ErrUnauthenticated = errors.New("gossip message failed authentication")

	// ErrReplayed means the MAC verified but the message is a duplicate or outside the freshness
	// window. Distinct from ErrUnauthenticated because it is not evidence of a wrong secret.
	ErrReplayed = errors.New("gossip message replayed or outside the freshness window")

	// ErrUnknownAuthVersion means the envelope version is not one this build understands, which is
	// a version skew during a rolling upgrade rather than an authentication failure.
	ErrUnknownAuthVersion = errors.New("unknown gossip envelope version")
)

// ClusterSecretEnv is the environment variable read for the cluster secret.
//
// Environment and file are the only two sources. A YAML field is deliberately not one: the shipped
// example configuration is installed to /etc/objectfs/config.yaml by the packaging scripts and is
// world-readable, so a secret field there would be a secret published to every user on the node.
const ClusterSecretEnv = "OBJECTFS_CLUSTER_SECRET"

// authEnvelope wraps a gossip message with its MAC.
//
// Payload is json.RawMessage rather than a decoded GossipMessage so that the MAC is computed and
// verified over the exact bytes that crossed the network. Re-marshaling to verify would make the
// check depend on Go's map ordering and number formatting agreeing byte-for-byte with the sender's
// — true today between identical builds, and a silent cluster-wide authentication failure the first
// time it is not.
type authEnvelope struct {
	Version int             `json:"v"`
	MAC     string          `json:"mac"`
	Payload json.RawMessage `json:"msg"`
}

// LoadClusterSecret reads the cluster secret from the environment or from a file.
//
// The environment takes precedence, because that is what a container orchestrator injects and it
// avoids a file that has to be mounted. secretFile may be empty, in which case only the environment
// is consulted.
//
// Returned errors name which source was tried and what was wrong with it. "no cluster secret
// configured" naming both candidates is the difference between an operator finding the missing step
// and an operator reading "failed to start cluster".
func LoadClusterSecret(secretFile string) ([]byte, error) {
	if env := os.Getenv(ClusterSecretEnv); env != "" {
		secret := []byte(strings.TrimSpace(env))
		if len(secret) < minSecretLen {
			return nil, fmt.Errorf("%w: %s holds %d bytes, need at least %d "+
				"(generate one with: openssl rand -hex 32)",
				ErrSecretTooShort, ClusterSecretEnv, len(secret), minSecretLen)
		}

		return secret, nil
	}

	if secretFile == "" {
		return nil, fmt.Errorf("%w: set %s or cluster.secret_file. Clustering will not start "+
			"without one, because an unauthenticated gossip port lets any host on the network "+
			"join the cluster and announce ownership of cached objects",
			ErrNoClusterSecret, ClusterSecretEnv)
	}

	info, err := os.Stat(secretFile)
	if err != nil {
		return nil, fmt.Errorf("reading cluster secret file %s: %w", secretFile, err)
	}

	// A secret readable by every user on the node is not a secret. This is checked rather than
	// fixed, because silently chmod-ing a file the operator placed is worse than refusing it.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is mode %#o, want 0600 (chmod 600 %s)",
			ErrSecretPermissions, secretFile, perm, secretFile)
	}

	raw, err := os.ReadFile(secretFile) //nolint:gosec // the path is operator-supplied configuration
	if err != nil {
		return nil, fmt.Errorf("reading cluster secret file %s: %w", secretFile, err)
	}

	// Trailing newlines are what every editor and `openssl rand -hex 32 > file` leave behind. A
	// secret that depends on whether the file ends in \n is a support case, not a security
	// property.
	secret := []byte(strings.TrimSpace(string(raw)))
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("%w: %s holds %d bytes, need at least %d "+
			"(generate one with: openssl rand -hex 32 > %s && chmod 600 %s)",
			ErrSecretTooShort, secretFile, len(secret), minSecretLen, secretFile, secretFile)
	}

	return secret, nil
}

// messageAuthenticator signs and verifies gossip datagrams.
//
// It holds the nonce cache, so it is per-node state rather than a pure function of the secret.
type messageAuthenticator struct {
	secret []byte

	mu   sync.Mutex
	seen map[string]time.Time // message ID -> when it was accepted

	// now is the clock, injectable so that freshness and eviction are testable without sleeping.
	now func() time.Time
}

// newMessageAuthenticator returns an authenticator for secret.
func newMessageAuthenticator(secret []byte) *messageAuthenticator {
	return &messageAuthenticator{
		secret: secret,
		seen:   make(map[string]time.Time),
		now:    time.Now,
	}
}

// mac computes the HMAC-SHA256 of payload, hex-encoded.
func (a *messageAuthenticator) mac(payload []byte) string {
	h := hmac.New(sha256.New, a.secret)
	h.Write(payload)

	return hex.EncodeToString(h.Sum(nil))
}

// seal marshals msg and wraps it in an authenticated envelope ready to send.
func (a *messageAuthenticator) seal(msg *GossipMessage) ([]byte, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling gossip message: %w", err)
	}

	return json.Marshal(authEnvelope{
		Version: authVersion,
		MAC:     a.mac(payload),
		Payload: payload,
	})
}

// open verifies a received datagram and returns the message it carries.
//
// Order matters and is deliberate: the MAC is checked before the payload is parsed, so a datagram
// from an unauthenticated source never reaches the JSON decoding of any message type, let alone a
// handler. Replay is checked after the MAC, because the nonce cache must not be writable by an
// unauthenticated sender — otherwise flooding it with random message IDs would evict the real
// entries and re-open the replay window.
func (a *messageAuthenticator) open(data []byte) (*GossipMessage, error) {
	var env authEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: not an authenticated envelope: %w", ErrUnauthenticated, err)
	}

	if env.Version != authVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnknownAuthVersion, env.Version, authVersion)
	}

	want, err := hex.DecodeString(env.MAC)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed MAC: %w", ErrUnauthenticated, err)
	}

	got, err := hex.DecodeString(a.mac(env.Payload))
	if err != nil {
		// Unreachable: mac returns hex it encoded itself.
		return nil, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}

	if !hmac.Equal(got, want) {
		return nil, ErrUnauthenticated
	}

	var msg GossipMessage
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		return nil, fmt.Errorf("%w: authenticated payload is not a gossip message: %w",
			ErrUnauthenticated, err)
	}

	if err := a.checkFresh(&msg); err != nil {
		return nil, err
	}

	return &msg, nil
}

// checkFresh enforces the freshness window and rejects duplicate message IDs.
//
// A MAC authenticates the sender, not the moment. Without this, a captured "node N owns key K" —
// or a captured "node N is dead" — stays valid forever and can be replayed to undo the state that
// replaced it. That is the case the issue singles out, because an ownership announcement is what a
// cache-warming read would follow.
//
// Why a timestamp window plus a nonce cache rather than a per-node monotonic sequence: a sequence
// has to survive restarts to be monotonic, so a restarted node either starts from persisted state
// — which is the fsync-before-ack obligation that per-key conditional writes exist to avoid — or
// starts from zero and is rejected by every peer until they age it out. A window bounds the cache
// without any persistent state, and a restart is indistinguishable from a normal message.
func (a *messageAuthenticator) checkFresh(msg *GossipMessage) error {
	now := a.now()

	skew := now.Sub(msg.Timestamp)
	if skew < 0 {
		skew = -skew
	}

	if skew > replayWindow {
		return fmt.Errorf("%w: timestamp is %s from local time (window %s)",
			ErrReplayed, skew.Round(time.Millisecond), replayWindow)
	}

	// An empty message ID cannot be deduplicated, so it cannot be accepted — otherwise omitting
	// the field is a way to opt out of replay protection while still passing the MAC check.
	if msg.MessageID == "" {
		return fmt.Errorf("%w: message carries no ID, so a replay of it could not be detected",
			ErrReplayed)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Evict anything older than the window. Entries outside it are already rejected on timestamp,
	// so remembering them adds nothing but memory.
	for id, at := range a.seen {
		if now.Sub(at) > replayWindow {
			delete(a.seen, id)
		}
	}

	if _, dup := a.seen[msg.MessageID]; dup {
		return fmt.Errorf("%w: message %s was already accepted", ErrReplayed, msg.MessageID)
	}

	a.seen[msg.MessageID] = now

	return nil
}

// secretFilePermissionsOK reports whether mode is safe for a secret file. Split out so the rule is
// stated once and can be asserted directly by a test.
func secretFilePermissionsOK(mode fs.FileMode) bool {
	return mode.Perm()&0o077 == 0
}
