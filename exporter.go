package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

var (
	errServiceNotRunning = errors.New("service it not running")
)

var elog *log.Logger

// serviceMetrics
const (
	SM_PROCESS_START_JIFFIES int = iota
)

type service struct {
	name string

	pid int
	procStatStartTime int64
}

type SvcCollector struct {
	services map[string]*service

	constMetrics []prometheus.Metric
	serviceMetrics map[int]*prometheus.Desc
}

func newSvcCollector(serviceNames []string) *SvcCollector {
	c := &SvcCollector{
		services: make(map[string]*service),
	}

	c.constMetrics = []prometheus.Metric{
		prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				"service_exporter_start_time",
				"The time at which the service exporter was started",
				nil,
				nil,
			),
			prometheus.GaugeValue,
			float64(time.Now().Unix()),
		),
	}

	c.serviceMetrics = map[int]*prometheus.Desc{
		SM_PROCESS_START_JIFFIES: prometheus.NewDesc(
			"service_process_start_jiffies",
			"The time at which the current process was started, in jiffies; -1 if currently not running.",
			[]string{"service"},
			nil,
		),
	}

	for _, svc := range serviceNames {
		c.services[svc] = &service{
			name: svc,
		}
		c.services[svc].reset()
	}

	return c
}

func (c *SvcCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range c.constMetrics {
		ch <- m.Desc()
	}
	for _, d := range c.serviceMetrics {
		ch <- d
	}
}

func (svc *service) readProcStatData() (procStatData []string, err error) {
	procStatPath := path.Join("/proc", strconv.Itoa(svc.pid), "stat")
	procStatRawData, err := ioutil.ReadFile(procStatPath)
	if err != nil && os.IsNotExist(err) {
		return nil, err
	} else if err != nil {
		elog.Fatalf("could not read process data for pid %d: %s", svc.pid, err)
	}
	procStatData = strings.Split(string(procStatRawData), " ")
	if len(procStatData) < 20 {
		elog.Fatalf("unexpected stat data for pid %d", svc.pid)
	}
	return procStatData, nil
}

func (svc *service) reset() {
	svc.pid = -1
	svc.procStatStartTime = -1
}

func (svc *service) verifyStillRunning() (procStatData []string, stillRunning bool) {
	svc.reset()
	return nil, false
}

func (svc *service) askServiceForPID() (pid int, err error) {
	cmd := exec.Command("service", svc.name, "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("could not query for the status of service %s: %s", svc.name, err)
	}
	parts := strings.Split(string(output), " ")
	if len(parts) < 4 {
		log.Printf("unexpected service status %s", string(output))
		log.Fatalf("could not query for the status of service %s", svc.name)
	}
	status := parts[len(parts) - 3]
	if status != "start/running," {
		return 0, errServiceNotRunning
	}
	pidStr := strings.TrimSpace(parts[len(parts) - 1])
	pid, err = strconv.Atoi(pidStr)
	if err != nil {
		log.Fatalf("could not query for the status of service %s: unexpected PID %s", svc.name, pidStr)
	}
	return pid, nil
}

func (svc *service) findPID() (procStatData []string, err error) {
	svc.pid, err = svc.askServiceForPID()
	if err == errServiceNotRunning {
		svc.reset()
		return nil, errServiceNotRunning
	} else if err != nil {
		panic(err)
	}

	procStatData, err = svc.readProcStatData()
	if err != nil {
		svc.reset()
		return nil, errServiceNotRunning
	}

	// check for race

	recheckPid, err := svc.askServiceForPID()
	if err == errServiceNotRunning {
		svc.reset()
		return nil, errServiceNotRunning
	} else if err != nil {
		panic(err)
	}
	if recheckPid != svc.pid {
		svc.reset()
		return nil, errServiceNotRunning
	}

	svc.procStatStartTime, err = strconv.ParseInt(procStatData[21], 10, 64)
	if err != nil {
		log.Fatalf("garbage start_time for pid %d", procStatData[21], svc.pid)
	}
	return procStatData, nil
}

func (c *SvcCollector) scrape(svc *service) error {
	var procStatData []string
	if svc.pid != -1 {
		var stillRunning bool
		procStatData, stillRunning = svc.verifyStillRunning()
		if !stillRunning {
			procStatData = nil
		}
	}
	if svc.pid == -1 {
		var err error
		procStatData, err = svc.findPID()
		if err != nil {
			return err
		}
	}
	_ = procStatData
	return nil
}

func (c *SvcCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.constMetrics {
		ch <- m
	}

	for _, svc := range c.services {
		_ = c.scrape(svc)
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_START_JIFFIES],
			prometheus.GaugeValue,
			float64(svc.procStatStartTime),
			svc.name,
		)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage:
  %s [--help] LISTEN_PORT SERVICENAME [...]
`, os.Args[0])
}

func main() {
	fls := flag.NewFlagSet("main", flag.ExitOnError)
	fls.Usage = func() { printUsage(os.Stderr) }
	printHelp := fls.Bool("help", false, "prints this help and exits")
	err := fls.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
		os.Exit(1)
	}
	if *printHelp {
		printUsage(os.Stdout)
		os.Exit(0)
	}
	if len(fls.Args()) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}
	listenPort := (fls.Args())[0]
	serviceNames := (fls.Args())[1:]

	elog = log.New(os.Stderr, "", log.LstdFlags)
	elog.Printf("service exporter starting up")

	collector := newSvcCollector(serviceNames)

	registry := prometheus.NewPedanticRegistry()
	err = registry.Register(collector)
	if err != nil {
		elog.Fatalf("ERROR:  %s", err)
	}
	httpHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog: elog,
	})
	http.Handle("/metrics", httpHandler)
	elog.Fatal(http.ListenAndServe(net.JoinHostPort("", listenPort), nil))
}
