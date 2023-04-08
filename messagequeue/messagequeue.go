package messagequeue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipfs/go-graphsync"
	gsmsg "github.com/ipfs/go-graphsync/message"
	gsnet "github.com/ipfs/go-graphsync/network"
	"github.com/ipfs/go-graphsync/notifications"
)

var log = logging.Logger("graphsync")

// max block size is the maximum size for batching blocks in a single payload
const maxBlockSize uint64 = 512 * 1024

type Topic uint64

type EventName uint64

const (
	Queued EventName = iota
	Sent
	Error
)

type Metadata struct {
	BlockData     map[graphsync.RequestID][]graphsync.BlockData
	ResponseCodes map[graphsync.RequestID]graphsync.ResponseStatusCode
}

type Event struct {
	Name     EventName
	Err      error
	Metadata Metadata
}

// MessageNetwork is any network that can connect peers and generate a message
// sender.
type MessageNetwork interface {
	NewMessageSender(context.Context, peer.ID, *gsnet.MessageSenderOpts) (gsnet.MessageSender, error)
	ConnectTo(context.Context, peer.ID) error
}

type Allocator interface {
	AllocateBlockMemory(p peer.ID, amount uint64) <-chan error
	ReleasePeerMemory(p peer.ID) error
	ReleaseBlockMemory(p peer.ID, amount uint64) error
}

// MessageQueue implements queue of want messages to send to peers.
type MessageQueue struct {
	p       peer.ID
	network MessageNetwork
	ctx     context.Context

	outgoingWork chan struct{}
	done         chan struct{}
	doneOnce     sync.Once

	// internal do not touch outside go routines
	sender             gsnet.MessageSender
	eventPublisher     notifications.Publisher
	buildersLk         sync.RWMutex
	builders           []*Builder
	nextBuilderTopic   Topic
	allocator          Allocator
	maxRetries         int
	sendMessageTimeout time.Duration
	sendErrorBackoff   time.Duration
}

// New creats a new MessageQueue.
func New(ctx context.Context, p peer.ID, network MessageNetwork, allocator Allocator, maxRetries int, sendMessageTimeout time.Duration, sendErrorBackoff time.Duration) *MessageQueue {
	return &MessageQueue{
		ctx:                ctx,
		network:            network,
		p:                  p,
		outgoingWork:       make(chan struct{}, 1),
		done:               make(chan struct{}),
		eventPublisher:     notifications.NewPublisher(),
		allocator:          allocator,
		maxRetries:         maxRetries,
		sendMessageTimeout: sendMessageTimeout,
		sendErrorBackoff:   sendErrorBackoff,
	}
}

// AllocateAndBuildMessage allows you to work modify the next message that is sent in the queue.
// If blkSize > 0, message building may block until enough memory has been freed from the queues to allocate the message.
func (mq *MessageQueue) AllocateAndBuildMessage(size uint64, buildMessageFn func(*Builder)) {
	if size > 0 {
		select {
		case <-mq.allocator.AllocateBlockMemory(mq.p, size):
		case <-mq.ctx.Done():
			return
		}
	}
	if mq.buildMessage(size, buildMessageFn) {
		mq.signalWork()
	}
}

func (mq *MessageQueue) buildMessage(size uint64, buildMessageFn func(*Builder)) bool {
	mq.buildersLk.Lock()
	defer mq.buildersLk.Unlock()
	if shouldBeginNewResponse(mq.builders, size) {
		topic := mq.nextBuilderTopic
		mq.nextBuilderTopic++
		ctx, _ := otel.Tracer("graphsync").Start(mq.ctx, "message", trace.WithAttributes(
			attribute.Int64("topic", int64(topic)),
		))
		mq.builders = append(mq.builders, NewBuilder(ctx, topic))
	}
	builder := mq.builders[len(mq.builders)-1]
	buildMessageFn(builder)
	return !builder.Empty()
}

func shouldBeginNewResponse(builders []*Builder, blkSize uint64) bool {
	if len(builders) == 0 {
		return true
	}
	if blkSize == 0 {
		return false
	}
	return builders[len(builders)-1].BlockSize()+blkSize > maxBlockSize
}

// Startup starts the processing of messages, and creates an initial message
// based on the given initial wantlist.
func (mq *MessageQueue) Startup() {
	go mq.runQueue()
}

// Shutdown stops the processing of messages for a message queue.
func (mq *MessageQueue) Shutdown() {
	mq.doneOnce.Do(func() {
		close(mq.done)
	})
}

func (mq *MessageQueue) runQueue() {
	defer func() {
		_ = mq.allocator.ReleasePeerMemory(mq.p)
		mq.eventPublisher.Shutdown()
	}()
	mq.eventPublisher.Startup()
	for {
		select {
		case <-mq.outgoingWork:
			mq.sendMessage()
		case <-mq.done:
			select {
			case <-mq.outgoingWork:
				for {
					_, metadata, err := mq.extractOutgoingMessage()
					if err == nil {
						span := trace.SpanFromContext(metadata.ctx)
						err := fmt.Errorf("message queue shutdown")
						span.RecordError(err)
						span.SetStatus(codes.Error, err.Error())
						span.End()
						mq.publishError(metadata, err)
						mq.eventPublisher.Close(metadata.topic)
					} else {
						break
					}
				}
			default:
			}
			if mq.sender != nil {
				mq.sender.Reset()
			}
			return
		case <-mq.ctx.Done():
			if mq.sender != nil {
				_ = mq.sender.Reset()
			}
			return
		}
	}
}

func (mq *MessageQueue) signalWork() {
	select {
	case mq.outgoingWork <- struct{}{}:
	default:
	}
}

var errEmptyMessage = errors.New("empty Message")

func (mq *MessageQueue) extractOutgoingMessage() (gsmsg.GraphSyncMessage, internalMetadata, error) {
	// grab outgoing message
	mq.buildersLk.Lock()
	if len(mq.builders) == 0 {
		mq.buildersLk.Unlock()
		return gsmsg.GraphSyncMessage{}, internalMetadata{}, errEmptyMessage
	}
	builder := mq.builders[0]
	mq.builders = mq.builders[1:]
	// if there are more queued messages, signal we still have more work
	if len(mq.builders) > 0 {
		select {
		case mq.outgoingWork <- struct{}{}:
		default:
		}
	}
	mq.buildersLk.Unlock()
	if builder.Empty() {
		return gsmsg.GraphSyncMessage{}, internalMetadata{}, errEmptyMessage
	}
	return builder.build(mq.eventPublisher)
}

func (mq *MessageQueue) sendMessage() {
	message, metadata, err := mq.extractOutgoingMessage()

	if err != nil {
		if err != errEmptyMessage {
			log.Errorf("Unable to assemble GraphSync message: %s", err.Error())
		}
		return
	}
	span := trace.SpanFromContext(metadata.ctx)
	defer span.End()
	_, sendSpan := otel.Tracer("graphsync").Start(metadata.ctx, "sendMessage", trace.WithAttributes(
		attribute.Int64("topic", int64(metadata.topic)),
		attribute.Int64("size", int64(metadata.msgSize)),
	))
	defer sendSpan.End()
	mq.publishQueued(metadata)
	defer mq.eventPublisher.Close(metadata.topic)

	err = mq.initializeSender()
	if err != nil {
		log.Infof("cant open message sender to peer %s: %s", mq.p, err)
		// TODO: cant connect, what now?
		mq.publishError(metadata, fmt.Errorf("cant open message sender to peer %s: %w", mq.p, err))
		mq.Shutdown()
		return
	}

	if err = mq.sender.SendMsg(mq.ctx, message); err != nil {
		// If the message couldn't be sent, the networking layer will
		// emit a Disconnect event and the MessageQueue will get cleaned up
		log.Infof("Could not send message to peer %s: %s", mq.p, err)
		mq.publishError(metadata, fmt.Errorf("expended retries on SendMsg(%s)", mq.p))
		mq.Shutdown()
		return
	}

	mq.publishSent(metadata)
}

func (mq *MessageQueue) scrubResponseStreams(responseStreams map[graphsync.RequestID]io.Closer) {
	requestIDs := make([]graphsync.RequestID, 0, len(responseStreams))
	for requestID, responseStream := range responseStreams {
		_ = responseStream.Close()
		requestIDs = append(requestIDs, requestID)
	}
	totalFreed := mq.scrubResponses(requestIDs)
	if totalFreed > 0 {
		err := mq.allocator.ReleaseBlockMemory(mq.p, totalFreed)
		if err != nil {
			log.Error(err)
		}
	}
}

// ScrubResponses removes the given response and associated blocks
// from all pending messages in the queue
func (mq *MessageQueue) scrubResponses(requestIDs []graphsync.RequestID) uint64 {
	mq.buildersLk.Lock()
	newBuilders := make([]*Builder, 0, len(mq.builders))
	totalFreed := uint64(0)
	for _, builder := range mq.builders {
		totalFreed = builder.ScrubResponses(requestIDs)
		if !builder.Empty() {
			newBuilders = append(newBuilders, builder)
		}
	}
	mq.builders = newBuilders
	mq.buildersLk.Unlock()
	return totalFreed
}

func (mq *MessageQueue) initializeSender() error {
	if mq.sender != nil {
		return nil
	}
	opts := gsnet.MessageSenderOpts{
		MaxRetries:       mq.maxRetries,
		SendTimeout:      mq.sendMessageTimeout,
		SendErrorBackoff: mq.sendErrorBackoff,
	}

	nsender, err := mq.network.NewMessageSender(mq.ctx, mq.p, &opts)
	if err != nil {
		return err
	}
	mq.sender = nsender
	return nil
}

type internalMetadata struct {
	ctx             context.Context
	public          Metadata
	topic           Topic
	msgSize         uint64
	responseStreams map[graphsync.RequestID]io.Closer
}

func (mq *MessageQueue) publishQueued(metadata internalMetadata) {
	mq.eventPublisher.Publish(metadata.topic, Event{Name: Queued, Metadata: metadata.public})
}

func (mq *MessageQueue) publishSent(metadata internalMetadata) {
	mq.eventPublisher.Publish(metadata.topic, Event{Name: Sent, Metadata: metadata.public})
	_ = mq.allocator.ReleaseBlockMemory(mq.p, metadata.msgSize)
}

func (mq *MessageQueue) publishError(metadata internalMetadata, err error) {
	mq.scrubResponseStreams(metadata.responseStreams)
	mq.eventPublisher.Publish(metadata.topic, Event{Name: Error, Err: err, Metadata: metadata.public})
	_ = mq.allocator.ReleaseBlockMemory(mq.p, metadata.msgSize)
}
