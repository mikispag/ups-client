// Command ups-client tails a NUT (Network UPS Tools) `upsd` instance and
// fans events out to shell, webhook, SSH, and Telegram targets.
//
// Usage:
//
//	ups-client -config /etc/ups-client/config.yaml
//
// See README.md for the full configuration reference.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mikispag/ups-client/config"
	"github.com/mikispag/ups-client/monitor"
	"github.com/mikispag/ups-client/notifier"
	"github.com/mikispag/ups-client/nut"
)

func main() {
	configPath := flag.String("config", "/etc/ups-client/config.yaml", "path to YAML configuration file")
	verbose := flag.Bool("v", false, "verbose logging (debug)")
	checkOnly := flag.Bool("check", false, "load config and exit (validates without connecting)")
	listVars := flag.Bool("list", false, "connect, dump all NUT variables, and exit")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if *checkOnly {
		fmt.Println("config OK")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dialer := func(ctx context.Context) (monitor.Conn, error) {
		c, err := nut.Dial(ctx, cfg.NUT.Address, cfg.NUT.Timeout)
		if err != nil {
			return nil, err
		}
		if cfg.NUT.TLS != nil && cfg.NUT.TLS.Enable {
			tlsCfg, terr := buildTLSConfig(cfg.NUT.TLS)
			if terr != nil {
				_ = c.Close()
				return nil, terr
			}
			if err := c.StartTLS(tlsCfg); err != nil {
				_ = c.Close()
				return nil, err
			}
		}
		if err := c.Login(cfg.NUT.Username, cfg.NUT.Password, ""); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}

	if *listVars {
		c, err := dialer(ctx)
		if err != nil {
			log.Error("connect", "err", err)
			os.Exit(2)
		}
		defer c.Close()
		vars, err := c.ListVars(cfg.NUT.UPS)
		if err != nil {
			log.Error("list vars", "err", err)
			os.Exit(2)
		}
		for k, v := range vars {
			fmt.Printf("%s: %s\n", k, v)
		}
		return
	}

	notifiers := cfg.BuildNotifiers()
	disp := notifier.NewDispatcher(log, notifiers...)
	mon := monitor.New(cfg.MonitorRuntimeConfig(), dialer, disp, log)

	log.Info("starting",
		"address", cfg.NUT.Address,
		"ups", cfg.NUT.UPS,
		"notifiers", len(notifiers),
	)
	if err := mon.Run(ctx); err != nil {
		log.Error("monitor", "err", err)
		os.Exit(2)
	}
}

func buildTLSConfig(t *config.TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.InsecureSkipVerify, //#nosec G402 — opt-in
		ServerName:         t.ServerName,
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %q: no certs found", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
