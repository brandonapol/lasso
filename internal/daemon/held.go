package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/store"
)

// Audit actions for held messages. Neither is a rules action: the send
// that held the message was already checked and audited; these record
// what happened to it afterwards.
const (
	actHold    = "hold_message"    // a message is held until its session closes
	actDeliver = "deliver_message" // a held message went out (or failed to)
	actorLasso = "lasso"           // lasso itself, carrying out an earlier send
)

// heldStore is the adapters' adapter.HeldStore: held messages live in
// lasso.db, so they survive a restart, and each is audited as it is held
// and as it goes out.
type heldStore struct {
	st  *store.Store
	log *audit.Log
	now func() time.Time
}

var _ adapter.HeldStore = heldStore{}

// Hold implements adapter.HeldStore.
func (h heldStore) Hold(m adapter.HeldMessage) error {
	err := h.st.AddHeld(store.Held{ID: m.ID, Adapter: m.Adapter, Session: m.Session, Text: m.Text, Created: m.Created})
	detail := "Held the message until the session closes."
	result := audit.OK
	if err != nil {
		result, detail = audit.Failed, "Held the message, but couldn't save it to disk: "+err.Error()
	}
	h.audit(m, actHold, result, detail)
	return err
}

// Release implements adapter.HeldStore.
func (h heldStore) Release(m adapter.HeldMessage, sendErr error) error {
	err := h.st.DeleteHeld(m.ID)
	if sendErr != nil {
		h.audit(m, actDeliver, audit.Failed, "Couldn't send the held message: "+sendErr.Error())
	} else {
		h.audit(m, actDeliver, audit.OK, "Sent the held message: the session closed.")
	}
	return err
}

// Held implements adapter.HeldStore.
func (h heldStore) Held(kind string) ([]adapter.HeldMessage, error) {
	hs, err := h.st.HeldFor(kind)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.HeldMessage, len(hs))
	for i, x := range hs {
		out[i] = adapter.HeldMessage{ID: x.ID, Adapter: x.Adapter, Session: x.Session, Text: x.Text, Created: x.Created}
	}
	return out, nil
}

// audit writes one line about a held message. The send itself was
// already audited before it ran, so a line lost here doesn't stop
// anything; the store is still the record of what is held.
func (h heldStore) audit(m adapter.HeldMessage, action, result, detail string) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	_ = h.log.Append(audit.Entry{
		Time: h.now(), ID: hex.EncodeToString(b[:]), Actor: actorLasso, Bot: m.Adapter,
		Action: action, Target: m.Session, Result: result, Detail: detail,
		Args: map[string]string{argAdapter: m.Adapter, "held_id": m.ID, argText: m.Text},
	})
}

// useHeldStore gives every adapter that holds messages the store, and so
// reloads what each held before a restart.
func useHeldStore(adapters []adapter.Adapter, s adapter.HeldStore, logf func(string, ...any)) {
	for _, a := range adapters {
		if h, ok := a.(adapter.Holder); ok {
			if err := h.UseHeldStore(s); err != nil {
				logf("lasso: %s: %v", a.Kind(), err)
			}
		}
	}
}

// closeAdapters closes every adapter that has a Close (Claude Code's
// relay session, grok's leader connections).
func closeAdapters(adapters []adapter.Adapter, logf func(string, ...any)) {
	for _, a := range adapters {
		if c, ok := a.(io.Closer); ok {
			if err := c.Close(); err != nil {
				logf("lasso: closing %s: %v", a.Kind(), err)
			}
		}
	}
}
