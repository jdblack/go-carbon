package main

import (
	"expvar"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"

	"github.com/lomik/zapwriter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	daemon "github.com/sevlyar/go-daemon"
	"go.uber.org/zap"

	"github.com/go-graphite/go-carbon/carbon"
	"github.com/go-graphite/go-carbon/points"

	_ "net/http/pprof"
)

// Version of go-carbon
const Version = "0.16.2"

var BuildVersion = "(development version)"

func httpServe(addr string) (func(), error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, err
	}

	go http.Serve(listener, nil)
	return func() { listener.Close() }, nil
}

func main() {
	var err error

	/* CONFIG start */

	configFile := flag.String("config", "", "Filename of config")
	printDefaultConfig := flag.Bool("config-print-default", false, "Print default config")
	checkConfig := flag.Bool("check-config", false, "Check config and exit")

	printVersion := flag.Bool("version", false, "Print version")

	isDaemon := flag.Bool("daemon", false, "Run in background")
	pidfile := flag.String("pidfile", "", "Pidfile path (only for daemon)")

	cat := flag.String("cat", "", "Print cache dump file")

	flag.Parse()

	if *printVersion {
		fmt.Println(Version)
		fmt.Println(BuildVersion)
		return
	}

	if *cat != "" {
		err = points.ReadFromFile(*cat, func(p *points.Points) {
			for _, d := range p.Data { // every metric point
				fmt.Printf("%s %v %v\n", p.Metric, d.Value, d.Timestamp)
			}
		})
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	if *printDefaultConfig {
		if err = carbon.PrintDefaultConfig(); err != nil {
			log.Fatal(err)
		}
		return
	}

	app := carbon.New(*configFile)

	if err = app.ParseConfig(); err != nil {
		log.Fatal(err)
	}

	cfg := app.Config

	var runAsUser *user.User
	if cfg.Common.User != "" {
		runAsUser, err = user.Lookup(cfg.Common.User)
		if err != nil {
			log.Fatal(err)
		}
	}

	// config parsed successfully. Exit in check-only mode
	if *checkConfig {
		return
	}

	for i := 0; i < len(cfg.Logging); i++ {
		if err := zapwriter.PrepareFileForUser(cfg.Logging[i].File, runAsUser); err != nil {
			log.Fatal(err)
		}
	}

	if err = zapwriter.ApplyConfig(cfg.Logging); err != nil {
		log.Fatal(err)
	}

	mainLogger := zapwriter.Logger("main")

	if *isDaemon {
		runtime.LockOSThread()

		context := new(daemon.Context)
		if *pidfile != "" {
			context.PidFileName = *pidfile
			context.PidFilePerm = 0644
		}

		if runAsUser != nil {
			uid, err := strconv.ParseInt(runAsUser.Uid, 10, 0)
			if err != nil {
				mainLogger.Fatal(err.Error())
			}

			gid, err := strconv.ParseInt(runAsUser.Gid, 10, 0)
			if err != nil {
				mainLogger.Fatal(err.Error())
			}

			context.Credential = &syscall.Credential{
				Uid: uint32(uid),
				Gid: uint32(gid),
			}
		}

		child, _ := context.Reborn()

		if child != nil {
			return
		}
		defer context.Release()

		runtime.UnlockOSThread()
	}
	/* CONFIG end */

	// pprof
	// httpStop := func() {}
	if cfg.Pprof.Enabled || cfg.Prometheus.Enabled {
		_, err = httpServe(cfg.Pprof.Listen)
		if err != nil {
			mainLogger.Fatal(err.Error())
		}
	}

	if cfg.Pprof.Enabled {
		expvar.NewString("GoVersion").Set(runtime.Version())
		expvar.NewString("BuildVersion").Set(BuildVersion)
		expvar.NewString("Version").Set(Version)
		expvar.Publish("Config", expvar.Func(func() interface{} { return cfg }))
		expvar.Publish("GoroutineCount", expvar.Func(func() interface{} { return runtime.NumGoroutine() }))
	}

	if cfg.Prometheus.Enabled {
		app.PromRegisterer.MustRegister(prometheus.NewGoCollector())
		app.PromRegisterer.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
		http.Handle(cfg.Prometheus.Endpoint, promhttp.HandlerFor(app.PromRegistry, promhttp.HandlerOpts{}))
	}
	if err = app.Start(BuildVersion); err != nil {
		mainLogger.Fatal(err.Error())
	} else {
		mainLogger.Info("started")
	}

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGUSR2)
		for {
			<-c
			app.DumpStop()
			os.Exit(0)
		}
	}()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		for {
			<-c
			mainLogger.Info("HUP received. Reload config")
			if err := app.ReloadConfig(); err != nil {
				mainLogger.Error("config reload failed", zap.Error(err))
			} else {
				mainLogger.Info("config successfully reloaded")
			}
		}
	}()

	app.Loop()

	mainLogger.Info("stopped")
}
