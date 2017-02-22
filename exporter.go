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

var _SC_CLK_TCK int

var elog *log.Logger

// serviceMetrics
const (
	SM_PROCESS_START int = iota
	SM_PROCESS_CPU_SELF_TIME
	SM_PROCESS_CPU_TIME
	SM_PROCESS_VSIZE
	SM_PROCESS_RSS
	SM_PROCESS_UPTIME_SECONDS
)

const (
	PROC_PID_STAT_STARTTIME int = 21
	PROC_PID_STAT_UTIME = 15
	PROC_PID_STAT_STIME = 16
	PROC_PID_STAT_CUTIME = 17
	PROC_PID_STAT_CSTIME = 18
	PROC_PID_STAT_VSIZE = 22
	PROC_PID_STAT_RSS = 23
)

type service struct {
	name string

	// Constant as long as the service is up
	pid int
	procStatStartTime int64

	// Re-populated on each scrape
	procStatCPUSelfTime int64
	procStatCPUTime int64
	procStatVSize int64
	procStatRSS int64
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
		SM_PROCESS_START: prometheus.NewDesc(
			"service_process_start",
			"The time at which the current process was started; -1 if currently not running.",
			[]string{"service"},
			nil,
		),
		SM_PROCESS_CPU_SELF_TIME: prometheus.NewDesc(
			"service_cpu_self_time_total",
			"The amount of CPU time used by this process, excluding children, measured in clock ticks.",
			[]string{"service"},
			nil,
		),
		SM_PROCESS_CPU_TIME: prometheus.NewDesc(
			"service_cpu_time_total",
			"The amount of CPU time used by this process and its waited-for children, measured in clock ticks.",
			[]string{"service"},
			nil,
		),
		SM_PROCESS_VSIZE: prometheus.NewDesc(
			"service_current_vsize",
			"The virtual memory size of the process, in bytes; 0 if currently not running.",
			[]string{"service"},
			nil,
		),
		SM_PROCESS_RSS: prometheus.NewDesc(
			"service_current_rss",
			"The Resident Set Size of the process; 0 if currently not running.",
			[]string{"service"},
			nil,
		),
		SM_PROCESS_UPTIME_SECONDS: prometheus.NewDesc(
			"service_process_uptime_seconds",
			"The uptime of the process in seconds; -1 if currently not running.",
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

func (c *SvcCollector) readProcUptimeData() []string {
	procUptimeRawData, err := ioutil.ReadFile("/proc/uptime")
	if err != nil {
		elog.Fatalf("could not read /proc/uptime: %s", err)
	}
	procUptimeData := strings.Split(string(procUptimeRawData), " ")
	if len(procUptimeData) < 2 {
		elog.Fatalf("unexpected /proc/uptime data")
	}
	return procUptimeData
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
	if len(procStatData) < 25 {
		elog.Fatalf("unexpected stat data for pid %d", svc.pid)
	}
	return procStatData, nil
}

func (svc *service) reset() {
	svc.pid = -1
	svc.procStatStartTime = -1

	svc.procStatCPUSelfTime = 0
	svc.procStatCPUTime = 0
	svc.procStatVSize = 0
	svc.procStatRSS = 0
}

// Verifies that a process is still running.  The returned procStatData is only
// valid if stillRunning is true.  Calls reset() if the process is not running
// anymore.
func (svc *service) verifyStillRunning() (procStatData []string, stillRunning bool) {
	procStatData, err := svc.readProcStatData()
	if err != nil {
		svc.reset()
		return nil, false
	}
	currentProcStartTime, err := strconv.ParseInt(procStatData[PROC_PID_STAT_STARTTIME], 10, 64)
	if err != nil {
		log.Fatalf("garbage start_time for pid %d", svc.pid)
	}
	if currentProcStartTime != svc.procStatStartTime {
		svc.reset()
		return nil, false
	}
	return procStatData, true
}

func (svc *service) askServiceForPID() (pid int, err error) {
	cmd := exec.Command("service", svc.name, "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		errStr := err.Error()
		if output != nil {
			log.Printf("command 'service %s status' failed: %s", svc.name, err)
			errStr = (strings.SplitN(string(output), "\n", 2))[0]
		}
		log.Printf("could not query for the status of service %s: %s", svc.name, errStr)
		os.Exit(1)
	}
	commaSeparated := strings.Split(string(output), ",")
	parts := strings.Split(commaSeparated[0], " ")
	if len(parts) < 2 {
		log.Printf("unexpected service status %s", string(output))
		log.Fatalf("could not query for the status of service %s", svc.name)
	}
	status := parts[len(parts) - 1]
	if status != "start/running" {
		return 0, errServiceNotRunning
	}
	if len(commaSeparated) != 2 {
		log.Printf("unexpected service status %s", string(output))
		log.Fatalf("could not query for the status of service %s", svc.name)
	}
	parts = strings.Split(commaSeparated[1], " ")
	pidStr := strings.TrimSpace(parts[len(parts) - 1])
	pid, err = strconv.Atoi(pidStr)
	if err != nil {
		log.Fatalf("could not query for the status of service %s: unexpected PID %s", svc.name, pidStr)
	}
	return pid, nil
}

// Tries to figure out the Linux process ID (PID) for the service.  The only
// error currently returned by this function is errServiceNotRunning; any error
// while attempting to figure out the PID will be fatal.  The returned
// procStatData is only valid if err is nil.
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
		log.Printf("service %s (pid %d) has died", svc.name, svc.pid)
		svc.reset()
		return nil, errServiceNotRunning
	}

	// Now that we have read the stat data, ask for the service's PID again to
	// guard against the possibility that the service died and another process
	// took its place with the same PID.  If the PID still matches we can quite
	// safely assume that we just read the data for the correct process.
	//
	// (There's still a window where we read the stat file for an unrelated
	// process, which then died before the service was restarted -- but that
	// will be detected on the next scrape, since the start time will have
	// changed from what we read on this scrape.)

	recheckPid, err := svc.askServiceForPID()
	if err == errServiceNotRunning {
		log.Printf("service %s (pid %d) has died", svc.name, svc.pid)
		svc.reset()
		return nil, errServiceNotRunning
	} else if err != nil {
		panic(err)
	}
	if recheckPid != svc.pid {
		log.Printf("service %s (pid %d) has died", svc.name, svc.pid)
		svc.reset()
		return nil, errServiceNotRunning
	}

	svc.procStatStartTime, err = strconv.ParseInt(procStatData[PROC_PID_STAT_STARTTIME], 10, 64)
	if err != nil {
		log.Fatalf("garbage start_time for pid %d", svc.pid)
	}
	return procStatData, nil
}

func (c *SvcCollector) scrape(svc *service) error {
	var procStatData []string
	if svc.pid != -1 {
		var stillRunning bool
		oldPid := svc.pid
		procStatData, stillRunning = svc.verifyStillRunning()
		if !stillRunning {
			log.Printf("service %s (pid %d) has died", svc.name, oldPid)
			procStatData = nil
		}
	}
	if svc.pid == -1 {
		var err error
		procStatData, err = svc.findPID()
		if err != nil {
			return err
		}
		log.Printf("service %s running, pid %d", svc.name, svc.pid)
	}
	readInt64 := func(idx int) int64 {
		val, err := strconv.ParseInt(procStatData[idx], 10, 64)
		if err != nil {
			log.Fatalf("garbage data at column index %d for pid %d", idx + 1, svc.pid)
		}
		return val
	}
	svc.procStatCPUSelfTime = readInt64(PROC_PID_STAT_UTIME) + readInt64(PROC_PID_STAT_STIME)
	svc.procStatCPUTime = svc.procStatCPUSelfTime + readInt64(PROC_PID_STAT_CUTIME) + readInt64(PROC_PID_STAT_CSTIME)
	svc.procStatVSize = readInt64(PROC_PID_STAT_VSIZE)
	svc.procStatRSS = readInt64(PROC_PID_STAT_RSS)
	return nil
}

func (c *SvcCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.constMetrics {
		ch <- m
	}

	for _, svc := range c.services {
		_ = c.scrape(svc)
	}
	procUptimeData := c.readProcUptimeData()
	systemUptimeInSeconds, err := strconv.ParseFloat(procUptimeData[0], 64)
	if err != nil {
		log.Fatalf("unexpected /proc/uptime data %s", procUptimeData[0])
	}
	systemUptimeInTicks := int64(systemUptimeInSeconds * float64(_SC_CLK_TCK))
	for _, svc := range c.services {
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_START],
			prometheus.GaugeValue,
			float64(svc.procStatStartTime),
			svc.name,
		)
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_CPU_SELF_TIME],
			prometheus.CounterValue,
			float64(svc.procStatCPUSelfTime),
			svc.name,
		)
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_CPU_TIME],
			prometheus.CounterValue,
			float64(svc.procStatCPUTime),
			svc.name,
		)
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_VSIZE],
			prometheus.GaugeValue,
			float64(svc.procStatVSize),
			svc.name,
		)
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_RSS],
			prometheus.GaugeValue,
			float64(svc.procStatRSS),
			svc.name,
		)
		var serviceUptimeSeconds float64
		if svc.pid == -1 {
			serviceUptimeSeconds = -1
		} else {
			serviceUptimeSeconds = float64(systemUptimeInTicks - svc.procStatStartTime) * float64(_SC_CLK_TCK)
		}
		ch <- prometheus.MustNewConstMetric(
			c.serviceMetrics[SM_PROCESS_UPTIME_SECONDS],
			prometheus.GaugeValue,
			float64(serviceUptimeSeconds),
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

	cmd := exec.Command("getconf", "CLK_TCK")
	sysconfOutput, err := cmd.CombinedOutput()
	if err != nil {
		errStr := err.Error()
		if sysconfOutput != nil {
			log.Printf("command 'getconf CLK_TCK' failed: %s", err)
			errStr = (strings.SplitN(string(sysconfOutput), "\n", 2))[0]
		}
		elog.Printf("could not query CLK_TCK from getconf: %s", errStr)
		os.Exit(1)
	}
	_SC_CLK_TCK, err = strconv.Atoi(strings.TrimSpace(string(sysconfOutput)))
	if err != nil {
		elog.Fatalf("could not query CLK_TCK from getconf: %s", err)
	}

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
