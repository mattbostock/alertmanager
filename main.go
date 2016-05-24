// Copyright 2015 Prometheus Team
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
	"flag"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/version"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/provider/boltmem"
	"github.com/prometheus/alertmanager/provider/gossip"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/weaveworks/mesh"
)

var (
	configSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "alertmanager",
		Name:      "config_last_reload_successful",
		Help:      "Whether the last configuration reload attempt was successful.",
	})
	configSuccessTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "alertmanager",
		Name:      "config_last_reload_success_timestamp_seconds",
		Help:      "Timestamp of the last successful configuration reload.",
	})
)

func init() {
	prometheus.MustRegister(configSuccess)
	prometheus.MustRegister(configSuccessTime)
	prometheus.MustRegister(version.NewCollector("alertmanager"))
}

func main() {
	peers := &stringset{}
	var (
		showVersion = flag.Bool("version", false, "print version information.")

		configFile = flag.String("config.file", "alertmanager.yml", "Alertmanager configuration file name.")
		dataDir    = flag.String("storage.path", "data/", "base path for data storage.")

		externalURL   = flag.String("web.external-url", "", "URL under which Alertmanager is externally reachable")
		listenAddress = flag.String("web.listen-address", ":9093", "address to listen on for the web interface and API.")

		meshListen = flag.String("mesh", net.JoinHostPort("0.0.0.0", strconv.Itoa(mesh.Port)), "mesh listen address")
		hwaddr     = flag.String("hwaddr", mustHardwareAddr(), "MAC address, i.e. mesh peer ID")
		nickname   = flag.String("nickname", mustHostname(), "peer nickname")
	)
	flag.Var(peers, "peer", "initial peer (may be repeated)")
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("alertmanager"))
		os.Exit(0)
	}

	log.Infoln("Starting alertmanager", version.Info())
	log.Infoln("Build context", version.BuildContext())

	host, portStr, err := net.SplitHostPort(*meshListen)
	if err != nil {
		log.Fatalf("mesh address: %s: %v", *meshListen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("mesh address: %s: %v", *meshListen, err)
	}

	name, err := mesh.PeerNameFromString(*hwaddr)
	if err != nil {
		log.Fatalf("%s: %v", *hwaddr, err)
	}

	mrouter := mesh.NewRouter(mesh.Config{
		Host:               host,
		Port:               port,
		ProtocolMinVersion: mesh.ProtocolMinVersion,
		Password:           []byte(""),
		ConnLimit:          64,
		PeerDiscovery:      true,
		TrustedSubnets:     []*net.IPNet{},
	}, name, *nickname, mesh.NullOverlay{}, stdlog.New(ioutil.Discard, "", 0))

	ni := gossip.NewNotificationInfo(name, log.With("peer", *nickname))
	nic := mrouter.NewGossip("notify_info", ni)
	ni.Register(nic)

	marker := types.NewMarker()

	silences := gossip.NewSilences(name, marker, log.With("peer", *nickname))
	silc := mrouter.NewGossip("silences", silences)
	silences.Register(silc)

	mrouter.Start()
	defer mrouter.Stop()

	mrouter.ConnectionMaker.InitiateConnections(peers.slice(), true)

	if err := os.MkdirAll(*dataDir, 0777); err != nil {
		log.Fatal(err)
	}

	alerts, err := boltmem.NewAlerts(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer alerts.Close()

	//notifies, err := boltmem.NewNotificationInfo(*dataDir)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//defer notifies.Close()

	//silences, err := boltmem.NewSilences(*dataDir, marker)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//defer silences.Close()

	var (
		inhibitor *Inhibitor
		tmpl      *template.Template
		disp      *Dispatcher
	)
	defer disp.Stop()

	api := NewAPI(alerts, silences, func() AlertOverview {
		return disp.Groups()
	})

	build := func(rcvs []*config.Receiver) notify.Notifier {
		var (
			router  = notify.Router{}
			fanouts = notify.Build(rcvs, tmpl)
		)
		for name, fo := range fanouts {
			for i, n := range fo {
				n = notify.Retry(n)
				n = notify.Log(n, log.With("step", "retry"))
				//n = notify.Dedup(notifies, n)
				n = notify.Dedup(ni, n)
				n = notify.Log(n, log.With("step", "dedup"))

				fo[i] = n
			}
			router[name] = fo
		}
		n := notify.Notifier(router)

		n = notify.Log(n, log.With("step", "route"))
		n = notify.Silence(silences, n, marker)
		n = notify.Log(n, log.With("step", "silence"))
		n = notify.Inhibit(inhibitor, n, marker)
		n = notify.Log(n, log.With("step", "inhibit"))

		return n
	}

	amURL, err := extURL(*externalURL, *listenAddress)
	if err != nil {
		log.Fatal(err)
	}

	reload := func() (err error) {
		log.With("file", *configFile).Infof("Loading configuration file")
		defer func() {
			if err != nil {
				log.With("file", *configFile).Errorf("Loading configuration file failed: %s", err)
				configSuccess.Set(0)
			} else {
				configSuccess.Set(1)
				configSuccessTime.Set(float64(time.Now().Unix()))
			}
		}()

		conf, err := config.LoadFile(*configFile)
		if err != nil {
			return err
		}

		api.Update(conf.String(), time.Duration(conf.Global.ResolveTimeout))

		tmpl, err = template.FromGlobs(conf.Templates...)
		if err != nil {
			return err
		}
		tmpl.ExternalURL = amURL

		disp.Stop()

		inhibitor = NewInhibitor(alerts, conf.InhibitRules, marker)
		disp = NewDispatcher(alerts, NewRoute(conf.Route, nil), build(conf.Receivers), marker)

		go disp.Run()

		return nil
	}

	if err := reload(); err != nil {
		os.Exit(1)
	}

	router := route.New()

	RegisterWeb(router.WithPrefix(amURL.Path))
	api.Register(router.WithPrefix(path.Join(amURL.Path, "/api")))

	log.Infoln("Listening on", *listenAddress)
	go listen(router, *listenAddress)

	var (
		hup  = make(chan os.Signal)
		term = make(chan os.Signal)
	)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	go func() {
		for range hup {
			reload()
		}
	}()

	<-term

	log.Infoln("Received SIGTERM, exiting gracefully...")
}

func extURL(s, listenAddress string) (*url.URL, error) {
	if s == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listenAddress)
		if err != nil {
			return nil, err
		}

		s = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	ppref := strings.TrimRight(u.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	u.Path = ppref

	return u, nil
}

func listen(router *route.Router, listenAddress string) {
	if err := http.ListenAndServe(listenAddress, router); err != nil {
		log.Fatal(err)
	}
}

type stringset map[string]struct{}

func (ss stringset) Set(value string) error {
	ss[value] = struct{}{}
	return nil
}

func (ss stringset) String() string {
	return strings.Join(ss.slice(), ",")
}

func (ss stringset) slice() []string {
	slice := make([]string, 0, len(ss))
	for k := range ss {
		slice = append(slice, k)
	}
	sort.Strings(slice)
	return slice
}

func mustHardwareAddr() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		panic(err)
	}
	for _, iface := range ifaces {
		if s := iface.HardwareAddr.String(); s != "" {
			return s
		}
	}
	panic("no valid network interfaces")
}

func mustHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return hostname
}
