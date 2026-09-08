package qemu

import (
	"github.com/shazow/virtle/backend/qemu/session"
	shared "github.com/shazow/virtle/internal/session"
)

// SessionHooks supplies the QEMU-specific CLI SSH and suspend adapters.
// Its internal return type makes this a module implementation detail.
func (*Backend) SessionHooks() shared.Hooks { return session.Hooks() }
