// Copyright 2018 The conprof Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/conprof/db/storage"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/oklog/run"
	"github.com/prometheus/prometheus/discovery"
	_ "github.com/prometheus/prometheus/discovery/install" // Register service discovery implementations.
	"github.com/thanos-io/thanos/pkg/prober"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/conprof/conprof/config"
	"github.com/conprof/conprof/pkg/store"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/conprof/scrape"
)

type SamplerCmd struct {
	configFileFlag
	storeAddressFlag
	maxMergeBatchSizeFlag

	Targets []string `name:"target" help:"Target to scrape"` // TODO: Are these URLs?

	BearerToken     string `name:"bearer-token" help:"Bearer token to authenticate with store."`
	BearerTokenFile string `name:"bearer-token-file" type:"path" help:"File to read bearer token from to authenticate with store."`

	Insecure           bool `name:"insecure" help:"Send gRPC requests via plaintext instead of TLS."`
	InsecureSkipVerify bool `name:"insecure-skip-verify" help:"Skip TLS certificate verification."`
}

func (cmd *SamplerCmd) Run(cli *cliContext) error {
	met := grpc_prometheus.NewClientMetrics()
	met.EnableClientHandlingTimeHistogram()
	cli.registry.MustRegister(met)

	opts := []grpc.DialOption{
		grpc.WithUnaryInterceptor(
			met.UnaryClientInterceptor(),
		),
	}

	if cmd.Insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		opts = append(opts,
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				InsecureSkipVerify: cmd.InsecureSkipVerify,
			})),
		)
	}

	if cmd.BearerToken != "" && cmd.BearerTokenFile != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
			token:    cmd.BearerToken,
			insecure: cmd.Insecure,
		}))
	}

	if cmd.BearerTokenFile != "" {
		b, err := ioutil.ReadFile(cmd.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("failed to read bearer token from file: %w", err)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
			token:    string(b),
			insecure: cmd.Insecure,
		}))
	}

	conn, err := grpc.Dial(cmd.StoreAddress, opts...)
	if err != nil {
		return err
	}
	c := storepb.NewWritableProfileStoreClient(conn)

	samplerOpts := []SamplerOption{
		WithLogger(cli.logger),
		WithReloaders(cli.reloaders),
	}

	if cmd.ConfigFile != "" {
		samplerOpts = append(samplerOpts, WithConfigFile(cmd.ConfigFile))
	}

	if len(cmd.Targets) > 0 {
		samplerOpts = append(samplerOpts, WithTargets(cmd.Targets))
	}

	s, err := NewSampler(samplerOpts...)
	if err != nil {
		return fmt.Errorf("failed to setup sampler: %w", err)
	}

	return s.Run(cli.probe, store.NewGRPCAppendable(cli.logger, c), cli.reloadCh)
}

type perRequestBearerToken struct {
	token    string
	insecure bool
}

func (t *perRequestBearerToken) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (t *perRequestBearerToken) RequireTransportSecurity() bool {
	return !t.insecure
}

func getScrapeConfigs(cfg *config.Config) map[string]discovery.Configs {
	c := make(map[string]discovery.Configs)
	for _, v := range cfg.ScrapeConfigs {
		c[v.JobName] = v.ServiceDiscoveryConfigs
	}
	return c
}

type Sampler struct {
	logger     log.Logger
	configFile string
	cfg        *config.Config
	reloaders  *configReloaders
}

type SamplerOption func(s *Sampler) error

func NewSampler(opts ...SamplerOption) (*Sampler, error) {
	s := &Sampler{
		logger: log.NewNopLogger(),
	}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func WithLogger(logger log.Logger) SamplerOption {
	return func(s *Sampler) error {
		s.logger = logger
		return nil
	}
}

func WithConfigFile(path string) SamplerOption {
	return func(s *Sampler) error {
		cfg, err := config.LoadFile(path)
		if err != nil {
			return fmt.Errorf("could not load config: %w", err)
		}
		s.cfg = cfg
		s.configFile = path
		return nil
	}
}

func WithReloaders(reloaders *configReloaders) SamplerOption {
	return func(s *Sampler) error {
		s.reloaders = reloaders
		return nil
	}
}

func WithTargets(targets []string) SamplerOption {
	const tmpConfig = `
scrape_configs:
- job_name: 'default'
  scrape_interval: 1m
  scrape_timeout: 1m
  static_configs:
  - targets: [%s]
`

	return func(s *Sampler) error {
		targetStrings := []string{}
		for _, t := range targets {
			targetStrings = append(targetStrings, fmt.Sprintf("\"%s\"", t))
		}
		tmpfile, err := ioutil.TempFile("", "conprof")
		if err != nil {
			return fmt.Errorf("could not create tempfile: %v", err)
		}

		content := fmt.Sprintf(tmpConfig, strings.Join(targetStrings, ","))
		if _, err := tmpfile.Write([]byte(content)); err != nil {
			return fmt.Errorf("could write tempfile: %v", err)
		}
		if err := tmpfile.Close(); err != nil {
			return fmt.Errorf("could close tempfile: %v", err)
		}

		configFile := tmpfile.Name()
		s.cfg, err = config.LoadFile(configFile)
		if err != nil {
			return fmt.Errorf("could not load config: %v", err)
		}

		return nil
	}
}

// Register the sampler to the run.Group to be run concurrently.
func (s *Sampler) Run(probe prober.Probe, db storage.Appendable, reloadCh chan struct{}) error {
	scrapeManager := scrape.NewManager(log.With(s.logger, "component", "scrape-manager"), db)

	s.reloaders.Register(scrapeManager.ApplyConfig)

	ctxScrape, cancelScrape := context.WithCancel(context.Background())
	discoveryManagerScrape := discovery.NewManager(ctxScrape, log.With(s.logger, "component", "discovery manager scrape"), discovery.Name("scrape"))

	s.reloaders.Register(func(cfg *config.Config) error {
		c := getScrapeConfigs(cfg)
		for _, v := range cfg.ScrapeConfigs {
			c[v.JobName] = v.ServiceDiscoveryConfigs
		}
		return discoveryManagerScrape.ApplyConfig(c)
	})

	var g run.Group
	{
		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-reloadCh:
					level.Info(s.logger).Log("msg", "Reloading configuration")
					cfg, err := config.LoadFile(s.configFile)
					if err != nil {
						level.Error(s.logger).Log("could not load config to reload: %v", err)
					}

					for _, reloader := range s.reloaders.funcs {
						if err := reloader(cfg); err != nil {
							level.Error(s.logger).Log("could not reload scrape configs: %v", err)
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		err := discoveryManagerScrape.ApplyConfig(getScrapeConfigs(s.cfg))
		if err != nil {
			level.Error(s.logger).Log("msg", err)
			cancelScrape()
			return err
		}
		// Scrape discovery manager.
		g.Add(
			func() error {
				err := discoveryManagerScrape.Run()
				level.Info(s.logger).Log("msg", "Scrape discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(s.logger).Log("msg", "Stopping scrape discovery manager...")
				cancelScrape()
			},
		)
	}
	{
		_, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			err := scrapeManager.ApplyConfig(s.cfg)
			if err != nil {
				return fmt.Errorf("could not apply config: %v", err)
			}
			return scrapeManager.Run(discoveryManagerScrape.SyncCh())
		}, func(error) {
			level.Debug(s.logger).Log("msg", "shutting down scrape manager")
			scrapeManager.Stop()
			cancel()
		})
	}

	probe.Ready()

	return g.Run()
}
