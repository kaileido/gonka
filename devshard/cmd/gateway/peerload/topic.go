package peerload

import (
	"strconv"
	"time"
)

type peerTopic interface {
	Name() string
	Version() uint32
	MaxBytes() int
	Interval() time.Duration
	MaxAge() time.Duration
	Build(now time.Time) ([]byte, error)
	Accept(from PeerID, envelope peerEnvelope, now time.Time) error
}

func topicLabel(topic peerTopic) string {
	return topic.Name() + topicSplitter + strconv.FormatUint(uint64(topic.Version()), 10)
}
