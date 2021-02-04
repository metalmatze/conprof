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
	"net/http"
	"time"

	"github.com/conprof/db/storage"
	"github.com/go-kit/kit/log"
	"github.com/julienschmidt/httprouter"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/thanos-io/thanos/pkg/prober"
	httpserver "github.com/thanos-io/thanos/pkg/server/http"
	"google.golang.org/grpc"

	conprofapi "github.com/conprof/conprof/api"
	"github.com/conprof/conprof/pkg/store"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/conprof/pprofui"
	"github.com/conprof/conprof/web"
)

type WebCmd struct {
	storeAddressFlag
	maxMergeBatchSizeFlag
	httpFlags
}

func (cmd *WebCmd) Run(cli *cliContext) error {
	conn, err := grpc.Dial(cmd.StoreAddress, grpc.WithInsecure())
	if err != nil {
		return err
	}
	c := storepb.NewReadableProfileStoreClient(conn)

	ws := NewWebserver(cmd.HTTPAddress,
		WebserverWithLogger(cli.logger),
		WebserverWithRegistry(cli.registry),
		WebserverGracePeriod(cmd.HTTPGracePeriod),
	)

	return ws.Run(store.NewGRPCQueryable(c), cmd.MaxMergeBatchSize, cli.reloadCh, cli.reloaders)
}

type Webserver struct {
	addr        string
	gradePeriod time.Duration
	logger      log.Logger
	registry    *prometheus.Registry
	apiOnly     bool
}

func (w *Webserver) String() string {
	return "web" // For Thanos compatibility
}

type WebserverOption func(w *Webserver)

func NewWebserver(addr string, opts ...WebserverOption) *Webserver {
	w := &Webserver{
		addr:     addr,
		logger:   log.NewNopLogger(),
		registry: prometheus.NewRegistry(),
	}

	for _, opt := range opts {
		opt(w)
	}

	return w
}

func WebserverWithLogger(logger log.Logger) WebserverOption {
	return func(w *Webserver) {
		w.logger = logger
	}
}

func WebserverWithRegistry(registry *prometheus.Registry) WebserverOption {
	return func(w *Webserver) {
		w.registry = registry
	}
}

func WebserverGracePeriod(duration time.Duration) WebserverOption {
	return func(w *Webserver) {
		w.gradePeriod = duration
	}
}

func WebserverAPIOnly() WebserverOption {
	return func(w *Webserver) {
		w.apiOnly = true
	}
}

func (w *Webserver) Run(db storage.Queryable, maxMergeBatchSize int64, reloadCh chan struct{}, reloaders *configReloaders) error {
	mux := http.NewServeMux()

	api := conprofapi.New(w.logger, w.registry, db, reloadCh, maxMergeBatchSize)
	mux.Handle("/api/v1", api.Router())

	//router.GET("/api/v1/query_range", instr("query_range", api.QueryRange))
	//router.GET("/api/v1/query", instr("query", api.Query))
	//router.GET("/api/v1/series", instr("series", api.Series))
	//router.GET("/api/v1/labels", instr("label_names", api.LabelNames))
	//router.GET("/api/v1/label/:name/values", instr("label_values", api.LabelValues))
	//router.GET("/api/v1/status/config", instr("config", api.Config))

	reloaders.Register(api.ApplyConfig)

	if !w.apiOnly {
		logger := log.With(w.logger, "component", "pprofui")
		ui := pprofui.New(logger, db)

		router := httprouter.New()
		router.RedirectTrailingSlash = false
		router.GET("/pprof/*remainder", ui.PprofView)
		router.GET("/download/*remainder", ui.PprofDownload)

		router.GET("/-/reload", api.Reload)
		router.NotFound = http.FileServer(web.Assets)

		mux.Handle("/", router)
	}

	//probe.Ready()

	var g run.Group
	{

	}
	{
		s := httpserver.New(w.logger, w.registry, w, &prober.HTTPProbe{},
			httpserver.WithListen(w.addr),
			httpserver.WithGracePeriod(w.gradePeriod),
		)

		g.Add(func() error {
			return s.ListenAndServe()
		}, func(err error) {
			s.Shutdown(err)
		})
	}

	return g.Run()
}
