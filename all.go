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
	"time"

	"github.com/conprof/db/tsdb"
	"github.com/conprof/db/tsdb/wal"
	"github.com/go-kit/kit/log"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/extkingpin"
	"github.com/thanos-io/thanos/pkg/prober"
	"gopkg.in/alecthomas/kingpin.v2"
)

type AllCmd struct {
	configFileFlag
	httpFlags

	StoragePath string        `name:"storage.tsdb.path" type:"path" default:"./data" help:"Directory to read storage from."`
	Retention   time.Duration `name:"storage.tsdb.retention.time" default:"360h" help:"How long to retain raw samples on local storage. 0d - disables this retention"` // TODO: Support Prometheus duration
}

func (a *AllCmd) Run(cli *cliContext) error {
	_, err := tsdb.Open(
		a.StoragePath,
		cli.logger,
		cli.registry,
		&tsdb.Options{
			RetentionDuration:      int64(a.Retention),
			WALSegmentSize:         wal.DefaultSegmentSize,
			MinBlockDuration:       tsdb.DefaultBlockDuration,
			MaxBlockDuration:       int64(a.Retention) / 10,
			NoLockfile:             true,
			AllowOverlappingBlocks: false,
			WALCompression:         true,
			StripeSize:             tsdb.DefaultStripeSize,
		},
	)
	if err != nil {
		return err
	}

	//if err := runSampler(g, p, logger, db, a.ConfigFile, nil, reloadCh, reloaders); err != nil {
	//	return err
	//
	//}
	//
	//if err = runWeb(mux, p, reg, logger, db, reloadCh, reloaders, a.MaxMergeBatchSize); err != nil {
	//	return err
	//}

	return nil
}

// registerAll registers the all command.
func registerAll(m map[string]setupFunc, app *kingpin.Application, name string, reloadCh chan struct{}, reloaders *configReloaders) {
	cmd := app.Command(name, "All in one command.")

	storagePath := cmd.Flag("storage.tsdb.path", "Directory to read storage from.").
		Default("./data").String()
	configFile := cmd.Flag("config.file", "Config file to use.").
		Default("conprof.yaml").String()
	retention := extkingpin.ModelDuration(cmd.Flag("storage.tsdb.retention.time", "How long to retain raw samples on local storage. 0d - disables this retention").Default("15d"))
	maxMergeBatchSize := cmd.Flag("max-merge-batch-size", "Bytes loaded in one batch for merging. This is to limit the amount of memory a merge query can use.").
		Default("64MB").Bytes()

	m[name] = func(comp component.Component, g *run.Group, mux httpMux, probe prober.Probe, logger log.Logger, reg *prometheus.Registry, debugLogging bool) (prober.Probe, error) {
		return runAll(
			comp,
			g,
			mux,
			probe,
			reg,
			logger,
			*storagePath,
			*configFile,
			*retention,
			reloadCh,
			reloaders,
			int64(*maxMergeBatchSize),
		)
	}
}

func runAll(
	comp component.Component,
	gOld *run.Group,
	mux httpMux,
	p prober.Probe,
	reg prometheus.Registerer,
	logger log.Logger,
	storagePath,
	configFile string,
	retention model.Duration,
	reloadCh chan struct{},
	reloaders *configReloaders,
	maxMergeBatchSize int64,
) (prober.Probe, error) {
	db, err := tsdb.Open(
		storagePath,
		logger,
		prometheus.DefaultRegisterer,
		&tsdb.Options{
			RetentionDuration:      int64(retention),
			WALSegmentSize:         wal.DefaultSegmentSize,
			MinBlockDuration:       tsdb.DefaultBlockDuration,
			MaxBlockDuration:       int64(retention) / 10,
			NoLockfile:             true,
			AllowOverlappingBlocks: false,
			WALCompression:         true,
			StripeSize:             tsdb.DefaultStripeSize,
		},
	)
	if err != nil {
		return nil, err
	}

	var g run.Group
	{
		s, err := NewSampler(
			WithLogger(logger),
			WithConfigFile(configFile),
			WithReloaders(reloaders),
		)
		if err != nil {
			return nil, err
		}

		g.Add(func() error {
			return s.Run(p, db, reloadCh)
		}, func(err error) {
		})
	}
	{
		ws := NewWebserver("",
			WebserverWithLogger(logger),
			//WebserverWithRegistry(reg),
		)

		g.Add(func() error {
			return ws.Run(db, maxMergeBatchSize, reloadCh, reloaders)
		}, func(err error) {
		})
	}

	return p, g.Run()
}
