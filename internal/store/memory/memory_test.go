package memory_test

import (
	"testing"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return memory.New()
	})
}
