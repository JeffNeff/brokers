// Copyright 2022 TriggerMesh Inc.
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"fmt"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/triggermesh/brokers/pkg/backend"
	"go.uber.org/zap"

	goredis "github.com/go-redis/redis/v9"
)

type subscription struct {
	instance string
	stream   string
	name     string
	group    string

	// caller's callback for dispatching events from Redis.
	ccbDispatch backend.ConsumerDispatcher

	// cancel function let us control when the subscription loop should exit.
	ctx    context.Context
	cancel context.CancelFunc
	// stoppedCh signals when a subscription has completely finished.
	stoppedCh chan struct{}

	client *goredis.Client
	logger *zap.SugaredLogger
}

func (s *subscription) start() {
	// Start reading all pending messages
	id := "0"

	// When the context is signaled mark an exitLoop flag to exit
	// the worker routine gracefuly.
	exitLoop := false
	go func() {
		<-s.ctx.Done()
		exitLoop = true
	}()

	go func() {
		for {
			// Check at the begining of each iteration if the exit loop flag has
			// been signaled due to done context.
			if exitLoop {

				break
			}

			// Although this call is blocking it will yield when the context is done,
			// the exit loop flag above will be triggered almost immediately if no
			// data has been read.
			streams, err := s.client.XReadGroup(s.ctx, &goredis.XReadGroupArgs{
				Group:    s.group,
				Consumer: s.instance,
				Streams:  []string{s.stream, id},
				Count:    1,
				Block:    time.Hour,
				NoAck:    false,
			}).Result()

			if err != nil {
				// Ignore errors when the blocking period ends without
				// receiving any event, and errors when the context is
				// canceled
				if !strings.HasSuffix(err.Error(), "i/o timeout") &&
					err.Error() != "context canceled" {
					s.logger.Error("Error reading CloudEvents from consumer group", zap.String("groups", s.group), zap.Error(err))
				}
				continue
			}

			if len(streams) != 1 {
				s.logger.Error("unexpected number of streams read", zap.Any("streams", streams))
				continue
			}

			// If we are processing pending messages from Redis and we reach
			// EOF, switch to reading new messages.
			if len(streams[0].Messages) == 0 && id != ">" {
				id = ">"
			}

			for _, msg := range streams[0].Messages {
				ce := &cloudevents.Event{}
				for k, v := range msg.Values {
					if k != ceKey {
						s.logger.Debug(fmt.Sprintf("Ignoring non expected key at message from backend: %s", k))
						continue
					}

					if err = ce.UnmarshalJSON([]byte(v.(string))); err != nil {
						s.logger.Error("Could not unmarshal CloudEvent from Redis", zap.Error(err))
						continue
					}
				}

				// If there was no valid CE in the message ACK so that we do not receive it again.
				if ce.ID() == "" {
					s.logger.Warn(fmt.Sprintf("Removing non valid message from backend: %s", msg.ID))
					s.ack(msg.ID)
					continue
				}

				ce.Context.SetExtension("tmbackendid", msg.ID)

				go func() {
					s.ccbDispatch(ce)
					id := ce.Extensions()["tmbackendid"].(string)

					if err := s.ack(id); err != nil {
						s.logger.Error(fmt.Sprintf("could not ACK the Redis message %s containing CloudEvent %s", id, ce.Context.GetID()),
							zap.Error(err))
					}
				}()

				// If we are processing pending messages the ACK might take a
				// while to be sent. We need to set the message ID so that the
				// next requested element is not any of the pending being processed.
				if id != ">" {
					id = msg.ID
				}
			}
		}

		// Close stoppedCh to singal external viewers that processing for this
		// subscription is no longer running.
		close(s.stoppedCh)
	}()
}

func (s *subscription) ack(id string) error {
	res := s.client.XAck(s.ctx, s.stream, s.group, id)
	_, err := res.Result()
	return err
}