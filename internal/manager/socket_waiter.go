package manager

import (
	"github.com/shazow/virtle/internal/manager/launch"
)

// Socket polling and vsock CID probing live in launch, shared with the qemu
// backend; these aliases keep the manager's own call sites unchanged.
const defaultSocketPollInterval = launch.DefaultSocketPollInterval

type pollingSocketWaiter = launch.PollingSocketWaiter

var newHostVSockCIDChecker = launch.NewHostVSockCIDChecker
