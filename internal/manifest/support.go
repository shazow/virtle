package manifest

import "fmt"

func (m *Manifest) SSHDestination(cid int) string {
	return fmt.Sprintf("%s@vsock/%d", m.SSH.User, cid)
}

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}
