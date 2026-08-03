package distributed

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The bar these tests hold the implementation to is #206's own: a host that does not hold the
// cluster secret must not be able to join the cluster or announce ownership of cached objects, and a
// captured message must not stay valid forever. So the assertions are on observable cluster state —
// "node-b is not in the nodes map" — and not only on the error returned by the verifier. An
// authenticator that rejects correctly while a handler runs anyway would pass the second kind of
// test and fail the first, and the handler is what changes what a reading process sees.

// testSecret is a secret of exactly the minimum accepted length. Fixed rather than random so a
// failure reproduces.
func testSecret() []byte { return []byte(strings.Repeat("k", minSecretLen)) }

// gossipForAuth returns a gossip protocol wired to a cluster manager, with no socket open.
//
// handleIncomingMessage is called directly rather than over UDP: the datagram's path from the socket
// to the handler is one function call, and driving it directly makes the test deterministic and
// independent of the loopback network, port availability, and delivery timing.
func gossipForAuth(t *testing.T, nodeID string) *GossipProtocol {
	t.Helper()

	cm, err := NewClusterManager(testConfig(t, nodeID))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	return cm.gossip
}

// joinDatagram builds a join message announcing nodeID, sealed with secret.
func joinDatagram(t *testing.T, secret []byte, nodeID string) []byte {
	t.Helper()

	payload, err := json.Marshal(JoinMessage{
		Node: &NodeInfo{
			ID:       nodeID,
			Address:  "10.0.0.99:8080",
			Status:   NodeStatusAlive,
			LastSeen: time.Now(),
			Metadata: map[string]string{},
		},
		Incarnation: 1,
	})
	if err != nil {
		t.Fatalf("marshaling the join payload: %v", err)
	}

	msg := &GossipMessage{
		Type:      MessageTypeJoin,
		From:      nodeID,
		Data:      payload,
		Timestamp: time.Now(),
		MessageID: "join-" + nodeID,
	}

	data, err := newMessageAuthenticator(secret).seal(msg)
	if err != nil {
		t.Fatalf("sealing the join message: %v", err)
	}

	return data
}

// TestUnauthenticatedJoinIsRejected is the defect in #206 stated as a test: before authentication,
// any host that could reach the gossip port could add itself to the cluster.
func TestUnauthenticatedJoinIsRejected(t *testing.T) {
	t.Parallel()

	gp := gossipForAuth(t, "node-a")
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 99), Port: 8080}

	// Sealed with a different secret — an attacker who knows the wire format but not the key.
	gp.handleIncomingMessage(joinDatagram(t, []byte(strings.Repeat("x", minSecretLen)), "intruder"), addr)

	if _, joined := gp.cluster.GetNodes()["intruder"]; joined {
		t.Error("a node holding the wrong cluster secret joined the cluster")
	}

	// An unsealed message must fare no better: the envelope itself is mandatory, so a peer speaking
	// the pre-authentication wire format is not accepted either. It is counted as a version
	// mismatch rather than an authentication failure, and deliberately so — a bare GossipMessage
	// carries no "v" field, so it decodes as version 0, and a peer sending one is running a build
	// from before authentication existed. During a rolling upgrade that is what an operator needs
	// to be told; "wrong secret" would send them to check a secret that is fine.
	bare, err := json.Marshal(&GossipMessage{
		Type:      MessageTypeJoin,
		From:      "plaintext",
		Timestamp: time.Now(),
		MessageID: "plaintext-join",
	})
	if err != nil {
		t.Fatalf("marshaling the bare message: %v", err)
	}

	gp.handleIncomingMessage(bare, addr)

	if _, joined := gp.cluster.GetNodes()["plaintext"]; joined {
		t.Error("a node sending an unauthenticated message joined the cluster")
	}

	stats := gp.GetStats()
	if stats.MessagesRejected != 2 {
		t.Errorf("MessagesRejected = %d, want 2", stats.MessagesRejected)
	}
	if stats.MessagesUnauthenticated != 1 {
		t.Errorf("MessagesUnauthenticated = %d, want 1 (the wrong-secret join)", stats.MessagesUnauthenticated)
	}
	if stats.MessagesWrongVersion != 1 {
		t.Errorf("MessagesWrongVersion = %d, want 1 (the unenveloped message)", stats.MessagesWrongVersion)
	}
}

// TestAuthenticatedJoinIsAccepted is the other half: authentication that rejects everything is not a
// working feature. Without this, every assertion above would pass on a handler that dropped all
// traffic.
func TestAuthenticatedJoinIsAccepted(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "node-a"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	// The peer holds the same secret this manager loaded, which is the shared-secret model.
	gp := cm.gossip
	data := joinDatagram(t, gp.auth.secret, "node-b")

	gp.handleIncomingMessage(data, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 99), Port: 8080})

	if _, joined := cm.GetNodes()["node-b"]; !joined {
		t.Fatal("a node holding the correct cluster secret failed to join")
	}

	if got := gp.GetStats().MessagesRejected; got != 0 {
		t.Errorf("MessagesRejected = %d, want 0 for an authenticated message", got)
	}
}

// TestReplayedAnnouncementIsRejected covers the case the issue singles out: a captured ownership or
// liveness announcement, replayed later, must not take effect a second time. A MAC alone does not
// stop this, because the replayed bytes carry a MAC that verifies.
func TestReplayedAnnouncementIsRejected(t *testing.T) {
	t.Parallel()

	gp := gossipForAuth(t, "node-a")
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 99), Port: 8080}
	data := joinDatagram(t, gp.auth.secret, "node-b")

	gp.handleIncomingMessage(data, addr)
	gp.handleIncomingMessage(data, addr) // the same datagram, captured and sent again

	stats := gp.GetStats()
	if stats.MessagesReplayed != 1 {
		t.Errorf("MessagesReplayed = %d, want 1", stats.MessagesReplayed)
	}
	if stats.MessagesRejected != 1 {
		t.Errorf("MessagesRejected = %d, want 1", stats.MessagesRejected)
	}
}

// TestStaleMessageIsRejected pins the freshness window. A message older than the window is refused
// even though its MAC verifies and its ID has never been seen — which is what bounds the nonce
// cache to one window's worth of entries.
func TestStaleMessageIsRejected(t *testing.T) {
	t.Parallel()

	a := newMessageAuthenticator(testSecret())

	data, err := a.seal(&GossipMessage{
		Type:      MessageTypeAlive,
		From:      "node-b",
		Timestamp: time.Now().Add(-2 * replayWindow),
		MessageID: "stale",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := a.open(data); !errors.Is(err, ErrReplayed) {
		t.Errorf("open(stale) error = %v, want ErrReplayed", err)
	}
}

// TestMessageWithoutIDIsRejected pins the rule that a message with no ID cannot be accepted: it
// cannot be deduplicated, so accepting it would be a way to opt out of replay protection while
// still passing the MAC check.
func TestMessageWithoutIDIsRejected(t *testing.T) {
	t.Parallel()

	a := newMessageAuthenticator(testSecret())

	data, err := a.seal(&GossipMessage{
		Type:      MessageTypeAlive,
		From:      "node-b",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := a.open(data); !errors.Is(err, ErrReplayed) {
		t.Errorf("open(no ID) error = %v, want ErrReplayed", err)
	}
}

// TestTamperedPayloadIsRejected checks that the MAC covers the payload and not merely its presence.
// The mutation is a single byte inside the sealed message, leaving the envelope well-formed.
func TestTamperedPayloadIsRejected(t *testing.T) {
	t.Parallel()

	a := newMessageAuthenticator(testSecret())

	data, err := a.seal(&GossipMessage{
		Type:      MessageTypeAlive,
		From:      "node-b",
		Timestamp: time.Now(),
		MessageID: "tamper",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	var env authEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshaling the envelope: %v", err)
	}

	// Rewrite the sender inside the authenticated payload, keeping the original MAC.
	env.Payload = json.RawMessage(strings.Replace(string(env.Payload), `"node-b"`, `"node-x"`, 1))

	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshaling the envelope: %v", err)
	}

	if _, err := a.open(tampered); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("open(tampered) error = %v, want ErrUnauthenticated", err)
	}
}

// TestUnknownEnvelopeVersionIsDistinguished pins that a version this build does not understand is
// reported as version skew rather than as an authentication failure. The distinction is what an
// operator needs during a rolling upgrade: "wrong secret" sends them to the wrong place.
func TestUnknownEnvelopeVersionIsDistinguished(t *testing.T) {
	t.Parallel()

	a := newMessageAuthenticator(testSecret())

	data, err := json.Marshal(authEnvelope{Version: authVersion + 1, MAC: "00", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("marshaling the envelope: %v", err)
	}

	_, err = a.open(data)
	if !errors.Is(err, ErrUnknownAuthVersion) {
		t.Errorf("open(future version) error = %v, want ErrUnknownAuthVersion", err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a version mismatch must not be reported as an authentication failure")
	}
}

// TestRoundTripAcrossAuthenticators verifies that a message sealed by one node opens on another
// holding the same secret. Sealing and opening with the same instance would pass even if the MAC
// were computed over something node-local.
func TestRoundTripAcrossAuthenticators(t *testing.T) {
	t.Parallel()

	sender := newMessageAuthenticator(testSecret())
	receiver := newMessageAuthenticator(testSecret())

	want := &GossipMessage{
		Type:      MessageTypeCacheInvalidate,
		From:      "node-b",
		Data:      json.RawMessage(`{"key":"objects/data.bin","from":"node-b"}`),
		Timestamp: time.Now(),
		MessageID: "round-trip",
	}

	data, err := sender.seal(want)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, err := receiver.open(data)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if got.From != want.From || got.Type != want.Type || string(got.Data) != string(want.Data) {
		t.Errorf("round trip changed the message: got %+v, want %+v", got, want)
	}
}

// TestNonceCacheIsEvicted pins that the cache does not grow without bound. Without eviction the
// freshness window would bound what is accepted but not what is remembered, and a long-running node
// would accumulate an entry per message forever.
func TestNonceCacheIsEvicted(t *testing.T) {
	t.Parallel()

	base := time.Now()
	clock := base
	a := newMessageAuthenticator(testSecret())
	a.now = func() time.Time { return clock }

	first := &GossipMessage{Type: MessageTypeAlive, From: "node-b", Timestamp: base, MessageID: "first"}
	data, err := a.seal(first)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := a.open(data); err != nil {
		t.Fatalf("open(first): %v", err)
	}

	// Move past the window and accept an in-window message, which is what runs eviction.
	clock = base.Add(2 * replayWindow)

	second := &GossipMessage{Type: MessageTypeAlive, From: "node-b", Timestamp: clock, MessageID: "second"}
	data, err = a.seal(second)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := a.open(data); err != nil {
		t.Fatalf("open(second): %v", err)
	}

	a.mu.Lock()
	_, stillThere := a.seen["first"]
	size := len(a.seen)
	a.mu.Unlock()

	if stillThere {
		t.Error("an entry older than the replay window was not evicted")
	}
	if size != 1 {
		t.Errorf("nonce cache holds %d entries, want 1", size)
	}
}

// TestClusterRefusesToStartWithoutASecret is the fail-closed rule. Running unauthenticated is the
// failure nobody notices, so the absence of a secret has to be an error at construction — and the
// error has to name what is missing, because "failed to create gossip protocol" does not tell an
// operator which step they skipped.
func TestClusterRefusesToStartWithoutASecret(t *testing.T) {
	// t.Setenv forbids t.Parallel, and this test needs the variable empty.
	t.Setenv(ClusterSecretEnv, "")

	_, err := NewClusterManager(&ClusterConfig{
		NodeID:        "no-secret",
		ListenAddr:    "127.0.0.1:0",
		AdvertiseAddr: "127.0.0.1:0",
	})
	if !errors.Is(err, ErrNoClusterSecret) {
		t.Fatalf("NewClusterManager without a secret: error = %v, want ErrNoClusterSecret", err)
	}

	// The message must be actionable: it should name both places a secret can come from.
	if msg := err.Error(); !strings.Contains(msg, ClusterSecretEnv) || !strings.Contains(msg, "secret_file") {
		t.Errorf("error message does not name both secret sources: %q", msg)
	}
}

// TestLoadClusterSecret covers the sources and the rules that reject a secret.
func TestLoadClusterSecret(t *testing.T) {
	t.Run("environment wins over file", func(t *testing.T) {
		t.Setenv(ClusterSecretEnv, strings.Repeat("e", minSecretLen))

		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte(strings.Repeat("f", minSecretLen)), 0o600); err != nil {
			t.Fatalf("writing the secret file: %v", err)
		}

		got, err := LoadClusterSecret(path)
		if err != nil {
			t.Fatalf("LoadClusterSecret: %v", err)
		}
		if string(got) != strings.Repeat("e", minSecretLen) {
			t.Errorf("got the file's secret, want the environment's")
		}
	})

	t.Run("trailing newline is trimmed", func(t *testing.T) {
		t.Setenv(ClusterSecretEnv, "")

		path := filepath.Join(t.TempDir(), "secret")
		// What `openssl rand -hex 32 > file` actually leaves behind.
		if err := os.WriteFile(path, []byte(strings.Repeat("g", minSecretLen)+"\n"), 0o600); err != nil {
			t.Fatalf("writing the secret file: %v", err)
		}

		got, err := LoadClusterSecret(path)
		if err != nil {
			t.Fatalf("LoadClusterSecret: %v", err)
		}
		if string(got) != strings.Repeat("g", minSecretLen) {
			t.Errorf("secret = %q, want the newline trimmed", got)
		}
	})

	t.Run("short secret is refused", func(t *testing.T) {
		t.Setenv(ClusterSecretEnv, "")

		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("hunter2"), 0o600); err != nil {
			t.Fatalf("writing the secret file: %v", err)
		}

		_, err := LoadClusterSecret(path)
		if !errors.Is(err, ErrSecretTooShort) {
			t.Errorf("error = %v, want ErrSecretTooShort", err)
		}
		// The message should tell the operator how to make a good one.
		if !strings.Contains(err.Error(), "openssl rand -hex 32") {
			t.Errorf("error does not say how to generate a secret: %q", err)
		}
	})

	t.Run("world-readable file is refused", func(t *testing.T) {
		t.Setenv(ClusterSecretEnv, "")

		path := filepath.Join(t.TempDir(), "secret")
		// 0644 is the whole point: this is the insecure file the loader must refuse. gosec's G306 is
		// suppressed rather than satisfied, because satisfying it would delete the test.
		if err := os.WriteFile(path, []byte(strings.Repeat("h", minSecretLen)), 0o644); err != nil { //nolint:gosec // deliberately world-readable
			t.Fatalf("writing the secret file: %v", err)
		}

		_, err := LoadClusterSecret(path)
		if !errors.Is(err, ErrSecretPermissions) {
			t.Errorf("error = %v, want ErrSecretPermissions", err)
		}
	})

	t.Run("missing file names the path", func(t *testing.T) {
		t.Setenv(ClusterSecretEnv, "")

		path := filepath.Join(t.TempDir(), "absent")
		_, err := LoadClusterSecret(path)
		if err == nil {
			t.Fatal("expected an error for a missing secret file")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error does not name the path it tried: %q", err)
		}
	})
}

// TestSecretFilePermissions states the permission rule as a table, so the boundary is visible rather
// than implied by a bitmask.
func TestSecretFilePermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode os.FileMode
		ok   bool
		why  string
	}{
		{0o600, true, "owner read/write is the intended mode"},
		{0o400, true, "owner read-only is stricter and fine"},
		{0o000, true, "unreadable is not a disclosure"},
		{0o640, false, "the group can read it"},
		{0o604, false, "everyone can read it"},
		{0o666, false, "everyone can read and write it"},
		{0o601, false, "execute for others still exposes the file to others"},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			t.Parallel()
			if got := secretFilePermissionsOK(tc.mode); got != tc.ok {
				t.Errorf("secretFilePermissionsOK(%#o) = %v, want %v (%s)", tc.mode, got, tc.ok, tc.why)
			}
		})
	}
}

// TestConcurrentOpenIsSafe drives the nonce cache from several goroutines, because it is reached
// from the receive path and a map without a lock there would be a race that only appears under load.
// Exactly one of the concurrent attempts at the same message ID may be accepted.
func TestConcurrentOpenIsSafe(t *testing.T) {
	t.Parallel()

	a := newMessageAuthenticator(testSecret())

	data, err := a.seal(&GossipMessage{
		Type:      MessageTypeAlive,
		From:      "node-b",
		Timestamp: time.Now(),
		MessageID: "contended",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	const goroutines = 16
	results := make(chan error, goroutines)

	for range goroutines {
		go func() {
			_, err := a.open(data)
			results <- err
		}()
	}

	accepted := 0
	for range goroutines {
		if err := <-results; err == nil {
			accepted++
		} else if !errors.Is(err, ErrReplayed) {
			t.Errorf("unexpected error: %v", err)
		}
	}

	if accepted != 1 {
		t.Errorf("%d goroutines accepted the same message, want exactly 1", accepted)
	}
}
