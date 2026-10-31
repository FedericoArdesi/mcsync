package archive_test

import (
	"testing"

	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/archive/storetest"
)

func TestMemStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) archive.Store {
		return archive.NewMemStore()
	})
}
