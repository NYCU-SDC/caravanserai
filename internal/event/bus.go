// Package event provides a lightweight in-process event bus.
//
// Design:
//
//	The Bus decouples producers (e.g. the store layer) from consumers (e.g.
//	controllers) without either side knowing about the other.  Dependency
//	direction is:
//
//	    store  →  Bus  ←  controller
//
//	Events are identified by a Topic string.  Subscribers receive a copy of
//	every event published to their topic via a buffered channel.  If the
//	subscriber's channel is full the event is dropped and a warning is logged
//	— this matches the Controller Manager's own queue-full behaviour and
//	avoids blocking the publisher (e.g. an HTTP handler) on a slow consumer.
//
//	The Bus is safe for concurrent use.
package event

import (
	"sync"

	"go.uber.org/zap"
)

// Topic identifies the kind of event being published.
type Topic string

const (
	// TopicProjectCreated is published whenever a new Project is persisted.
	// The payload is the project name (string).
	TopicProjectCreated Topic = "project.created"

	// TopicProjectUpdated is published whenever a Project's status is updated.
	// The payload is the project name (string).
	TopicProjectUpdated Topic = "project.updated"

	// TopicNodeCreated is published whenever a new Node is persisted.
	// The payload is the node name (string).
	TopicNodeCreated Topic = "node.created"

	// TopicNodeUpdated is published whenever a Node's status is updated.
	// The payload is the node name (string).
	TopicNodeUpdated Topic = "node.updated"
)

// Event carries a topic and the name of the resource that changed.
type Event struct {
	Topic Topic
	Name  string
}

// Handler is a channel that receives events for a subscribed topic.
// Consumers read from this channel in their own goroutine.
type Handler chan Event

// Bus is the in-process event bus.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[Topic][]Handler
	logger      *zap.Logger
	bufSize     int
}

// New creates a Bus.  bufSize is the capacity of each subscriber channel;
// 256 is a reasonable default that matches the Controller Manager's queue size.
func New(logger *zap.Logger, bufSize int) *Bus {
	return &Bus{
		subscribers: make(map[Topic][]Handler),
		logger:      logger,
		bufSize:     bufSize,
	}
}

// Subscribe returns a channel that will receive all future events for topic.
// The caller must read from the channel promptly; if the buffer fills up,
// events are dropped.
func (b *Bus) Subscribe(topic Topic) Handler {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(Handler, b.bufSize)
	b.subscribers[topic] = append(b.subscribers[topic], ch)
	return ch
}

// Publish sends an event to all subscribers of topic.
// It never blocks: if a subscriber's buffer is full the event is dropped.
func (b *Bus) Publish(topic Topic, name string) {
	b.mu.RLock()
	subs := b.subscribers[topic]
	b.mu.RUnlock()

	e := Event{Topic: topic, Name: name}
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			b.logger.Warn("Event bus: subscriber channel full, event dropped",
				zap.String("topic", string(topic)),
				zap.String("name", name),
			)
		}
	}
}
