package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tianwei/diskless/internal/store"
)

// newImageTestStore 打开一个临时 sqlite store。
func newImageTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	st, err := store.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
