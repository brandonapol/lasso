package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/agentpane"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/store"
)

// agentPaneFeature is the agent pane's name in the spend table.
const agentPaneFeature = "agentpane"

// lineJumperEvery is how often the agent pane looks for line-jumpers to
// mention.
const lineJumperEvery = 2 * time.Minute

// startAgentPane builds the assistant over the API at 127.0.0.1:port,
// calling it with the agent pane's token only, and starts its line-jumper
// watch. The model is built per message from whatever key is configured
// then, under sonnet_daily_usd. stop ends the watch and closes its store.
func startAgentPane(ctx context.Context, opts Options, port int, agentToken string, hub *api.Hub) (api.Chat, func(), error) {
	cfg := opts.Config
	if cfg == nil {
		c, err := config.Load()
		if err != nil {
			c = config.Default()
		}
		cfg = &c
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	// Its own handle on lasso.db: the conversation and spend live there.
	st, err := store.Open(DBFile(opts.StateDir))
	if err != nil {
		return nil, nil, fmt.Errorf("agent pane: %w", err)
	}
	cfgDir, _ := config.Dir()
	daily := cfg.Models.SonnetDailyUSD

	var mu sync.Mutex
	var cached *llm.Client
	var cachedKey string
	model := func() (agentpane.Model, error) {
		key, err := llm.LoadKey(os.Getenv, cfgDir)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if cached == nil || key != cachedKey {
			c, err := llm.New(llm.Options{
				Key: key, Model: llm.Sonnet, DailyUSD: daily,
				Spend: llm.StoreSpend{S: st, Feature: agentPaneFeature},
				// Tool rounds are short; a whole reply can take a while.
				HTTP: &http.Client{Timeout: 90 * time.Second},
			})
			if err != nil {
				return nil, err
			}
			cached, cachedKey = c, key
		}
		return cached, nil
	}

	pane := agentpane.New(agentpane.Options{
		API:    fmt.Sprintf("http://127.0.0.1:%d", port),
		Token:  agentToken,
		Model:  model,
		Store:  st,
		Notify: hub.Publish,
		Logf:   logf,
	})
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pane.Watch(wctx, lineJumperEvery)
	}()
	return pane, func() {
		cancel()
		<-done
		_ = st.Close()
	}, nil
}
