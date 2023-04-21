package pubsub

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"encore.dev/appruntime/exported/config"
	"encore.dev/appruntime/exported/model"
	"encore.dev/beta/errs"
	"encore.dev/internal/limiter"
	"encore.dev/pubsub/internal/test"
	"encore.dev/pubsub/internal/types"
	"encore.dev/pubsub/internal/utils"
)

// Topic presents a flow of events of type T from any number of publishers to
// any number of subscribers.
//
// Each subscription will receive a copy of each message published to the topic.
//
// See NewTopic for more information on how to declare a Topic.
type Topic[T any] struct {
	cfg            TopicConfig
	mgr            *Manager
	topicCfg       *config.PubsubTopic
	topic          types.TopicImplementation
	publishLimiter limiter.Limiter
}

func newTopic[T any](mgr *Manager, name string, cfg TopicConfig) *Topic[T] {
	if mgr.static.Testing {
		return &Topic[T]{
			cfg:            cfg,
			mgr:            mgr,
			topicCfg:       &config.PubsubTopic{EncoreName: name},
			topic:          test.NewTopic[T](mgr.ts, name),
			publishLimiter: limiter.New(nil), // Create a no-op limiter
		}
	}

	// Look up the topic configuration
	topic, ok := mgr.runtime.PubsubTopics[name]
	if !ok {
		mgr.rootLogger.Fatal().Msgf("unregistered/unknown topic: %v", name)
	}

	// Look up the server config
	provider := mgr.runtime.PubsubProviders[topic.ProviderID]

	tried := make([]string, 0, len(mgr.providers))
	for _, p := range mgr.providers {
		if p.Matches(provider) {
			impl := p.NewTopic(provider, topic)
			return &Topic[T]{
				mgr:            mgr,
				topicCfg:       topic,
				topic:          impl,
				publishLimiter: limiter.New(topic.Limiter),
			}
		}
		tried = append(tried, p.ProviderName())
	}

	mgr.rootLogger.Fatal().Msgf("unsupported PubSub provider for server[%d], tried: %v",
		topic.ProviderID, tried)
	panic("unreachable")
}

// TopicMeta contains metadata about a topic.
// The fields should not be modified by the caller.
// Additional fields may be added in the future.
type TopicMeta struct {
	// Name is the name of the topic, as provided in the constructor to NewTopic.
	Name string
	// Config is the topic's configuration.
	Config TopicConfig
}

// Meta returns metadata about the topic.
func (t *Topic[T]) Meta() TopicMeta {
	return TopicMeta{
		Name:   t.topicCfg.EncoreName,
		Config: t.cfg,
	}
}

// Publish will publish a message to the topic and returns a unique message ID for the message.
//
// This function will not return until the message has been successfully accepted by the topic.
//
// If an error is returned, it is probable that the message failed to be published, however it is possible
// that the message could still be received by subscriptions to the topic.
func (t *Topic[T]) Publish(ctx context.Context, msg T) (id string, err error) {
	if t.topicCfg == nil || t.topic == nil {
		return "", errs.B().Code(errs.Unimplemented).Msg("pubsub topic was not created using pubsub.NewTopic").Err()
	}

	// Extract the message attributes
	attrs, err := utils.MarshalFields(msg, utils.AttrTag)
	if err != nil {
		return "", errs.B().Cause(err).Code(errs.InvalidArgument).Msgf("failed to extract message attributes for topic %s", t.topicCfg.EncoreName).Err()
	}

	// Marshal the message to JSON
	data, err := json.Marshal(msg)
	if err != nil {
		return "", errs.B().Cause(err).Code(errs.InvalidArgument).Msgf("failed to marshal message to JSON for topic %s", t.topicCfg.EncoreName).Err()
	}

	// Add the correlation ID to the attributes
	if req := t.mgr.rt.Current().Req; req != nil {
		// Pass our trace ID through, so the subscribers can mark their traces as children of this trace
		if req.TraceID != (model.TraceID{}) {
			attrs[parentTraceIDAttribute] = req.TraceID.String()
		}

		if req.ExtCorrelationID != "" {
			// If we have a correlation ID from the request, use that
			attrs[extCorrelationIDAttribute] = req.ExtCorrelationID
		} else if req.TraceID != (model.TraceID{}) {
			// Otherwise this is the first request in the event chain, so this trace ID becomes the correlation ID
			attrs[extCorrelationIDAttribute] = req.TraceID.String()
		}
	}

	// Start the trace span
	publishTraceID := atomic.AddUint64(&t.mgr.publishCounter, 1)
	curr := t.mgr.rt.Current()
	if curr.Req != nil && curr.Trace != nil {
		curr.Trace.PublishStart(t.topicCfg.EncoreName, data, curr.Req.SpanID, curr.Goctr, publishTraceID, 2)
	}

	// Publish once the rate limiter allows it
	if err = t.publishLimiter.Wait(ctx); err == nil {
		// Publish to the clouds topic
		id, err = t.topic.PublishMessage(ctx, attrs, data)
	}

	// End the trace span
	if curr.Req != nil && curr.Trace != nil {
		curr.Trace.PublishEnd(publishTraceID, id, err)
	}

	if err != nil {
		return "", errs.B().Cause(err).Code(errs.Unavailable).Msgf("failed to publish message to %s", t.topicCfg.EncoreName).Err()
	}

	return id, nil
}
