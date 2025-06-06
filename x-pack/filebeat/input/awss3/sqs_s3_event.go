// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package awss3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"go.uber.org/multierr"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
)

const (
	sqsApproximateReceiveCountAttribute = "ApproximateReceiveCount"
	sqsSentTimestampAttribute           = "SentTimestamp"
	sqsInvalidParameterValueErrorCode   = "InvalidParameterValue"
	sqsReceiptHandleIsInvalidErrCode    = "ReceiptHandleIsInvalid"
)

type nonRetryableError struct {
	Err error
}

func (e *nonRetryableError) Unwrap() error {
	return e.Err
}

func (e *nonRetryableError) Error() string {
	return "non-retryable error: " + e.Err.Error()
}

func (e *nonRetryableError) Is(err error) bool {
	_, ok := err.(*nonRetryableError) //nolint:nolintlint,errorlint // This is not used directly to detected wrapped errors (errors.Is handles unwrapping).
	return ok
}

func nonRetryableErrorWrap(err error) error {
	if errors.Is(err, &nonRetryableError{}) {
		return err
	}
	return &nonRetryableError{Err: err}
}

// s3EventsV2 is the notification message that Amazon S3 sends to notify of S3 changes.
// This was derived from the version 2.2 schema.
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-content-structure.html
// If the notification message is sent from SNS to SQS, then Records will be
// replaced by TopicArn and Message fields.
type s3EventsV2 struct {
	TopicArn string      `json:"TopicArn"`
	Message  string      `json:"Message"`
	Records  []s3EventV2 `json:"Records"`
}

// s3EventV2 is a S3 change notification event.
type s3EventV2 struct {
	AWSRegion   string `json:"awsRegion"`
	Provider    string `json:"provider"`
	EventName   string `json:"eventName"`
	EventSource string `json:"eventSource"`
	S3          struct {
		Bucket struct {
			Name string `json:"name"`
			ARN  string `json:"arn"`
		} `json:"bucket"`
		Object struct {
			Key          string    `json:"key"`
			LastModified time.Time `json:"lastModified"`
		} `json:"object"`
	} `json:"s3"`
}

type sqsS3EventProcessor struct {
	s3HandlerFactory     s3ObjectHandlerFactory
	sqsVisibilityTimeout time.Duration
	maxReceiveCount      int
	sqs                  sqsAPI
	log                  *logp.Logger
	warnOnce             sync.Once
	metrics              *inputMetrics
	script               *script
}

func newSQSS3EventProcessor(
	log *logp.Logger,
	metrics *inputMetrics,
	sqs sqsAPI,
	script *script,
	sqsVisibilityTimeout time.Duration,
	maxReceiveCount int,
	s3 s3ObjectHandlerFactory,
) *sqsS3EventProcessor {
	if metrics == nil {
		// Metrics are optional. Initialize a stub.
		metrics = newInputMetrics("", nil, 0)
	}
	return &sqsS3EventProcessor{
		s3HandlerFactory:     s3,
		sqsVisibilityTimeout: sqsVisibilityTimeout,
		maxReceiveCount:      maxReceiveCount,
		sqs:                  sqs,
		log:                  log,
		metrics:              metrics,
		script:               script,
	}
}

type sqsProcessingResult struct {
	processor       *sqsS3EventProcessor
	msg             *types.Message
	receiveCount    int // How many times this SQS object has been read
	eventCount      int // How many events were generated from this SQS object
	keepaliveCancel context.CancelFunc
	processingErr   error

	// Finalizer callbacks for the returned S3 events, invoked via
	// finalizeS3Objects after all events are acknowledged.
	finalizers []finalizerFunc
}

type finalizerFunc func() error

func (p *sqsS3EventProcessor) ProcessSQS(ctx context.Context, msg *types.Message, eventCallback func(beat.Event)) sqsProcessingResult {
	log := p.log.With(
		"message_id", *msg.MessageId,
		"message_receipt_time", time.Now().UTC())

	keepaliveCtx, keepaliveCancel := context.WithCancel(ctx)
	defer keepaliveCancel()

	// Start SQS keepalive worker.
	var keepaliveWg sync.WaitGroup
	keepaliveWg.Add(1)
	go func() {
		defer keepaliveWg.Done()
		p.keepalive(keepaliveCtx, log, msg)
	}()

	receiveCount := getSQSReceiveCount(msg.Attributes)
	if receiveCount == 1 {
		// Only contribute to the sqs_lag_time histogram on the first message
		// to avoid skewing the metric when processing retries.
		if s, found := msg.Attributes[sqsSentTimestampAttribute]; found {
			if sentTimeMillis, err := strconv.ParseInt(s, 10, 64); err == nil {
				sentTime := time.UnixMilli(sentTimeMillis)
				p.metrics.sqsLagTime.Update(time.Since(sentTime).Nanoseconds())
			}
		}
	}

	eventCount := 0
	finalizers, processingErr := p.processS3Events(ctx, log, *msg.Body, func(e beat.Event) {
		eventCount++
		eventCallback(e)
	})

	return sqsProcessingResult{
		msg:             msg,
		processor:       p,
		receiveCount:    receiveCount,
		eventCount:      eventCount,
		keepaliveCancel: keepaliveCancel,
		processingErr:   processingErr,
		finalizers:      finalizers,
	}
}

// Call Done to indicate that all events from this SQS message have been
// acknowledged and it is safe to stop the keepalive routine and
// delete / finalize the message.
func (r sqsProcessingResult) Done() {
	p := r.processor
	processingErr := r.processingErr

	// Stop keepalive routine before changing visibility.
	r.keepaliveCancel()

	// No error. Delete SQS.
	if processingErr == nil {
		if msgDelErr := p.sqs.DeleteMessage(context.Background(), r.msg); msgDelErr != nil {
			p.log.Errorf("failed deleting message from SQS queue (it may be reprocessed): %v", msgDelErr.Error())
			return
		}
		if p.metrics != nil {
			// This nil check always passes in production, but it's nice when unit
			// tests don't have to initialize irrelevant fields
			p.metrics.sqsMessagesDeletedTotal.Inc()
		}
		// SQS message finished and deleted, finalize s3 objects
		if finalizeErr := r.finalizeS3Objects(); finalizeErr != nil {
			p.log.Errorf("failed finalizing message from SQS queue (manual cleanup is required): %v", finalizeErr.Error())
		}
		return
	}

	if p.maxReceiveCount > 0 && r.receiveCount >= p.maxReceiveCount {
		// Prevent poison pill messages from consuming all workers. Check how
		// many times this message has been received before making a disposition.
		processingErr = nonRetryableErrorWrap(fmt.Errorf(
			"sqs ApproximateReceiveCount <%v> exceeds threshold %v: %w",
			r.receiveCount, p.maxReceiveCount, processingErr))
	}

	// An error that reprocessing cannot correct. Delete SQS.
	if errors.Is(processingErr, &nonRetryableError{}) {
		if msgDelErr := p.sqs.DeleteMessage(context.Background(), r.msg); msgDelErr != nil {
			p.log.Errorf("failed processing SQS message (attempted to delete message): %v", processingErr.Error())
			p.log.Errorf("failed deleting message from SQS queue (it may be reprocessed): %v", msgDelErr.Error())
			return
		}
		p.metrics.sqsMessagesDeletedTotal.Inc()
		p.log.Errorf("failed processing SQS message (message was deleted): %w", processingErr)
		return
	}

	// An error that may be resolved by letting the visibility timeout
	// expire thereby putting the message back on SQS. If a dead letter
	// queue is enabled then the message will eventually placed on the DLQ
	// after maximum receives is reached.
	p.metrics.sqsMessagesReturnedTotal.Inc()
	p.log.Errorf("failed processing SQS message (it will return to queue after visibility timeout): %w", processingErr)
}

func (p *sqsS3EventProcessor) keepalive(ctx context.Context, log *logp.Logger, msg *types.Message) {
	t := time.NewTicker(p.sqsVisibilityTimeout / 2)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Debugw("Extending SQS message visibility timeout.",
				"visibility_timeout", p.sqsVisibilityTimeout,
				"expires_at", time.Now().UTC().Add(p.sqsVisibilityTimeout))
			p.metrics.sqsVisibilityTimeoutExtensionsTotal.Inc()

			// Renew visibility.
			if err := p.sqs.ChangeMessageVisibility(ctx, msg, p.sqsVisibilityTimeout); err != nil {
				var apiError smithy.APIError
				if errors.As(err, &apiError) {
					switch apiError.ErrorCode() {
					case sqsReceiptHandleIsInvalidErrCode, sqsInvalidParameterValueErrorCode:
						log.Warnw("Failed to extend message visibility timeout "+
							"because SQS receipt handle is no longer valid. "+
							"Stopping SQS message keepalive routine.", "error", err)
						return
					}
				}
			}
		}
	}
}

func (p *sqsS3EventProcessor) getS3Notifications(body string) ([]s3EventV2, error) {
	// Check if a parsing script is defined. If so, it takes precedence over
	// format autodetection.
	if p.script != nil {
		return p.script.run(body)
	}

	// NOTE: If AWS introduces a V3 schema this will need updated to handle that schema.
	var events s3EventsV2
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&events); err != nil {
		p.log.Debugw("Invalid SQS message body.", "sqs_message_body", body)
		return nil, fmt.Errorf("failed to decode SQS message body as an S3 notification: %w", err)
	}

	// Check if the notification is from S3 -> SNS -> SQS
	if events.TopicArn != "" {
		dec := json.NewDecoder(strings.NewReader(events.Message))
		if err := dec.Decode(&events); err != nil {
			p.log.Debugw("Invalid SQS message body.", "sqs_message_body", body)
			return nil, fmt.Errorf("failed to decode SQS message body as an S3 notification: %w", err)
		}
	}

	if events.Records == nil {
		p.log.Debugw("Invalid SQS message body: missing Records field", "sqs_message_body", body)
		return nil, errors.New("the message is an invalid S3 notification: missing Records field")
	}

	return p.getS3Info(events)
}

func (p *sqsS3EventProcessor) getS3Info(events s3EventsV2) ([]s3EventV2, error) {
	out := make([]s3EventV2, 0, len(events.Records))
	for _, record := range events.Records {
		if !p.isObjectCreatedEvents(record) {
			p.warnOnce.Do(func() {
				p.log.Warnf("Received S3 notification for %q event type, but "+
					"only 'ObjectCreated:*' types are handled. It is recommended "+
					"that you update the S3 Event Notification configuration to "+
					"only include ObjectCreated event types to save resources.",
					record.EventName)
			})
			continue
		}

		// Unescape s3 key name. For example, convert "%3D" back to "=".
		key, err := url.QueryUnescape(record.S3.Object.Key)
		if err != nil {
			return nil, fmt.Errorf("url unescape failed for '%v': %w", record.S3.Object.Key, err)
		}
		record.S3.Object.Key = key

		out = append(out, record)
	}
	return out, nil
}

func (*sqsS3EventProcessor) isObjectCreatedEvents(event s3EventV2) bool {
	return event.EventSource == "aws:s3" && strings.HasPrefix(event.EventName, "ObjectCreated:")
}

func (p *sqsS3EventProcessor) processS3Events(
	ctx context.Context,
	log *logp.Logger,
	body string,
	eventCallback func(beat.Event),
) ([]finalizerFunc, error) {
	s3Events, err := p.getS3Notifications(body)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Messages that are in-flight at shutdown should be returned to SQS.
			return nil, err
		}
		return nil, &nonRetryableError{err}
	}
	log.Debugf("SQS message contained %d S3 event notifications.", len(s3Events))
	defer log.Debug("End processing SQS S3 event notifications.")

	if len(s3Events) == 0 {
		return nil, nil
	}

	var errs []error
	var finalizers []finalizerFunc
	for i, event := range s3Events {
		s3Processor := p.s3HandlerFactory.Create(ctx, event)
		if s3Processor == nil {
			// A nil result generally means that this object key doesn't match the
			// user-configured filters.
			continue
		}

		// Process S3 object (download, parse, create events).
		if err := s3Processor.ProcessS3Object(log, eventCallback); err != nil {
			errs = append(errs, fmt.Errorf(
				"failed processing S3 event for object key %q in bucket %q (object record %d of %d in SQS notification): %w",
				event.S3.Object.Key, event.S3.Bucket.Name, i+1, len(s3Events), err))
		} else {
			finalizers = append(finalizers, s3Processor.FinalizeS3Object)
		}
	}

	return finalizers, multierr.Combine(errs...)
}

func (r sqsProcessingResult) finalizeS3Objects() error {
	var errs []error
	for i, finalize := range r.finalizers {
		if err := finalize(); err != nil {
			errs = append(errs, fmt.Errorf(
				"failed finalizing S3 event (object record %d of %d in SQS notification): %w",
				i+1, len(r.finalizers), err))
		}
	}
	return multierr.Combine(errs...)
}

// getSQSReceiveCount returns the SQS ApproximateReceiveCount attribute. If the value
// cannot be read then -1 is returned.
func getSQSReceiveCount(attributes map[string]string) int {
	if s, found := attributes[sqsApproximateReceiveCountAttribute]; found {
		if receiveCount, err := strconv.Atoi(s); err == nil {
			return receiveCount
		}
	}
	return -1
}
