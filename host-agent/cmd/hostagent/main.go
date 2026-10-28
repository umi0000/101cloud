package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"101cloud/hostagent/internal/bootstrap"
	"101cloud/hostagent/internal/config"
	"101cloud/hostagent/internal/gateway"
	"101cloud/hostagent/internal/server"
)

func main() { os.Exit(run()) }

func run() int {
	return runArgs(os.Args[1:], os.Stdout, os.Stderr)
}

func runArgs(args []string, output, errorOutput io.Writer) int {
	if len(args) > 0 && args[0] == "init" {
		return runInit(args[1:], output, errorOutput)
	}
	flags := flag.NewFlagSet("hostagent", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	flags.Usage = func() {
		fmt.Fprintln(errorOutput, "Usage: hostagent [-config path] [-check]\n       hostagent init --state-dir path --endpoint https://host:port [flags]")
		flags.PrintDefaults()
	}
	path := flags.String("config", "/etc/101cloud-agent/agent.json", "path to strict JSON configuration")
	check := flags.Bool("check", false, "validate configuration, certificate and token without listening or contacting Incus")
	if len(args) == 1 && args[0] == "help" {
		flags.Usage()
		return 0
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errorOutput, "unexpected positional arguments")
		return 2
	}
	log := slog.New(slog.NewJSONHandler(errorOutput, nil))
	c, err := config.Load(*path)
	if err != nil {
		log.Error("configuration rejected", "error", err)
		return 1
	}
	if *check {
		if _, err := gateway.TLSConfig(c.TLS.Cert, c.TLS.Key); err != nil {
			log.Error("TLS configuration rejected", "error", err)
			return 1
		}
		if _, err := gateway.LoadToken(c.TokenFile); err != nil {
			log.Error("token rejected", "error", err)
			return 1
		}
		log.Info("configuration valid; Incus connectivity was not checked")
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := server.Run(ctx, c, log); err != nil {
		log.Error("agent stopped", "error", err)
		return 1
	}
	return 0
}

func runInit(args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("hostagent init", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var opts bootstrap.Options
	flags.StringVar(&opts.StateDir, "state-dir", "", "new private state directory; must not already exist (required)")
	flags.StringVar(&opts.Endpoint, "endpoint", "", "advertised HTTPS origin with an explicit port, without path, query or fragment (required)")
	flags.StringVar(&opts.Listen, "listen", "", "listen host:port; defaults to endpoint host:port")
	flags.StringVar(&opts.IncusSocket, "incus-socket", bootstrap.DefaultIncusSocket, "absolute path to the local Incus socket")
	flags.DurationVar(&opts.Validity, "validity", bootstrap.DefaultValidity, "self-signed server certificate validity, as a Go duration (for example 8760h)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errorOutput, "unexpected positional arguments")
		return 2
	}
	if opts.StateDir == "" || opts.Endpoint == "" {
		fmt.Fprintln(errorOutput, "hostagent init: --state-dir and --endpoint are required")
		return 2
	}
	paths, err := bootstrap.Init(opts)
	if err != nil {
		fmt.Fprintln(errorOutput, "hostagent init:", err)
		return 1
	}
	fmt.Fprintf(output, "Created agent configuration: %s\nServer certificate: %s\nServer private key: %s\nNode token file: %s\nBackend connection JSON: %s\n", paths.Config, paths.Certificate, paths.Key, paths.Token, paths.Connection)
	return 0
}
