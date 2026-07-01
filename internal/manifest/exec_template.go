package manifest

import "github.com/shazow/virtle/internal/executor"

func RenderCommand(command Command, renderer *executor.Renderer) (Command, error) {
	renderedArgv, err := renderer.RenderArgv(commandArgv(command))
	if err != nil {
		return Command{}, err
	}
	rendered := Command{
		Env: append([]string(nil), command.Env...),
	}
	if len(renderedArgv) > 0 {
		rendered.Path = renderedArgv[0]
		rendered.Args = append([]string(nil), renderedArgv[1:]...)
	}
	rendered.Env = append(rendered.Env, renderer.Env()...)
	return rendered, nil
}

func commandArgv(command Command) []string {
	if command.Path == "" {
		return append([]string(nil), command.Args...)
	}
	argv := []string{command.Path}
	return append(argv, command.Args...)
}
