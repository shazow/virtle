package manifest

import "fmt"

func (m *Manifest) SSHDestination(cid int) string {
	return fmt.Sprintf("%s@vsock/%d", m.SSH.User, cid)
}
