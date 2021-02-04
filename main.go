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
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/prober"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv"

	"github.com/conprof/conprof/config"
)

type httpMux interface {
	Handle(pattern string, handler http.Handler)
}

type setupFunc func(component.Component, *run.Group, httpMux, prober.Probe, log.Logger, *prometheus.Registry, bool) (prober.Probe, error)

type configReloaders struct {
	funcs []func(*config.Config) error
}

func (r *configReloaders) Register(reloader func(*config.Config) error) {
	r.funcs = append(r.funcs, reloader)
}

var cli struct {
	Debug     bool   `env:"DEBUG" help:"Enable debug mode"`
	LogLevel  string `name:"log.level" enum:"debug,info,warn,error" default:"info" help:"Set the logging level (debug,info,warn,error)"`
	LogFormat string `name:"log.format" enum:"logfmt,json" default:"logfmt" help:"Log format to use (logfmt,json)."`

	API     APICmd     `cmd help:"Run an API to query profiles from a storage."`
	All     AllCmd     `cmd help:"All in one command."`
	Sampler SamplerCmd `cmd help:"Run a sampler, that appends profiles to a configured storage."`
	Web     WebCmd     `cmd help:"Run a web interface to view profiles from a storage."`
}

type cliContext struct {
	debug     bool
	group     *run.Group
	probe     *prober.HTTPProbe
	logger    log.Logger
	registry  *prometheus.Registry
	reloadCh  chan struct{}
	reloaders *configReloaders
}

type httpFlags struct {
	HTTPAddress     string        `name:"http-address" default:"0.0.0.0:10902" help:"Listen host:port for HTTP endpoints."`
	HTTPGracePeriod time.Duration `name:"http-grace-period" default:"2m" help:"Time to wait after an interrupt received for HTTP Server."` // TODO use model.Duration
}

// configFileFlag is used on multiple commands but not globally in all commands.
type configFileFlag struct {
	ConfigFile string `name:"config.file" type:"path" default:"./conprof.yaml" help:"Config file to use."`
}

// storeAddressFlag is used on multiple commands but not globally in all commands.
type storeAddressFlag struct {
	StoreAddress string `name:"store" default:"127.0.0.1:10901" help:"Address of statically configured store."`
}

// maxMergeBatchSizeFlag is used on multiple commands but not globally in all commands.
type maxMergeBatchSizeFlag struct {
	MaxMergeBatchSize int64 `name:"max-merge-batch-size" default:"68719476736" help:"Bytes loaded in one batch for merging. This is to limit the amount of memory a merge query can use."` // TODO: Use units.Base2Bytes
}

func main() {
	ctx := kong.Parse(&cli,
		kong.Name("conprof"),
		kong.Description("Continuous profiling - to have a profile when it matters."),
		kong.UsageOnError(),
		kong.Vars{
			"version": version.Print("conprof"),
		},
	)

	if cli.Debug {
		runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(10)
	}

	var logger log.Logger
	{
		if cli.LogFormat == "json" {
			logger = log.NewJSONLogger(log.NewSyncWriter(os.Stderr))
		} else {
			logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
		}

		var lvl level.Option
		switch strings.ToLower(cli.LogLevel) {
		case "error":
			lvl = level.AllowError()
		case "warn":
			lvl = level.AllowWarn()
		case "info":
			lvl = level.AllowInfo()
		case "debug":
			lvl = level.AllowDebug()
		default:
			panic("unexpected log level")
		}
		logger = level.NewFilter(logger, lvl)

		//if *debugName != "" {
		//	logger = log.With(logger, "name", *debugName)
		//}

		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	}
	var registry *prometheus.Registry
	{
		registry = prometheus.NewRegistry()
		registry.MustRegister(
			version.NewCollector("conprof"),
			prometheus.NewGoCollector(),
			prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		)
		prometheus.DefaultRegisterer = registry
	}

	reloadCh := make(chan struct{}, 1)
	reloaders := &configReloaders{}

	var g run.Group

	//mux := http.NewServeMux()
	httpProbe := prober.NewHTTP()

	err := ctx.Run(&cliContext{
		debug:     cli.Debug,
		group:     &g,
		probe:     httpProbe,
		logger:    logger,
		registry:  registry,
		reloadCh:  reloadCh,
		reloaders: reloaders,
	})
	ctx.FatalIfErrorf(err)

	//if *otlpAddress != "" {
	//	initTracer(logger, cmd, *otlpAddress)
	//}

	//statusProber, err := cmds[cmd](comp, &g, mux, httpProbe, logger, reg, *logLevel == "debug")
	//if err != nil {
	//	fmt.Fprintln(os.Stderr, errors.Wrapf(err, "%s command failed", cmd))
	//	os.Exit(1)
	//}
	//
	//{
	//	srv := httpserver.New(logger, reg, comp, httpProbe,
	//		httpserver.WithListen(*httpBindAddr),
	//		httpserver.WithGracePeriod(time.Duration(*httpGracePeriod)),
	//	)
	//	srv.Handle("/", cors(*corsOrigin, *corsMethods, mux))
	//	g.Add(func() error {
	//		statusProber.Healthy()
	//
	//		return srv.ListenAndServe()
	//	}, func(err error) {
	//		statusProber.NotReady(err)
	//		defer statusProber.NotHealthy(err)
	//
	//		srv.Shutdown(err)
	//	})
	//}

	// Listen for termination signals.
	g.Add(run.SignalHandler(context.Background(), syscall.SIGINT, syscall.SIGTERM))

	if err := g.Run(); err != nil {
		level.Error(logger).Log("msg", "running command failed", "err", err)
		os.Exit(1)
	}

	level.Info(logger).Log("msg", "exiting")
}

//	app := kingpin.New(filepath.Base(os.Args[0]), "Continuous profiling - to have a profile when it matters.")
//	app.Version(version.Print("conprof"))
//	app.HelpFlag.Short('h')
//
//	debugName := app.Flag("debug.name", "Name to add as prefix to log lines.").Hidden().String()
//
//	logLevel := app.Flag("log.level", "Log filtering level.").
//		Default("info").Enum("error", "warn", "info", "debug")
//	logFormat := app.Flag("log.format", "Log format to use.").
//		Default(logFormatLogfmt).Enum(logFormatLogfmt, logFormatJSON)
//	otlpAddress := app.Flag("otlp-address", "OpenTelemetry collector address to send traces to.").
//		Default("").String()
//	corsOrigin := app.Flag("cors.access-control-allow-origin", "Cross-origin resource sharing allowed origins.").
//		Default("").String()
//	corsMethods := app.Flag("cors.access-control-allow-methods", "Cross-origin resource sharing allowed methods.").
//		Default("").String()
//	httpBindAddr, httpGracePeriod := extkingpin.RegisterHTTPFlags(app)
//
//	cmds := map[string]setupFunc{}
//	reloadCh := make(chan struct{}, 1)
//
//	reloaders := &configReloaders{}
//
//	registerSampler(cmds, app, "sampler", reloadCh, reloaders)
//	registerStorage(cmds, app, "storage", reloadCh)
//	registerWeb(cmds, app, "web", reloadCh, reloaders)
//	registerApi(cmds, app, "api")
//	registerAll(cmds, app, "all", reloadCh, reloaders)
//
//	cmd, err := app.Parse(os.Args[1:])
//	if err != nil {
//		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "Error parsing commandline arguments"))
//		app.Usage(os.Args[1:])
//		os.Exit(2)
//	}
//
//	var logger log.Logger
//	{
//		var lvl level.Option
//		switch *logLevel {
//		case "error":
//			lvl = level.AllowError()
//		case "warn":
//			lvl = level.AllowWarn()
//		case "info":
//			lvl = level.AllowInfo()
//		case "debug":
//			lvl = level.AllowDebug()
//		default:
//			panic("unexpected log level")
//		}
//		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
//		if *logFormat == logFormatJSON {
//			logger = log.NewJSONLogger(log.NewSyncWriter(os.Stderr))
//		}
//		logger = level.NewFilter(logger, lvl)
//
//		if *debugName != "" {
//			logger = log.With(logger, "name", *debugName)
//		}
//
//		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
//	}
//
//	reg := prometheus.NewRegistry()
//	reg.MustRegister(
//		version.NewCollector("conprof"),
//		prometheus.NewGoCollector(),
//		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
//	)
//
//	prometheus.DefaultRegisterer = reg
//
//	var g run.Group
//	mux := http.NewServeMux()
//	httpProbe := prober.NewHTTP()
//	comp := componentString(cmd)
//	if *otlpAddress != "" {
//		initTracer(logger, cmd, *otlpAddress)
//	}
//
//	statusProber, err := cmds[cmd](comp, &g, mux, httpProbe, logger, reg, *logLevel == "debug")
//	if err != nil {
//		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "%s command failed", cmd))
//		os.Exit(1)
//	}
//
//	{
//		srv := httpserver.New(logger, reg, comp, httpProbe,
//			httpserver.WithListen(*httpBindAddr),
//			httpserver.WithGracePeriod(time.Duration(*httpGracePeriod)),
//		)
//		srv.Handle("/", cors(*corsOrigin, *corsMethods, mux))
//		g.Add(func() error {
//			statusProber.Healthy()
//
//			return srv.ListenAndServe()
//		}, func(err error) {
//			statusProber.NotReady(err)
//			defer statusProber.NotHealthy(err)
//
//			srv.Shutdown(err)
//		})
//	}
//
//	// Listen for termination signals.
//	g.Add(run.SignalHandler(context.Background(), syscall.SIGINT, syscall.SIGTERM))
//
//	if err := g.Run(); err != nil {
//		level.Error(logger).Log("msg", "running command failed", "err", err)
//		os.Exit(1)
//	}
//	level.Info(logger).Log("msg", "exiting")
//}

func cors(corsOrigin, corsMethods string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
		}
		if corsMethods != "" {
			w.Header().Set("Access-Control-Allow-Methods", corsMethods)
		}
		h.ServeHTTP(w, r)
	})
}

func initTracer(logger log.Logger, serviceName string, otlpAddress string) func() {
	ctx := context.Background()
	exporter, err := otlp.NewExporter(
		ctx,
		otlp.WithInsecure(),
		otlp.WithAddress(otlpAddress),
	)
	handleErr(logger, err, "failed to create exporter")

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	handleErr(logger, err, "failed to create resource")

	bsp := sdktrace.NewBatchSpanProcessor(exporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithConfig(sdktrace.Config{DefaultSampler: sdktrace.AlwaysSample()}),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)

	// set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tracerProvider)

	return func() {
		handleErr(logger, exporter.Shutdown(context.Background()), "failed to stop exporter")
	}
}

func handleErr(logger log.Logger, err error, message string) {
	if err != nil {
		level.Error(logger).Log("msg", message, "err", err)
		os.Exit(1)
	}
}
