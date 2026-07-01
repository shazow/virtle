package manifest

import (
	"fmt"
	"strconv"

	"github.com/shazow/virtle/internal/executor"
)

// TemplateProvider contributes values to an exec template rendering context.
type TemplateProvider interface {
	TemplateContext() executor.Context
}

type TemplateContextFunc func() executor.Context

func (f TemplateContextFunc) TemplateContext() executor.Context {
	return f()
}

func NewTemplateRenderer(providers ...TemplateProvider) (*executor.Renderer, error) {
	return executor.New(TemplateContext(providers...))
}

func TemplateContext(providers ...TemplateProvider) executor.Context {
	context := executor.Context{}
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		for key, value := range provider.TemplateContext() {
			context[key] = value
		}
	}
	return context
}

func StaticTemplateContext(values executor.Context) TemplateProvider {
	return TemplateContextFunc(func() executor.Context {
		context := executor.Context{}
		for key, value := range values {
			context[key] = value
		}
		return context
	})
}

type QEMUTemplateProvider struct {
	HostName   string
	WorkingDir string
	StateDir   string
	Host       HostInput
}

func (p QEMUTemplateProvider) TemplateContext() executor.Context {
	return executor.Context{
		"HostName":   p.HostName,
		"WorkingDir": p.WorkingDir,
		"StateDir":   p.StateDir,
		"HostOS":     p.Host.OS,
		"HostArch":   p.Host.Arch,
		"HostSystem": p.Host.System,
	}
}

type VirtioFSTemplateProvider struct {
	SocketPath string
	SourcePath string
	Tag        string
}

func (p VirtioFSTemplateProvider) TemplateContext() executor.Context {
	return executor.Context{
		"Socket":      p.SocketPath,
		"MountTag":    p.Tag,
		"MountSource": p.SourcePath,
	}
}

type RunTemplateProvider struct {
	CID       int
	StateDir  string
	Workspace Workspace
	Vars      map[string]any
}

func (p RunTemplateProvider) TemplateContext() executor.Context {
	context := executor.Context{
		"CID":      fmt.Sprintf("%d", p.CID),
		"StateDir": p.StateDir,
		"Workspace": templateWorkspace{
			GuestPath: p.Workspace.GuestDir,
			HostPath:  p.Workspace.HostDir,
		},
	}
	for key, value := range p.Vars {
		context[key] = value
	}
	return context
}

type ForwardTemplateProvider struct {
	Host string
	Port int
}

func (p ForwardTemplateProvider) TemplateContext() executor.Context {
	return executor.Context{
		"Host": p.Host,
		"Port": strconv.Itoa(p.Port),
	}
}

type SSHTemplateProvider struct {
	CID         int
	User        string
	Destination string
}

func (p SSHTemplateProvider) TemplateContext() executor.Context {
	return executor.Context{
		"CID":         fmt.Sprintf("%d", p.CID),
		"User":        p.User,
		"Destination": p.Destination,
	}
}

type NotificationTemplateProvider struct {
	State   string
	Message string
	Values  map[string]string
}

func (p NotificationTemplateProvider) TemplateContext() executor.Context {
	context := executor.Context{
		"State":   p.State,
		"Message": p.Message,
	}
	for key, value := range p.Values {
		context[key] = value
	}
	return context
}
