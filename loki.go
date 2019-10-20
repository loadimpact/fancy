package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/golang/snappy"
	"github.com/negbie/fancy/logproto"
	"github.com/prometheus/common/model"
)

const (
	contentType  = "application/x-protobuf"
	postPath     = "/api/prom/push"
	getPath      = "/api/prom/label"
	jobName      = model.LabelValue("fancy")
	maxErrMsgLen = 1024
)

type entry struct {
	labels model.LabelSet
	*logproto.Entry
}

type Loki struct {
	entry
	LokiURL   string
	BatchWait time.Duration
	BatchSize int
}

func (l *Loki) setup() error {
	//l.BatchSize = config.Setting.LokiBulk * 1024
	//l.BatchWait = time.Duration(config.Setting.LokiTimer) * time.Second
	//l.URL = config.Setting.LokiURL

	u, err := url.Parse(l.LokiURL)
	if err != nil {
		return err
	}
	if !strings.Contains(u.Path, postPath) {
		u.Path = postPath
		q := u.Query()
		u.RawQuery = q.Encode()
		l.LokiURL = u.String()
	}
	u.Path = getPath
	q := u.Query()
	u.RawQuery = q.Encode()
	return nil
}

func (l *Loki) start(logLine chan *LogLine) {
	var (
		curPktTime  time.Time
		lastPktTime time.Time
		batch       = map[model.Fingerprint]*logproto.Stream{}
		batchSize   = 0
		maxWait     = time.NewTimer(l.BatchWait)
	)

	defer func() {
		if err := l.sendBatch(batch); err != nil {
			fmt.Fprintf(os.Stderr, "loki flush: %v", err)
		}
	}()

	for {
		select {
		case ll, ok := <-logLine:
			if !ok {
				return
			}
			curPktTime = ll.Timestamp
			// guard against entry out of order errors
			if lastPktTime.After(curPktTime) {
				curPktTime = time.Now()
			}
			lastPktTime = curPktTime

			tsNano := curPktTime.UnixNano()
			ts := &timestamp.Timestamp{
				Seconds: tsNano / int64(time.Second),
				Nanos:   int32(tsNano % int64(time.Second)),
			}

			l.entry = entry{model.LabelSet{}, &logproto.Entry{Timestamp: ts}}
			l.entry.labels["job"] = jobName
			l.entry.labels["level"] = model.LabelValue(ll.Severity)
			l.entry.labels["hostname"] = model.LabelValue(ll.Hostname)
			l.entry.labels["facility"] = model.LabelValue(ll.Facility)
			l.entry.labels["program"] = model.LabelValue(ll.Program)
			l.entry.Entry.Line = ll.Msg

			if batchSize+len(l.entry.Line) > l.BatchSize {
				if err := l.sendBatch(batch); err != nil {
					fmt.Fprintf(os.Stderr, "send size batch: %v", err)
				}
				batchSize = 0
				batch = map[model.Fingerprint]*logproto.Stream{}
				maxWait.Reset(l.BatchWait)
			}

			batchSize += len(l.entry.Line)
			fp := l.entry.labels.FastFingerprint()
			stream, ok := batch[fp]
			if !ok {
				stream = &logproto.Stream{
					Labels: l.entry.labels.String(),
				}
				batch[fp] = stream
			}
			stream.Entries = append(stream.Entries, l.Entry)

		case <-maxWait.C:
			if len(batch) > 0 {
				if err := l.sendBatch(batch); err != nil {
					fmt.Fprintf(os.Stderr, "send time batch: %v", err)
				}
				batchSize = 0
				batch = map[model.Fingerprint]*logproto.Stream{}
			}
			maxWait.Reset(l.BatchWait)
		}
	}
}

func (l *Loki) sendBatch(batch map[model.Fingerprint]*logproto.Stream) error {
	buf, err := encodeBatch(batch)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = l.send(ctx, buf)
	if err != nil {
		return err
	}
	return nil
}

func encodeBatch(batch map[model.Fingerprint]*logproto.Stream) ([]byte, error) {
	req := logproto.PushRequest{
		Streams: make([]*logproto.Stream, 0, len(batch)),
	}
	for _, stream := range batch {
		req.Streams = append(req.Streams, stream)
	}
	buf, err := proto.Marshal(&req)
	if err != nil {
		return nil, err
	}
	buf = snappy.Encode(nil, buf)
	return buf, nil
}

func (l *Loki) send(ctx context.Context, buf []byte) (int, error) {
	req, err := http.NewRequest("POST", l.LokiURL, bytes.NewReader(buf))
	if err != nil {
		return -1, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxErrMsgLen))
		line := ""
		if scanner.Scan() {
			line = scanner.Text()
		}
		err = fmt.Errorf("server returned HTTP status %s (%d): %s", resp.Status, resp.StatusCode, line)
	}
	return resp.StatusCode, err
}
