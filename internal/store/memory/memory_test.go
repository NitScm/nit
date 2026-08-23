package memory_test

import (
	"testing"

	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/pkg/store"
	"github.com/NitScm/nit/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

func TestPrunerConformance(t *testing.T) {
	storetest.RunPruner(t, newStore)
}

func newStore(t *testing.T) store.Store {
	return memory.New()
}
