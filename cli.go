package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	dep "github.com/hashicorp/consul-template/dependency"
	"github.com/hashicorp/consul-template/logging"
	"github.com/hashicorp/consul-template/watch"
)

// Exit codes are int valuse that represent an exit code for a particular error.
// Sub-systems may check this unique error to determine the cause of an error
// without parsing the output or help text.
const (
	ExitCodeOK int = 0

	// Errors start at 10
	ExitCodeError = 10 + iota
	ExitCodeInterrupt
	ExitCodeLoggingError
	ExitCodeParseFlagsError
	ExitCodeParseConfigError
	ExitCodeRunnerError
	ExitCodeConsulAPIError
	ExitCodeWatcherError
)

/// ------------------------- ///

// CLI is the main entry point for envconsul.
type CLI struct {
	sync.Mutex

	// outSteam and errStream are the standard out and standard error streams to
	// write messages from the CLI.
	outStream, errStream io.Writer

	// stopCh is an internal channel used to trigger a shutdown of the CLI.
	stopCh  chan struct{}
	stopped bool
}

func NewCLI(out, err io.Writer) *CLI {
	return &CLI{
		outStream: out,
		errStream: err,
		stopCh:    make(chan struct{}),
	}
}

// Run accepts a slice of arguments and returns an int representing the exit
// status from the command.
func (cli *CLI) Run(args []string) int {
	// Parse the flags and args
	config, parsedArgs, once, version, err := cli.parseFlags(args[1:])
	if err != nil {
		return cli.handleError(err, ExitCodeParseFlagsError)
	}

	// Setup the logging
	if err := logging.Setup(&logging.Config{
		Name:           Name,
		Level:          config.LogLevel,
		Syslog:         config.Syslog.Enabled,
		SyslogFacility: config.Syslog.Facility,
		Writer:         cli.errStream,
	}); err != nil {
		return cli.handleError(err, ExitCodeLoggingError)
	}

	// If the version was requested, return an "error" containing the version
	// information. This might sound weird, but most *nix applications actually
	// print their version on stderr anyway.
	if version {
		log.Printf("[DEBUG] (cli) version flag was given, exiting now")
		fmt.Fprintf(cli.errStream, "%s v%s\n", Name, Version)
		return ExitCodeOK
	}

	var command []string
	if len(config.Prefixes) > 0 {
		command = parsedArgs
	} else {
		if len(parsedArgs) < 2 {
			err := fmt.Errorf("cli: missing required arguments prefix and command")
			return cli.handleError(err, ExitCodeParseFlagsError)
		}

		// TODO: Remove in an upcoming release
		log.Printf("[WARN] specifying the consul key on the command line is " +
			"deprecated, please use -prefix instead")
		prefixRaw := parsedArgs[0]
		command = parsedArgs[1:]
		prefix, err := dep.ParseStoreKeyPrefix(prefixRaw)
		if err != nil {
			return cli.handleError(err, ExitCodeError)
		}
		config.Prefixes = append(config.Prefixes, prefix)
	}

	// Initial runner
	runner, err := NewRunner(config, command, once)
	if err != nil {
		return cli.handleError(err, ExitCodeRunnerError)
	}
	go runner.Start()

	// Listen for signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, Signals...)

	for {
		select {
		case err := <-runner.ErrCh:
			return cli.handleError(err, ExitCodeRunnerError)
		case <-runner.DoneCh:
			return ExitCodeOK
		case code := <-runner.ExitCh:
			log.Printf("[INFO] (cli) subprocess exited")
			runner.Stop()

			if code == ExitCodeOK {
				return ExitCodeOK
			} else {
				err := fmt.Errorf("unexpected exit from subprocess (%d)", code)
				return cli.handleError(err, code)
			}
		case s := <-signalCh:
			// Propogate the signal to the child process
			runner.Signal(s)

			switch s {
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
				fmt.Fprintf(cli.errStream, "Received interrupt, cleaning up...\n")
				runner.Stop()
				return ExitCodeInterrupt
			}
		case <-cli.stopCh:
			return ExitCodeOK
		}
	}
}

// stop is used internally to shutdown a running CLI
func (cli *CLI) stop() {
	cli.Lock()
	defer cli.Unlock()

	if cli.stopped {
		return
	}

	close(cli.stopCh)
	cli.stopped = true
}

// parseFlags is a helper function for parsing command line flags using Go's
// Flag library. This is extracted into a helper to keep the main function
// small, but it also makes writing tests for parsing command line arguments
// much easier and cleaner.
func (cli *CLI) parseFlags(args []string) (*Config, []string, bool, bool, error) {
	var once, version bool
	var config = DefaultConfig()

	// Parse the flags and options
	flags := flag.NewFlagSet(Name, flag.ContinueOnError)
	flags.SetOutput(cli.errStream)
	flags.Usage = func() {
		fmt.Fprintf(cli.errStream, usage, Name)
	}
	flags.StringVar(&config.Consul, "consul", config.Consul, "")
	flags.StringVar(&config.Token, "token", config.Token, "")
	flags.Var((*authVar)(config.Auth), "auth", "")
	flags.BoolVar(&config.SSL.Enabled, "ssl", config.SSL.Enabled, "")
	flags.BoolVar(&config.SSL.Verify, "ssl-verify", config.SSL.Verify, "")
	flags.DurationVar(&config.MaxStale, "max-stale", config.MaxStale, "")
	flags.BoolVar(&config.Syslog.Enabled, "syslog", config.Syslog.Enabled, "")
	flags.StringVar(&config.Syslog.Facility, "syslog-facility", config.Syslog.Facility, "")
	flags.Var((*watch.WaitVar)(config.Wait), "wait", "")
	flags.DurationVar(&config.Retry, "retry", config.Retry, "")
	flags.Var((*prefixVar)(&config.Prefixes), "prefix", "")
	flags.BoolVar(&config.Sanitize, "sanitize", config.Sanitize, "")
	flags.BoolVar(&config.Upcase, "upcase", config.Upcase, "")
	flags.StringVar(&config.Path, "config", config.Path, "")
	flags.StringVar(&config.LogLevel, "log-level", config.LogLevel, "")
	flags.BoolVar(&once, "once", false, "")
	flags.BoolVar(&version, "version", false, "")

	// If there was a parser error, stop
	if err := flags.Parse(args); err != nil {
		return nil, nil, false, false, err
	}

	return config, flags.Args(), once, version, nil
}

// handleError outputs the given error's Error() to the errStream and returns
// the given exit status.
func (cli *CLI) handleError(err error, status int) int {
	log.Printf("[ERR] %s", err.Error())
	return status
}

const usage = `
Usage: %s [options]

  Watches values from Consul's K/V store and sets environment variables when
  Consul values are changed.

Options:

  -auth=<user[:pass]>      Set the basic authentication username (and password)
  -consul=<address>        Sets the address of the Consul instance
  -max-stale=<duration>    Set the maximum staleness and allow stale queries to
                           Consul which will distribute work among all servers
                           instead of just the leader
  -ssl                     Use SSL when connecting to Consul
  -ssl-verify              Verify certificates when connecting via SSL
  -token=<token>           Sets the Consul API token

  -syslog                  Send the output to syslog instead of standard error
                           and standard out. The syslog facility defaults to
                           LOCAL0 and can be changed using a configuration file
  -syslog-facility=<f>     Set the facility where syslog should log. If this
                           attribute is supplied, the -syslog flag must also be
                           supplied.

  -wait=<duration>         Sets the 'minumum(:maximum)' amount of time to wait
                           before writing a triggering a restart
  -retry=<duration>        The amount of time to wait if Consul returns an
                           error when communicating with the API

  -prefix                  A prefix to watch, multiple prefixes are merged from
                           left to right, with the right-most result taking
                           precedence
  -sanitize                Replace invalid characters in keys to underscores
  -upcase                  Convert all environment variable keys to uppercase


  -config=<path>           Sets the path to a configuration file on disk

  -log-level=<level>       Set the logging level - valid values are "debug",
                           "info", "warn" (default), and "err"

  -once                    Do not run the process as a daemon
  -version                 Print the version of this daemon
`
