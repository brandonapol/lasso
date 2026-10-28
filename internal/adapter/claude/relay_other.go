//go:build !linux

package claude

import "os/exec"

// setDeathSignal does nothing here: the relay ends when lasso closes its
// stdin, or after it has sat unused for a while.
func setDeathSignal(*exec.Cmd) {}
