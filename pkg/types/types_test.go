package types

import (
	"encoding/json"
	"testing"
	"time"
)

// TestKeyAnnouncementJSON pins the wire form.
//
// A KeyAnnouncement exists to travel: it is one node telling others what it has cached, and both halves
// of that exchange are remote. So the field names are a wire contract, and the reason to pin them is
// specific rather than general — Precondition shipped in this package with no tags at all and put
// `Absent` and `ETag` beside `key` and `created_at` in the same message. That was free to fix only
// because it had not been released yet.
//
// Every field is asserted present even when zero, which is deliberate and the opposite of Precondition's
// omitzero treatment. An announcement's zero values carry meaning: Offset 0 with Length 0 is "the whole
// object from the start", which is the commonest announcement there is, and omitting the pair would make
// it indistinguishable on the wire from a malformed message with no range at all.
func TestKeyAnnouncementJSON(t *testing.T) {
	t.Parallel()

	cachedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	got, err := json.Marshal(KeyAnnouncement{
		Key:      "datasets/reads.bam",
		NodeID:   "node-a",
		ETag:     `"abc123"`,
		Size:     4096,
		CachedAt: cachedAt,
		Offset:   0,
		Length:   0,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{"key":"datasets/reads.bam","node_id":"node-a","etag":"\"abc123\"","size":4096,` +
		`"cached_at":"2026-08-08T12:00:00Z","offset":0,"length":0}`
	if string(got) != want {
		t.Errorf("marshaled to\n  %s\nwant\n  %s", got, want)
	}

	// And it round-trips. A peer acts on what it decodes, so a field that marshals under the right name
	// but does not survive the return trip is the same defect one step later.
	var back KeyAnnouncement
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.CachedAt.Equal(cachedAt) {
		t.Errorf("CachedAt round-tripped to %v, want %v", back.CachedAt, cachedAt)
	}
	back.CachedAt = cachedAt // time.Time compares unequal across locations even when it is the same instant
	if want := (KeyAnnouncement{
		Key: "datasets/reads.bam", NodeID: "node-a", ETag: `"abc123"`, Size: 4096, CachedAt: cachedAt,
	}); back != want {
		t.Errorf("round-tripped to %+v, want %+v", back, want)
	}
}

// TestKeyAnnouncementJSON_RangeSurvives is the case the zero-value test cannot cover: a partial range
// must arrive as the range it was, not collapsed to the whole object.
//
// Length 0 means "to the end", so a marshaler that dropped a nonzero Length would turn "I hold the first
// 64 KiB of this 10 GiB object" into "I hold all of it". A peer acting on that fetches bytes the holder
// does not have, and the holder either reads them from S3 itself — slower than the peer would have been
// — or answers short.
func TestKeyAnnouncementJSON_RangeSurvives(t *testing.T) {
	t.Parallel()

	ann := KeyAnnouncement{
		Key:    "big/object",
		NodeID: "node-b",
		ETag:   `"v2"`,
		Size:   10 << 30,
		Offset: 1 << 20,
		Length: 64 << 10,
	}

	data, err := json.Marshal(ann)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back KeyAnnouncement
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if back.Offset != ann.Offset || back.Length != ann.Length {
		t.Errorf("range round-tripped to [%d,+%d), want [%d,+%d)", back.Offset, back.Length,
			ann.Offset, ann.Length)
	}
	if back.Size != ann.Size {
		t.Errorf("Size round-tripped to %d, want %d: the full object size is what tells a peer what "+
			"fraction of the object the announced range is", back.Size, ann.Size)
	}
}
