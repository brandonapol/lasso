package jev

import (
	"sync"

	"github.com/brandonapol/lasso/internal/store"
)

// SpendTracker keeps Jev's spending per day (YYYY-MM-DD, local time), in US
// dollars.
type SpendTracker interface {
	Spent(day string) (float64, error)
	AddSpend(day string, usd float64) error
}

// StoreSpend tracks spending in lasso's database, under the feature "jev".
type StoreSpend struct{ S *store.Store }

// Spent implements SpendTracker.
func (s StoreSpend) Spent(day string) (float64, error) { return s.S.Spent("jev", day) }

// AddSpend implements SpendTracker.
func (s StoreSpend) AddSpend(day string, usd float64) error { return s.S.AddSpend("jev", day, usd) }

// MemSpend tracks spending in memory, for tests and for running without a
// database.
type MemSpend struct {
	mu  sync.Mutex
	usd map[string]float64
}

// NewMemSpend returns an empty MemSpend.
func NewMemSpend() *MemSpend { return &MemSpend{usd: map[string]float64{}} }

// Spent implements SpendTracker.
func (m *MemSpend) Spent(day string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usd[day], nil
}

// AddSpend implements SpendTracker.
func (m *MemSpend) AddSpend(day string, usd float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usd[day] += usd
	return nil
}
