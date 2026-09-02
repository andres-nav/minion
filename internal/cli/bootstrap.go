package cli

import (
	"os"
	"runtime"

	"github.com/andres-nav/minion/internal/ssh"
)

// agentBinaryFor resolves the local minion binary to upload for the remote's
// platform (passed to ssh.EnsureAgent).
func agentBinaryFor(target string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	local := runtime.GOOS + "/" + runtime.GOARCH
	return ssh.ResolveAgentBinary(self, local, target, func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir()
	})
}
