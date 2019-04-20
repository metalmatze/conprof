package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Go-SIP/conprof/config"
	"github.com/Go-SIP/conprof/scrape"
	"github.com/Go-SIP/conprof/storage/tsdb"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

// registerSampler registers a sampler command.
func registerSampler(m map[string]setupFunc, app *kingpin.Application, name string) {
	cmd := app.Command(name, "Run a sampler, that appends profiles to a configured storage.")

	storagePath := cmd.Flag("storage.path", "Directory to read storage from.").
		Default("./data").String()
	configFile := cmd.Flag("config.file", "Config file to use.").
		Default("conprof.yaml").String()

	m[name] = func(g *run.Group, mux *http.ServeMux, logger log.Logger, reg *prometheus.Registry, tracer opentracing.Tracer, debugLogging bool) error {
		db, err := tsdb.Open(*storagePath, logger, prometheus.DefaultRegisterer, tsdb.DefaultOptions)
		if err != nil {
			return err
		}
		return runSampler(g, logger, db, *configFile)
	}
}

func runSampler(g *run.Group, logger log.Logger, db *tsdb.DB, configFile string) error {
	scrapeManager := scrape.NewManager(log.With(logger, "component", "scrape-manager"), db)
	c, err := config.LoadFile(configFile)
	if err != nil {
		return fmt.Errorf("could not load config: %v", err)
	}

	syncCh := make(chan map[string][]*targetgroup.Group)

	{
		manager := discovery.NewManager(context.TODO(), logger)

		g.Add(func() error {
			return manager.Run()
		}, func(err error) {

		})
	}

	{
		_, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			err = scrapeManager.ApplyConfig(c)
			if err != nil {
				return fmt.Errorf("could not apply config: %v", err)
			}
			scrapeManager.Run(syncCh)

			return nil
		}, func(error) {
			level.Debug(logger).Log("msg", "shutting down scrape manager")
			scrapeManager.Stop()
			cancel()
			close(syncCh)
		})
	}
	{
		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			for _, sc := range c.ScrapeConfigs {
				select {
				case <-ctx.Done():
					return nil
				case syncCh <- map[string][]*targetgroup.Group{sc.JobName: sc.ServiceDiscoveryConfig.StaticConfigs}:
					// continue
				}
			}
			<-ctx.Done()

			return nil
		}, func(error) {
			level.Debug(logger).Log("msg", "shutting down discovery")
			cancel()
		})
	}
	return nil
}
