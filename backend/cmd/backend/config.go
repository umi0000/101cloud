package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func commandConfig(args []string) (string, string, error) {
	if len(args) == 0 {
		return "", "", errors.New("usage: backend {install|migrate|serve} [-c config-file]")
	}
	command := args[0]
	switch command {
	case "install", "migrate", "serve":
	default:
		return "", "", errors.New("unknown command")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("c", "", "configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		return "", "", errors.New("invalid arguments: expected " + command + " [-c config-file]")
	}
	if flags.NArg() != 0 {
		return "", "", errors.New("unexpected positional arguments")
	}
	empty := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "c" && *file == "" {
			empty = true
		}
	})
	if empty {
		return "", "", errors.New("-c requires a nonempty file path")
	}
	return command, *file, nil
}

// parseConfigFile accepts literal, single-line CLOUD_* assignments, not shell code.
// Error messages deliberately omit values, which may contain credentials.
func parseConfigFile(r io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		valid := ok && strings.HasPrefix(key, "CLOUD_") && len(key) > len("CLOUD_")
		for _, c := range key {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				valid = false
			}
		}
		if !valid {
			return nil, fmt.Errorf("invalid configuration assignment at line %d", line)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate configuration key at line %d", line)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return nil, fmt.Errorf("unclosed configuration quote at line %d", line)
			}
			value = value[1 : len(value)-1]
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid configuration value at line %d", line)
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot read configuration file (maximum line size 64 KiB)")
	}
	return values, nil
}

func loadConfigFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configuration file: %w", err)
	}
	defer file.Close()
	values, err := parseConfigFile(file)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set configuration key %s: %w", key, err)
		}
	}
	return nil
}
