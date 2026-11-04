package upload

import "os"

// Windows does not support os.File.Sync on directory handles. Publication is
// atomic, but power-loss directory durability is not promised on this platform.
func syncParent(_ *os.Root) error { return nil }
func syncStage(_ *os.Root) error  { return nil }
