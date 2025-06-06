// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package awss3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/beats/v7/libbeat/beat"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	sqsTypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"

	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
)

const (
	cloudtrailTestFile            = "testdata/aws-cloudtrail.json.gz"
	totalListingObjects           = 10000
	totalListingObjectsForInputS3 = totalListingObjects / 5
)

type constantSQS struct {
	msgs []sqsTypes.Message
}

var _ sqsAPI = (*constantSQS)(nil)

func newConstantSQS() *constantSQS {
	return &constantSQS{
		msgs: []sqsTypes.Message{
			newSQSMessage(newS3Event(filepath.Base(cloudtrailTestFile))),
		},
	}
}

func (c *constantSQS) ReceiveMessage(ctx context.Context, maxMessages int) ([]sqsTypes.Message, error) {
	return c.msgs, nil
}

func (*constantSQS) DeleteMessage(ctx context.Context, msg *sqsTypes.Message) error {
	return nil
}

func (*constantSQS) ChangeMessageVisibility(ctx context.Context, msg *sqsTypes.Message, timeout time.Duration) error {
	return nil
}

func (c *constantSQS) GetQueueAttributes(ctx context.Context, attr []sqsTypes.QueueAttributeName) (map[string]string, error) {
	return map[string]string{}, nil
}

type s3PagerConstant struct {
	mutex        *sync.Mutex
	objects      []s3Types.Object
	currentIndex int
}

var _ s3Pager = (*s3PagerConstant)(nil)

func (c *s3PagerConstant) HasMorePages() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.currentIndex < len(c.objects)
}

func (c *s3PagerConstant) NextPage(ctx context.Context, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if !c.HasMorePages() {
		return nil, errors.New("no more pages")
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()

	ret := &s3.ListObjectsV2Output{}
	pageSize := 1000
	if len(c.objects) < c.currentIndex+pageSize {
		pageSize = len(c.objects) - c.currentIndex
	}

	ret.Contents = c.objects[c.currentIndex : c.currentIndex+pageSize]
	c.currentIndex = c.currentIndex + pageSize

	return ret, nil
}

func newS3PagerConstant(listPrefix string) *s3PagerConstant {
	lastModified := time.Now()
	ret := &s3PagerConstant{
		mutex:        new(sync.Mutex),
		currentIndex: 0,
	}

	for i := 0; i < totalListingObjectsForInputS3; i++ {
		ret.objects = append(ret.objects, s3Types.Object{
			Key:          aws.String(fmt.Sprintf("%s-%d.json.gz", listPrefix, i)),
			ETag:         aws.String(fmt.Sprintf("etag-%s-%d", listPrefix, i)),
			LastModified: aws.Time(lastModified),
		})
	}

	return ret
}

type constantS3 struct {
	filename      string
	data          []byte
	contentType   string
	pagerConstant s3Pager
}

var _ s3API = (*constantS3)(nil)

func newConstantS3(t testing.TB) *constantS3 {
	data, err := os.ReadFile(cloudtrailTestFile)
	if err != nil {
		t.Fatal(err)
	}

	return &constantS3{
		filename:    filepath.Base(cloudtrailTestFile),
		data:        data,
		contentType: contentTypeJSON,
	}
}

func (c constantS3) GetObject(ctx context.Context, _, bucket, key string) (*s3.GetObjectOutput, error) {
	return newS3GetObjectResponse(c.filename, c.data, c.contentType), nil
}

func (c constantS3) CopyObject(ctx context.Context, _, from_bucket, to_bucket, from_key, to_key string) (*s3.CopyObjectOutput, error) {
	return nil, nil
}

func (c constantS3) DeleteObject(ctx context.Context, _, bucket, key string) (*s3.DeleteObjectOutput, error) {
	return nil, nil
}

func (c constantS3) ListObjectsPaginator(bucket, prefix string) s3Pager {
	return c.pagerConstant
}

var _ beat.Pipeline = (*fakePipeline)(nil)

// fakePipeline returns new ackClients.
type fakePipeline struct {
}

func newFakePipeline() *fakePipeline {
	return &fakePipeline{}
}

func (c *fakePipeline) ConnectWith(config beat.ClientConfig) (beat.Client, error) {
	return &ackClient{
		eventListener: config.EventListener,
	}, nil
}

func (c *fakePipeline) Connect() (beat.Client, error) {
	panic("Connect() is not implemented.")
}

var _ beat.Client = (*ackClient)(nil)

// ackClient is a fake beat.Client that ACKs the published messages.
type ackClient struct {
	eventListener beat.EventListener
}

func (c *ackClient) Close() error { return nil }

func (c *ackClient) Publish(event beat.Event) {
	c.eventListener.AddEvent(event, true)
	go c.eventListener.ACKEvents(1)
}

func (c *ackClient) PublishAll(event []beat.Event) {
	for _, e := range event {
		c.Publish(e)
	}
}

func makeBenchmarkConfig(t testing.TB) config {
	cfg := conf.MustNewConfigFrom(`---
queue_url: foo
file_selectors:
-
  regex: '.json.gz$'
  expand_event_list_from_field: Records
`)

	inputConfig := defaultConfig()
	if err := cfg.Unpack(&inputConfig); err != nil {
		t.Fatal(err)
	}
	return inputConfig
}

func benchmarkInputSQS(t *testing.T, workerCount int) testing.BenchmarkResult {
	return testing.Benchmark(func(b *testing.B) {
		var err error

		config := makeBenchmarkConfig(t)
		config.NumberOfWorkers = workerCount
		sqsReader := newSQSReaderInput(config, aws.Config{})
		sqsReader.log = log.Named("sqs")
		sqsReader.pipeline = newFakePipeline()
		sqsReader.metrics = newInputMetrics("test_id", monitoring.NewRegistry(), workerCount)
		sqsReader.sqs = newConstantSQS()
		require.NoError(t, err)
		sqsReader.s3 = newConstantS3(t)
		sqsReader.msgHandler, err = sqsReader.createEventProcessor()
		require.NoError(t, err, "createEventProcessor must succeed")

		ctx, cancel := context.WithCancel(context.Background())
		b.Cleanup(cancel)

		go func() {
			for sqsReader.metrics.sqsMessagesReceivedTotal.Get() < uint64(b.N) {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()

		b.ResetTimer()
		start := time.Now()
		sqsReader.run(ctx)
		b.StopTimer()
		elapsed := time.Since(start)

		b.ReportMetric(float64(workerCount), "number_of_workers")
		b.ReportMetric(elapsed.Seconds(), "sec")

		b.ReportMetric(float64(sqsReader.metrics.s3EventsCreatedTotal.Get()), "events")
		b.ReportMetric(float64(sqsReader.metrics.s3EventsCreatedTotal.Get())/elapsed.Seconds(), "events_per_sec")

		b.ReportMetric(float64(sqsReader.metrics.s3BytesProcessedTotal.Get()), "s3_bytes")
		b.ReportMetric(float64(sqsReader.metrics.s3BytesProcessedTotal.Get())/elapsed.Seconds(), "s3_bytes_per_sec")

		b.ReportMetric(float64(sqsReader.metrics.sqsMessagesDeletedTotal.Get()), "sqs_messages")
		b.ReportMetric(float64(sqsReader.metrics.sqsMessagesDeletedTotal.Get())/elapsed.Seconds(), "sqs_messages_per_sec")
	})
}

func TestBenchmarkInputSQS(t *testing.T) {
	logp.TestingSetup(logp.WithLevel(logp.InfoLevel))

	results := []testing.BenchmarkResult{
		benchmarkInputSQS(t, 1),
		benchmarkInputSQS(t, 2),
		benchmarkInputSQS(t, 4),
		benchmarkInputSQS(t, 8),
		benchmarkInputSQS(t, 16),
		benchmarkInputSQS(t, 32),
		benchmarkInputSQS(t, 64),
		benchmarkInputSQS(t, 128),
		benchmarkInputSQS(t, 256),
		benchmarkInputSQS(t, 512),
		benchmarkInputSQS(t, 1024),
	}

	headers := []string{
		"Max Msgs Inflight",
		"Events per sec",
		"S3 Bytes per sec",
		"Time (sec)",
		"CPUs",
	}
	data := make([][]string, 0)
	for _, r := range results {
		data = append(data, []string{
			fmt.Sprintf("%v", r.Extra["max_messages_inflight"]),
			fmt.Sprintf("%v", r.Extra["events_per_sec"]),
			fmt.Sprintf("%v", humanize.Bytes(uint64(r.Extra["s3_bytes_per_sec"]))),
			fmt.Sprintf("%v", r.Extra["sec"]),
			fmt.Sprintf("%v", runtime.GOMAXPROCS(0)),
		})
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(headers)
	table.AppendBulk(data)
	table.Render()
}

func benchmarkInputS3(t *testing.T, numberOfWorkers int) testing.BenchmarkResult {
	return testing.Benchmark(func(b *testing.B) {
		log := logp.NewLogger(inputName)
		log.Infof("benchmark with %d number of workers", numberOfWorkers)

		metricRegistry := monitoring.NewRegistry()
		metrics := newInputMetrics("test_id", metricRegistry, numberOfWorkers)
		pipeline := newFakePipeline()

		config := makeBenchmarkConfig(t)
		config.NumberOfWorkers = numberOfWorkers

		b.ResetTimer()
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		b.Cleanup(cancel)

		go func() {
			for metrics.s3ObjectsAckedTotal.Get() < totalListingObjects {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()

		errChan := make(chan error)
		wg := new(sync.WaitGroup)
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(i int, wg *sync.WaitGroup) {
				defer wg.Done()
				curConfig := config
				curConfig.BucketListPrefix = fmt.Sprintf("list_prefix_%d", i)
				s3API := newConstantS3(t)
				s3API.pagerConstant = newS3PagerConstant(curConfig.BucketListPrefix)
				store := openTestStatestore()

				states, err := newStates(nil, store, "")
				assert.NoError(t, err, "states creation should succeed")

				s3EventHandlerFactory := newS3ObjectProcessorFactory(metrics, s3API, config.FileSelectors, backupConfig{})
				s3Poller := &s3PollerInput{
					log:             logp.NewLogger(inputName),
					config:          config,
					metrics:         metrics,
					s3:              s3API,
					pipeline:        pipeline,
					s3ObjectHandler: s3EventHandlerFactory,
					states:          states,
					provider:        "provider",
					filterProvider:  newFilterProvider(&config),
				}

				s3Poller.run(ctx)
			}(i, wg)
		}

		wg.Wait()
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatal(err)
			}
		default:

		}

		b.StopTimer()
		elapsed := time.Since(start)

		b.ReportMetric(float64(numberOfWorkers), "number_of_workers")
		b.ReportMetric(elapsed.Seconds(), "sec")

		b.ReportMetric(float64(metrics.s3EventsCreatedTotal.Get()), "events")
		b.ReportMetric(float64(metrics.s3EventsCreatedTotal.Get())/elapsed.Seconds(), "events_per_sec")

		b.ReportMetric(float64(metrics.s3BytesProcessedTotal.Get()), "s3_bytes")
		b.ReportMetric(float64(metrics.s3BytesProcessedTotal.Get())/elapsed.Seconds(), "s3_bytes_per_sec")

		b.ReportMetric(float64(metrics.s3ObjectsListedTotal.Get()), "objects_listed")
		b.ReportMetric(float64(metrics.s3ObjectsListedTotal.Get())/elapsed.Seconds(), "objects_listed_per_sec")

		b.ReportMetric(float64(metrics.s3ObjectsProcessedTotal.Get()), "objects_processed")
		b.ReportMetric(float64(metrics.s3ObjectsProcessedTotal.Get())/elapsed.Seconds(), "objects_processed_per_sec")

		b.ReportMetric(float64(metrics.s3ObjectsAckedTotal.Get()), "objects_acked")
		b.ReportMetric(float64(metrics.s3ObjectsAckedTotal.Get())/elapsed.Seconds(), "objects_acked_per_sec")
	})
}

func TestBenchmarkInputS3(t *testing.T) {
	logp.TestingSetup(logp.WithLevel(logp.InfoLevel))

	results := []testing.BenchmarkResult{
		benchmarkInputS3(t, 1),
		benchmarkInputS3(t, 2),
		benchmarkInputS3(t, 4),
		benchmarkInputS3(t, 8),
		benchmarkInputS3(t, 16),
		benchmarkInputS3(t, 32),
		benchmarkInputS3(t, 64),
		benchmarkInputS3(t, 128),
		benchmarkInputS3(t, 256),
		benchmarkInputS3(t, 512),
		benchmarkInputS3(t, 1024),
	}

	headers := []string{
		"Number of workers",
		"Objects listed total",
		"Objects listed per sec",
		"Objects processed total",
		"Objects processed per sec",
		"Objects acked total",
		"Objects acked per sec",
		"Events total",
		"Events per sec",
		"S3 Bytes total",
		"S3 Bytes per sec",
		"Time (sec)",
		"CPUs",
	}
	data := make([][]string, 0)
	for _, r := range results {
		data = append(data, []string{
			fmt.Sprintf("%v", r.Extra["number_of_workers"]),
			fmt.Sprintf("%v", r.Extra["objects_listed"]),
			fmt.Sprintf("%v", r.Extra["objects_listed_per_sec"]),
			fmt.Sprintf("%v", r.Extra["objects_processed"]),
			fmt.Sprintf("%v", r.Extra["objects_processed_per_sec"]),
			fmt.Sprintf("%v", r.Extra["objects_acked"]),
			fmt.Sprintf("%v", r.Extra["objects_acked_per_sec"]),
			fmt.Sprintf("%v", r.Extra["events"]),
			fmt.Sprintf("%v", r.Extra["events_per_sec"]),
			fmt.Sprintf("%v", humanize.Bytes(uint64(r.Extra["s3_bytes"]))),
			fmt.Sprintf("%v", humanize.Bytes(uint64(r.Extra["s3_bytes_per_sec"]))),
			fmt.Sprintf("%v", r.Extra["sec"]),
			fmt.Sprintf("%v", runtime.GOMAXPROCS(0)),
		})
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(headers)
	table.AppendBulk(data)
	table.Render()
}
