package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// Config contains node transport settings, not user permissions or billing rules.
type Config struct {
	Listen      string `json:"listen"`
	IncusSocket string `json:"incus_socket"`
	TokenFile   string `json:"token_file"`
	TLS         TLS    `json:"tls"`
}

type TLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

func Load(path string) (Config, error) {
	var c Config
	absolute, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	f, err := os.Open(absolute)
	if err != nil {
		return c, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 64*1024+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return c, errors.New("config must contain exactly one JSON object")
	}
	if c.Listen == "" {
		return c, errors.New("listen is required; choose a management-network address")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return c, fmt.Errorf("listen: %w", err)
	}
	if c.IncusSocket == "" {
		c.IncusSocket = "/var/lib/incus/unix.socket"
	}
	if !filepath.IsAbs(c.IncusSocket) {
		return c, errors.New("incus_socket must be an absolute path")
	}
	if c.TLS.Cert == "" || c.TLS.Key == "" || c.TokenFile == "" {
		return c, errors.New("tls cert, key and token_file are required")
	}
	for _, p := range []*string{&c.TLS.Cert, &c.TLS.Key, &c.TokenFile} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(absolute), *p)
		}
	}
	return c, nil
}
