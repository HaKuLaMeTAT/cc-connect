package daemon

import (
	"errors"
	"os"
	"testing"
)

func TestRotatingWriter_WriteReturnsErrClosedWhenFileUnavailable(t *testing.T) {
	w := &RotatingWriter{}
	if _, err := w.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write error = %v, want %v", err, os.ErrClosed)
	}
}
