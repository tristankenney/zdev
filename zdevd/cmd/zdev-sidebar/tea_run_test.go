package main

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/render"
)

// The pre-tea wipe must be exactly home + clear-below. Anything less leaves
// the RenderUnreachable retry frames from the shared startup path stranded
// above tea's region (seen live on daemon restart); anything more risks
// fighting tea's own terminal initialization.
func TestPrepareScreenForTea(t *testing.T) {
	if got, want := prepareScreenForTea(), render.CursorHome+render.ClearToEnd; got != want {
		t.Errorf("prepareScreenForTea() = %q, want %q", got, want)
	}
}
