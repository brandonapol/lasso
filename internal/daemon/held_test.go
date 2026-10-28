package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
	"github.com/brandonapol/lasso/internal/tui"
)

// world is the fake bot's machine, which outlives a daemon restart: is
// the session still open in a terminal, and what reached it.
type world struct {
	mu        sync.Mutex
	open      bool
	delivered []string
}

func (w *world) setOpen(open bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.open = open
}

func (w *world) got() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.delivered)
}

// holdBot is a fake bot whose one session, s1, is open in a terminal while
// w.open: like Claude Code and grok, it holds a message for an open session
// and sends it once the session closes, keeping held messages in the
// daemon's held store.
type holdBot struct {
	w     *world
	wake  chan struct{}
	mu    sync.Mutex
	store adapter.HeldStore
	held  []adapter.HeldMessage
	sent  int // held messages it has sent
	close int // Close calls
}

func newHoldBot(w *world) *holdBot { return &holdBot{w: w, wake: make(chan struct{}, 1)} }

func (b *holdBot) Kind() string { return "fake" }

func (b *holdBot) Sessions(context.Context) ([]adapter.Session, error) {
	return []adapter.Session{{ID: "s1", Bot: "fake", Title: "task one", State: adapter.Running, Writable: true}}, nil
}

// Events sends what is held once the session has closed.
func (b *holdBot) Events(_ context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	b.w.mu.Lock()
	open := b.w.open
	b.w.mu.Unlock()
	if open {
		return nil, since, nil
	}
	b.mu.Lock()
	held := b.held
	b.held = nil
	b.mu.Unlock()
	for _, m := range held {
		b.w.mu.Lock()
		b.w.delivered = append(b.w.delivered, m.Session+": "+m.Text)
		b.w.mu.Unlock()
		if err := b.store.Release(m, nil); err != nil {
			return nil, since, err
		}
		b.mu.Lock()
		b.sent++
		b.mu.Unlock()
	}
	return nil, since, nil
}

func (b *holdBot) Send(_ context.Context, session, text string) (adapter.Delivery, error) {
	b.w.mu.Lock()
	open := b.w.open
	if !open {
		b.w.delivered = append(b.w.delivered, session+": "+text)
	}
	b.w.mu.Unlock()
	if !open {
		return adapter.Sent, nil
	}
	m := adapter.HeldMessage{ID: adapter.NewHeldID(), Adapter: "fake", Session: session, Text: text, Created: time.Now()}
	b.mu.Lock()
	b.held = append(b.held, m)
	s := b.store
	b.mu.Unlock()
	return adapter.Held, s.Hold(m)
}

func (b *holdBot) Answer(context.Context, string, adapter.Choice) error { return nil }

func (b *holdBot) UseHeldStore(s adapter.HeldStore) error {
	ms, err := s.Held("fake")
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store = s
	b.held = append(b.held, ms...)
	return err
}

func (b *holdBot) Watch(context.Context) (<-chan struct{}, error) { return b.wake, nil }

func (b *holdBot) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.close++
	return nil
}

func (b *holdBot) counts() (sent, closed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent, b.close
}

// runDaemon runs the real daemon with bot until the returned stop is
// called (or the test ends).
func runDaemon(t *testing.T, opts Options, bot adapter.Adapter) (base string, stop func()) {
	t.Helper()
	opts.Port = freePort(t)
	opts.Adapters = []adapter.Adapter{bot}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	opts.Ready = func(*net.TCPAddr) { close(ready) }
	go func() { done <- Run(ctx, opts) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return fmt.Sprintf("http://127.0.0.1:%d", opts.Port), stop
}

// A message to a session that is open is held: the API and the TUI say
// it is waiting, not sent. It survives a daemon restart and goes out once
// the session closes; the adapters are closed at each shutdown, and the
// audit log has the message held and then sent.
func TestHeldMessageSurvivesRestartAndIsSentOnClose(t *testing.T) {
	opts := testOptions(t, 0)
	opts.Source = nil
	cfg := config.Default()
	opts.Config = &cfg
	opts.Runner = noGH{t}
	opts.AgentToken = "agent-pane-token"
	opts.RulesPath = filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(opts.RulesPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &world{open: true}

	bot1 := newHoldBot(w)
	base, stop := runDaemon(t, opts, bot1)
	tok, err := os.ReadFile(opts.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, base: base, userTok: strings.TrimSpace(string(tok)), agentTok: opts.AgentToken, opts: opts}

	// The API: the outcome carries delivery "held" and says it's waiting.
	code, body := r.post(r.userTok, `{"bot":"fake","action":"send_message","target":"session:fake:s1","args":{"text":"please rebase"}}`)
	if code != 200 || !strings.Contains(body, `"delivery":"held"`) || !strings.Contains(body, "waiting until that session closes") || strings.Contains(body, "Sent your message") {
		t.Fatalf("send: %d %s", code, body)
	}
	// The TUI shows the outcome's detail in its status line.
	out, err := tui.NewClient(base, r.userTok, nil).Action(context.Background(), rules.Request{
		Bot: "fake", Action: rules.SendMessage, Target: "session:fake:s1", Args: map[string]string{"text": "and add a test"},
	})
	if err != nil || out.Delivery != string(adapter.Held) || !strings.Contains(out.Detail, "waiting until that session closes") {
		t.Fatalf("tui: %+v %v", out, err)
	}

	stop()
	if _, closed := bot1.counts(); closed != 1 {
		t.Fatalf("the adapter was closed %d times at shutdown", closed)
	}
	if got := w.got(); len(got) != 0 {
		t.Fatalf("sent while the session was open: %q", got)
	}
	st, err := store.Open(DBFile(opts.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if hs, err := st.HeldFor("fake"); err != nil || len(hs) != 2 {
		t.Fatalf("held on disk after shutdown: %+v %v", hs, err)
	}
	_ = st.Close()

	// Restart with a new adapter: it reloads both, and sends them once the
	// session closes, oldest first.
	bot2 := newHoldBot(w)
	_, stop2 := runDaemon(t, opts, bot2)
	if got := w.got(); len(got) != 0 {
		t.Fatalf("sent before the session closed: %q", got)
	}
	w.setOpen(false)
	bot2.wake <- struct{}{}
	r.eventually("the held messages to go out", func() bool { n, _ := bot2.counts(); return n == 2 })
	if got := w.got(); !slices.Equal(got, []string{"s1: please rebase", "s1: and add a test"}) {
		t.Fatalf("delivered %q", got)
	}
	stop2()
	if _, closed := bot2.counts(); closed != 1 {
		t.Fatalf("the restarted adapter was closed %d times", closed)
	}

	st, err = store.Open(DBFile(opts.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if hs, err := st.HeldFor("fake"); err != nil || len(hs) != 0 {
		t.Fatalf("still held after delivery: %+v %v", hs, err)
	}

	var holds, delivers int
	for _, e := range r.audit() {
		if e.Actor != actorLasso {
			continue
		}
		if e.Bot != "fake" || e.Target != "s1" || e.Result != audit.OK || e.Args["held_id"] == "" {
			t.Fatalf("audit line %+v", e)
		}
		switch e.Action {
		case actHold:
			holds++
		case actDeliver:
			delivers++
		}
	}
	if holds != 2 || delivers != 2 {
		t.Fatalf("audit: %d held, %d delivered", holds, delivers)
	}
}

// The assistant is told a held message is waiting, not sent.
func TestAssistantIsToldAHeldMessageIsWaiting(t *testing.T) {
	r := startRigWith(t, messageRules, newHoldBot(&world{open: true}))
	r.eventually("the session on In progress", func() bool { return slices.Contains(r.ids(api.TabDoing), "session:fake:s1") })

	pane, m := r.assistant(toolCall("send_to_bot", map[string]string{"session": "session:fake:s1", "text": "please rebase"}))
	if _, err := pane.Send(context.Background(), "tell it to rebase"); err != nil {
		t.Fatal(err)
	}
	res := m.result(t)
	if res.IsError || !strings.Contains(res.Content, "NOT delivered yet") || !strings.Contains(res.Content, "waiting until that session closes") || strings.HasPrefix(res.Content, "Done") {
		t.Fatalf("tool result = %+v", res)
	}
}

func TestSendDetail(t *testing.T) {
	for d, want := range map[adapter.Delivery]string{
		adapter.Sent:            "Sent your message to claude session s1.",
		adapter.Held:            "Your message to claude session s1 is waiting until that session closes; lasso sends it then.",
		adapter.HeldForApproval: "Your message reached claude session s1, which is holding it until you approve it there.",
	} {
		if got := sendDetail("claude", "s1", d); got != want {
			t.Errorf("%s: %q", d, got)
		}
	}
}
