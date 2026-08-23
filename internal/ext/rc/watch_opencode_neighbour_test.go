package rc

import (
	"fmt"
	"testing"
	"time"
)

// TestOpencodeDoesNotAdoptANeighboursConversation pins a bug seen live, on a
// setup the product actively encourages: two opencode RC sessions running in
// the SAME repository.
//
// Each RC session gets its own opencode server on its own port, which reads a
// SHARED per-project session store — so GET /session lists the neighbour's
// conversations too, and the directory cannot tell them apart. The
// newest-match rule therefore adopted whatever conversation was most recent in
// that directory and seeded its entire history into this session's feed: two
// agents, one transcript, and no way for a reader to know whose words they
// were looking at.
//
// The discriminator is time. A conversation that existed BEFORE this RC
// session did cannot be its own.
func TestOpencodeDoesNotAdoptANeighboursConversation(t *testing.T) {
	const dir = ocFixtureDir
	sessionStart := time.Date(2026, 8, 23, 18, 27, 42, 0, time.UTC)
	neighbour := sessionStart.Add(-7 * time.Minute) // the other agent's, minutes older
	mine := sessionStart.Add(30 * time.Second)      // this session's own, moments later

	entry := func(id string, at time.Time) string {
		return fmt.Sprintf(`{"id":%q,"directory":%q,"parentID":"","time":{"created":%d}}`,
			id, dir, at.UnixMilli())
	}

	t.Run("a conversation older than the session is not adopted", func(t *testing.T) {
		f := newFakeOpencode(t)
		f.sessionBody = "[" + entry(ocOtherSID, neighbour) + "]"
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", sessionStart, clk.now, nil)
		t.Cleanup(w.close)

		if id, ok := w.restFindCandidate(); ok {
			t.Fatalf("adopted the neighbour's conversation %q — it predates this session", id)
		}
	})

	t.Run("its own conversation still correlates", func(t *testing.T) {
		f := newFakeOpencode(t)
		// Newest first, exactly as opencode returns them: the neighbour's is the
		// most recently UPDATED, which is what made the old rule pick it.
		f.sessionBody = "[" + entry(ocOtherSID, neighbour) + "," + entry(ocFixtureSID, mine) + "]"
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", sessionStart, clk.now, nil)
		t.Cleanup(w.close)

		id, ok := w.restFindCandidate()
		if !ok || id != ocFixtureSID {
			t.Fatalf("candidate = %q/%v, want %q — its own conversation", id, ok, ocFixtureSID)
		}
	})

	t.Run("a session with no creation time keeps the old behaviour", func(t *testing.T) {
		// Losing correlation entirely would be a worse failure than the one being
		// fixed, so an RC session that cannot say when it started still adopts.
		f := newFakeOpencode(t)
		f.sessionBody = "[" + entry(ocOtherSID, neighbour) + "]"
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", time.Time{}, clk.now, nil)
		t.Cleanup(w.close)

		if id, ok := w.restFindCandidate(); !ok || id != ocOtherSID {
			t.Fatalf("candidate = %q/%v, want the pre-existing one", id, ok)
		}
	})

	t.Run("an opencode too old to stamp its sessions still correlates", func(t *testing.T) {
		f := newFakeOpencode(t)
		f.sessionBody = fmt.Sprintf(`[{"id":%q,"directory":%q,"parentID":""}]`, ocOtherSID, dir)
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", sessionStart, clk.now, nil)
		t.Cleanup(w.close)

		if id, ok := w.restFindCandidate(); !ok || id != ocOtherSID {
			t.Fatalf("candidate = %q/%v: a missing stamp means 'cannot tell', not 'ancient'", id, ok)
		}
	})

	t.Run("a conversation another session owns is never adopted", func(t *testing.T) {
		// THE ORDERING AGE CANNOT SETTLE, and the reason claims exist: a session
		// that started FIRST and then sat idle must not adopt the conversation a
		// LATER session is actively using. That conversation is newer than the
		// adopter, so every age rule says yes; only ownership says no.
		later := sessionStart.Add(5 * time.Minute)
		f := newFakeOpencode(t)
		f.sessionBody = "[" + entry(ocOtherSID, later) + "]"
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", sessionStart, clk.now, nil)
		t.Cleanup(w.close)

		if id, ok := w.restFindCandidate(); !ok || id != ocOtherSID {
			t.Fatalf("unclaimed it is a legitimate candidate: %q/%v", id, ok)
		}
		w.setClaimed([]string{ocOtherSID})
		if id, ok := w.restFindCandidate(); ok {
			t.Errorf("adopted %q although another session owns it", id)
		}
		// The same on the SSE path.
		frame := peekEnvelope([]byte(fmt.Sprintf(
			`{"type":"session.updated","properties":{"info":%s}}`, entry(ocOtherSID, later))))
		if _, ok := w.rootPinFromCreated(frame); ok {
			t.Error("pinned to a claimed conversation from a session.updated frame")
		}
	})

	t.Run("session.updated for a neighbour is not a pin either", func(t *testing.T) {
		// The REST path is not the only way in: session.updated fires for every
		// conversation in the shared store, so the same age rule applies there.
		f := newFakeOpencode(t)
		f.holdOpenSSE()
		clk := opencodeClock()
		w := newOpencodeWatcher(f.port(t), dir, "", sessionStart, clk.now, nil)
		t.Cleanup(w.close)

		frame := func(id string, at time.Time) *ocPeek {
			return peekEnvelope([]byte(fmt.Sprintf(
				`{"type":"session.updated","properties":{"info":%s}}`, entry(id, at))))
		}
		if _, ok := w.rootPinFromCreated(frame(ocOtherSID, neighbour)); ok {
			t.Error("pinned to the neighbour's conversation from a session.updated frame")
		}
		if id, ok := w.rootPinFromCreated(frame(ocFixtureSID, mine)); !ok || id != ocFixtureSID {
			t.Errorf("own conversation: id=%q ok=%v", id, ok)
		}
	})
}
