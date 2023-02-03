// Package agent represents the CLI UI for Ubuntu Pro agent.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/consts"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/daemon"
	log "github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/grpc/logstreamer"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/i18n"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/proservices"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/ubuntu/decorate"
)

const (
	Version = "dev"
)

// App encapsulate commands and options of the daemon, which can be controlled by env variables and config files.
type App struct {
	rootCmd cobra.Command
	viper   *viper.Viper
	config  daemonConfig

	daemon *daemon.Daemon

	ready chan struct{}
}

type daemonConfig struct {
	Verbosity int
}

// New registers commands and return a new App.
func New() *App {
	a := App{ready: make(chan struct{})}
	a.rootCmd = cobra.Command{
		Use:   fmt.Sprintf("%s COMMAND", cmdName()),
		Short: i18n.G("Ubuntu Pro for Windows agent"),
		Long:  i18n.G(`Ubuntu Pro for Windows agent for managing your pro-enabled distro.`),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Force a visit of the local flags so persistent flags for all parents are merged.
			cmd.LocalFlags()

			// command parsing has been successful. Returns to not print usage anymore.
			a.rootCmd.SilenceUsage = true

			// Parse environment veriables
			a.viper.SetEnvPrefix("UP4W")
			a.viper.AutomaticEnv()

			if err := a.viper.Unmarshal(&a.config); err != nil {
				return fmt.Errorf("unable to decode configuration into struct: %w", err)
			}

			setVerboseMode(a.config.Verbosity)
			log.Debug(context.Background(), "Debug mode is enabled")

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serve()
		},
		// We display usage error ourselves
		SilenceErrors: true,
	}
	a.viper = viper.New()

	installVerbosityFlag(&a.rootCmd, a.viper)

	// subcommands
	a.installVersion()

	return &a
}

// cmdName is the binary name for the agent.
func cmdName() string {
	return filepath.Base(os.Args[0])
}

// daemonOpts sets the options for the daemon.
// Having it be a global variable allows tests to change them.
var daemonOpts []daemon.Option

// proservicesOpts sets the options for the pro services.
// Having it be a global variable allows tests to change them.
var proservicesOpts []proservices.Option

// serve creates new GRPC services and listen on a TCP socket. This call is blocking until we quit it.
func (a *App) serve() error {
	proservice, err := proservices.New(context.Background(), proservicesOpts...)
	if err != nil {
		close(a.ready)
		return err
	}

	daemon, err := daemon.New(context.Background(), proservice.RegisterGRPCServices, daemonOpts...)
	if err != nil {
		close(a.ready)
		return err
	}

	a.daemon = &daemon
	close(a.ready)

	return daemon.Serve(context.Background())
}

// installVerbosityFlag adds the -v and -vv options and returns the reference to it.
func installVerbosityFlag(cmd *cobra.Command, viper *viper.Viper) *int {
	r := cmd.PersistentFlags().CountP("verbosity", "v", i18n.G("issue INFO (-v), DEBUG (-vv) or DEBUG with caller (-vvv) output"))
	decorate.LogOnError(viper.BindPFlag("verbosity", cmd.PersistentFlags().Lookup("verbosity")))
	return r
}

// SetVerboseMode change ErrorFormat and logs between very, middly and non verbose.
func setVerboseMode(level int) {
	var reportCaller bool
	switch level {
	case 0:
		logrus.SetLevel(consts.DefaultLogLevel)
	case 1:
		logrus.SetLevel(logrus.InfoLevel)
	case 3:
		reportCaller = true
		fallthrough
	default:
		logrus.SetLevel(logrus.DebugLevel)
	}
	log.SetReportCaller(reportCaller)
}

// Run executes the command and associated process. It returns an error on syntax/usage error.
func (a *App) Run() error {
	return a.rootCmd.Execute()
}

// UsageError returns if the error is a command parsing or runtime one.
func (a App) UsageError() bool {
	return !a.rootCmd.SilenceUsage
}

// Quit gracefully shutdown the service.
func (a *App) Quit() {
	a.WaitReady()
	if a.daemon == nil {
		return
	}
	a.daemon.Quit(context.Background(), false)
}

// WaitReady signals when the daemon is ready
// Note: we need to use a pointer to not copy the App object before the daemon is ready, and thus, creates a data race.
func (a *App) WaitReady() {
	<-a.ready
}

// RootCmd returns a copy of the root command for the app. Shouldn't be in general necessary apart when running generators.
func (a App) RootCmd() cobra.Command {
	return a.rootCmd
}

// SetArgs changes the root command args. Shouldn't be in general necessary apart for tests.
func (a *App) SetArgs(args ...string) {
	a.rootCmd.SetArgs(args)
}