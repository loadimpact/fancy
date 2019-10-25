package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const version = "1.3"

func main() {
	fs := flag.NewFlagSet("fancy", flag.ExitOnError)
	var (
		lokiURL    = fs.String("lokiurl", "http://localhost:3100", "Loki Server URL")
		chanSize   = fs.Int("chansize", 10000, "Loki buffered channel capacity")
		batchSize  = fs.Int("batchsize", 100*1024, "Loki will batch these bytes before sending them")
		batchWait  = fs.Int("batchwait", 4, "Loki will send logs after these seconds")
		cmd        = fs.String("cmd", "", "Send input msg to external command and use it's output as new msg")
		metricOnly = fs.Bool("metriconly", false, "Only metrics for Prometheus will be exposed")
		promAddr   = fs.String("promaddr", ":9090", "Prometheus scrape endpoint address")
		promTag    = fs.String("promtag", "", "Will be used as a tag label for the fancy_input_scan_total metric")
	)
	fs.Parse(os.Args[1:])

	t := time.Now()
	defer fmt.Fprintf(os.Stderr, "%v end fancy with flags %s\n", t, os.Args[1:])

	input := &Input{
		promTag:    *promTag,
		metricOnly: *metricOnly,
		cmd:        strings.Fields(*cmd),
	}

	if *metricOnly {
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			err := http.ListenAndServe(*promAddr, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v ERROR: %v\n", t, err)
			}
		}()
	} else {
		input.lineChan = make(chan *LogLine, *chanSize)
		l, err := NewLoki(input.lineChan, *lokiURL, *batchSize, *batchWait)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v ERROR: %v\n", t, err)
		}
		go l.Run()
	}

	fmt.Fprintf(os.Stderr, "%v run fancy v.%s with flags %s\n", time.Now(), version, os.Args[1:])
	input.run(os.Stderr, os.Stdin)
	os.Exit(0)
}

var (
	logScanNumber = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fancy_input_scan_total",
		Help: "Total number of logs received from rsyslog fancy template"},
		[]string{"hostname", "program", "level", "tag"})
	logScanSize = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fancy_input_raw_bytes_total",
		Help: "Total number of bytes received from rsyslog fancy template"},
		[]string{"hostname", "program"})
)

type Input struct {
	lineChan   chan *LogLine
	metricOnly bool
	cmd        []string
	promTag    string
}

func (in *Input) run(stderr io.Writer, stdin io.Reader) {
	t := time.Now()
	s := bufio.NewScanner(stdin)
	for s.Scan() {
		ll, err := scanLine(s.Bytes(), in.metricOnly)
		if err != nil {
			fmt.Fprintf(stderr, "%v ERROR: %v\n", time.Now(), err)
			continue
		}

		if in.metricOnly {
			rawSize := float64(len(ll.Raw))
			logScanNumber.WithLabelValues(ll.Hostname, ll.Program, ll.Severity, in.promTag).Inc()
			logScanSize.WithLabelValues(ll.Hostname, ll.Program).Add(rawSize)
			continue
		}

		if len(in.cmd) > 0 {
			c := exec.Command(in.cmd[0], in.cmd[1:]...)
			c.Stdin = bytes.NewReader(ll.Raw[ll.MsgPos:])
			out, err := c.Output()
			if err != nil {
				fmt.Fprintf(stderr, "%v ERROR: %v\n", time.Now(), err)
				continue
			}
			ll.Msg = string(out)
		}

		select {
		case in.lineChan <- ll:
		default:
			if time.Since(t) > 1e9 {
				fmt.Fprintf(stderr, "%v ERROR: overflowing Loki buffered channel capacity\n", t)
			}
			t = time.Now()
		}
	}
}
