package hop

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	teacmd "gitea.dev/tea/cmd"
	"github.com/urfave/cli/v3"
)

var teaEnvironmentMu sync.Mutex
var nativeGiteaCommandNames = loadTeaCompatibleCommandNames()

func loadTeaCompatibleCommandNames() map[string]struct{} {
	commands := make(map[string]struct{})
	for _, command := range teacmd.App().Commands {
		if command.Name == "logins" || command.Name == "logout" {
			continue
		}
		commands[command.Name] = struct{}{}
		for _, alias := range command.Aliases {
			commands[alias] = struct{}{}
		}
	}
	return commands
}

func teaCompatibleCommandNames() map[string]struct{} {
	result := make(map[string]struct{}, len(nativeGiteaCommandNames))
	for name := range nativeGiteaCommandNames {
		result[name] = struct{}{}
	}
	return result
}

func isTeaCompatibleCommand(name string) bool {
	_, exists := nativeGiteaCommandNames[name]
	return exists
}

func runTeaCompatibleCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, auth *AuthClient) int {
	teaEnvironmentMu.Lock()
	defer teaEnvironmentMu.Unlock()
	if !containsHelpFlag(args) {
		session, err := auth.OAuthSession(ctx)
		if err != nil {
			return printCLIError(err, false, stdout, stderr)
		}
		restore := setTemporaryEnvironment(map[string]string{
			"GITEA_INSTANCE_URL": session.Forge,
			"GITEA_TOKEN":        session.AccessToken,
		})
		defer restore()
	}

	app := teacmd.App()
	app.Name = "hop"
	app.Usage = "Hop-native Gitea command"
	brandTeaCommand(app)
	app.Reader = stdin
	app.Writer = stdout
	app.ErrWriter = stderr
	if err := app.Run(ctx, append([]string{"hop"}, normalizeTeaArgs(args)...)); err != nil {
		fmt.Fprintf(stderr, "hop: %v\n", err)
		return 1
	}
	return 0
}

func brandTeaCommand(command *cli.Command) {
	command.Description = strings.ReplaceAll(command.Description, "tea ", "hop ")
	for _, child := range command.Commands {
		brandTeaCommand(child)
	}
}

func containsHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func normalizeTeaArgs(args []string) []string {
	result := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if arg == "-o-" {
			result = append(result, "-o", "-")
		} else {
			result = append(result, arg)
		}
	}
	return result
}

func setTemporaryEnvironment(values map[string]string) func() {
	previous := make(map[string]*string, len(values))
	for name, value := range values {
		if old, exists := os.LookupEnv(name); exists {
			copy := old
			previous[name] = &copy
		} else {
			previous[name] = nil
		}
		_ = os.Setenv(name, value)
	}
	return func() {
		for name, value := range previous {
			if value == nil {
				_ = os.Unsetenv(name)
			} else {
				_ = os.Setenv(name, *value)
			}
		}
	}
}
