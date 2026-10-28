package claude

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Watch tails the transcripts and Claude Code's session files: the
// channel gets a value soon after any of them changes (several changes
// may come as one), so a poller can call Events at once instead of on its
// next tick. It closes when ctx ends.
func (a *Adapter) Watch(ctx context.Context) (<-chan struct{}, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(a.opts.Root); err != nil {
		_ = w.Close()
		return nil, err
	}
	ents, _ := os.ReadDir(a.opts.Root)
	for _, e := range ents {
		if e.IsDir() {
			_ = w.Add(filepath.Join(a.opts.Root, e.Name())) // best effort: a limit hit means polling covers it
		}
	}
	_ = w.Add(a.opts.SessionsDir) // busy/idle changes; best effort, as above
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		defer func() { _ = w.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) && filepath.Dir(ev.Name) == a.opts.Root {
					if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
						_ = w.Add(ev.Name) // a new project directory
					}
				}
				status := filepath.Dir(ev.Name) == a.opts.SessionsDir && filepath.Ext(ev.Name) == ".json"
				if !status && filepath.Ext(ev.Name) != ".jsonl" && filepath.Dir(ev.Name) != a.opts.Root {
					continue
				}
				select {
				case out <- struct{}{}:
				default:
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return out, nil
}
