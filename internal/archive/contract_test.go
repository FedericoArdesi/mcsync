package archive_test

import (
	"testing"

	"mcsync/internal/archive"
	"mcsync/internal/archive/storetest"
)

func TestMemStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) archive.Store {
		return archive.NewMemStore()
	})
}
