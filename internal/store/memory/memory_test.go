package memory_test

import (
	"testing"

	"github.com/xiguac/atomhelper/internal/store"
	"github.com/xiguac/atomhelper/internal/store/memory"
	"github.com/xiguac/atomhelper/internal/store/storetest"
)

func TestMemoryStore契约(t *testing.T) {
	storetest.RunSuite(t, func(*testing.T) store.TaskStore {
		return memory.New()
	})
}
