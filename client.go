package pulsar

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	pb "github.com/cornelk/pulsar-go/proto"
	"github.com/golang/protobuf/proto"
)

// client constants
const (
	libraryVersion  = "0.01" // TODO use git version tag
	protocolVersion = int32(pb.ProtocolVersion_v15)
)

// Client implements a Pulsar client.
type Client struct {
	log  Logger
	host string
	cmds commands

	cancel context.CancelFunc
	ctx    context.Context // passed to consumers/producers

	conn      *conn
	connMutex sync.RWMutex // protects conn init/close access

	req *requests

	consumers *consumerRegistry
	producers *producerRegistry

	connected chan struct{}
	stopped   chan struct{}
}

// NewClient creates a new Pulsar client.
func NewClient(serverURL string, opts ...ClientOption) (*Client, error) {
	conf := applyOptions(opts)

	if !strings.Contains(serverURL, "://") {
		serverURL = "pulsar://" + serverURL
	}

	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}

	if u.Port() == "" {
		// Use default port.
		u.Host += ":6650"
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		log:  conf.Logger,
		host: u.Host,

		cancel: cancel,
		ctx:    ctx,

		req: newRequests(),

		consumers: newConsumerRegistry(),
		producers: newProducerRegistry(),

		connected: make(chan struct{}, 1),
		stopped:   make(chan struct{}, 1),
	}

	if c.log == nil || (reflect.ValueOf(c.log).Kind() == reflect.Ptr && reflect.ValueOf(c.log).IsNil()) {
		c.log = newLogger()
	}
	c.cmds = c.newCommandMap()

	return c, nil
}

// Dial connects to the Pulsar server.
// This needs to be called before a Consumer or Producer can be created.
func (c *Client) Dial(ctx context.Context) error {
	d := newDialer(c.log, c.host)
	conn, err := d.connect(ctx)
	if err != nil {
		c.log.Printf("Dialing failed: %w", err)
		return err
	}

	c.connMutex.Lock()
	c.conn = conn
	c.connMutex.Unlock()

	if err = sendConnectCommand(conn); err != nil {
		return err
	}

	go c.readCommands()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.connected:
		return nil
	}
}

// NewProducer creates a new Producer, returning after the connection
// has been made.
func (c *Client) NewProducer(ctx context.Context, config ProducerConfig) (*Producer, error) {
	// TODO check connected state

	b := c.newBrokerConnection()

	id := c.producers.newID()
	prod, err := newProducer(c, b, config, id)
	if err != nil {
		return nil, err
	}

	c.producers.add(id, prod)
	c.topicLookup(prod.topic.completeTopicName, prod.topicReady)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-prod.connected:
		return prod, nil
	}
}

func (c *Client) createNewConsumer(config ConsumerConfig) (*consumer, error) {
	b := c.newBrokerConnection()

	id := c.consumers.newID()
	cons, err := newConsumer(c, b, config, id)
	if err != nil {
		return nil, err
	}

	c.consumers.add(id, cons)
	return cons, nil
}

// NewConsumer creates a new Consumer, returning after the connection
// has been made.
func (c *Client) NewConsumer(ctx context.Context, config ConsumerConfig) (Consumer, error) {
	// TODO check connected state

	if config.TopicPattern != "" {
		if config.TopicPatternDiscoveryInterval <= 0 {
			config.TopicPatternDiscoveryInterval = 30000
		}

		b := c.newBrokerConnection()
		multi, err := newMultiTopicConsumer(c, b, config)
		if err != nil {
			return nil, err
		}

		go c.nameSpaceTopicLookup(multi, config)
		return multi, nil
	}

	cons, err := c.createNewConsumer(config)
	if err != nil {
		return nil, err
	}
	c.topicLookup(cons.topic, cons.topicLookupFinished)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-cons.connected:
		return cons, err
	}
}

func (c *Client) newBrokerConnection() brokerConnection {
	return brokerConnection{
		ctx:  c.ctx,
		log:  c.log,
		conn: c.conn,
		req:  c.req,
	}
}

func (c *Client) topicLookup(topic string, topicReady requestCallback) {
	reqID := c.req.newID()
	cmd := newPartitionedMetadataCommand(reqID, topic)
	respHandler := func(resp *command) error {
		if resp.err != nil {
			return resp.err
		}

		partitions := resp.PartitionMetadataResponse.GetPartitions()
		if partitions != 0 {
			return fmt.Errorf("partitioned topics not supported") // TODO support
		}

		return nil
	}

	err := c.conn.SendCallbackCommand(c.req, reqID, cmd, respHandler)
	if err != nil {
		c.log.Printf("Getting partitioned meta data failed: %w", err)
		return
	}

	reqID = c.req.newID()
	c.req.addCallbackCustom(reqID, topicReady, topic)
	if err := c.sendLookupTopicCommand(topic, reqID); err != nil {
		c.log.Printf("Sending lookup topic command failed: %w", err)
		return
	}
}

func (c *Client) nameSpaceTopicLookup(multi *multiTopicConsumer, config ConsumerConfig) {
	pattern, err := regexp.Compile(config.TopicPattern)
	if err != nil {
		c.log.Printf("Compiling topic regexp pattern failed: %w", err)
		return
	}

	config.MessageChannel = multi.incomingMessages
	config.TopicPattern = ""
	knownTopics := map[string]struct{}{}

	tick := time.NewTicker(time.Duration(config.TopicPatternDiscoveryInterval) * time.Millisecond)
	defer tick.Stop()

	for {
		var newTopics []string

		reqID := c.req.newID()
		cmd := newGetTopicsOfNamespaceCommand(reqID)

		respHandler := func(resp *command) error {
			if resp.err != nil {
				return resp.err
			}

			topics := resp.GetTopicsOfNamespaceResponse.Topics
			for _, name := range topics {
				t, err := newTopic(name)
				if err != nil {
					c.log.Printf("Processing topic name failed: %w", err)
					continue
				}

				if !pattern.MatchString(t.localName) {
					continue
				}

				if _, ok := knownTopics[t.completeTopicName]; !ok {
					newTopics = append(newTopics, t.completeTopicName)
					knownTopics[t.completeTopicName] = struct{}{}
				}
			}

			return nil
		}

		// TODO handle deleted topics

		err = c.conn.SendCallbackCommand(c.req, reqID, cmd, respHandler)
		if err != nil {
			c.log.Printf("Getting topics of namespace failed: %w", err)
			return
		}

		for _, topic := range newTopics {
			config.Topic = topic
			cons, err := c.createNewConsumer(config)
			if err != nil {
				c.log.Printf("Creating consumer failed: %w", err)
				return
			}
			cons.multi = multi
			multi.addConsumer(cons.consumerID, cons)
			c.topicLookup(cons.topic, cons.topicLookupFinished)
		}

		select {
		case <-tick.C:
			break
		case <-c.ctx.Done():
			return
		}
	}
}

// CloseConsumer closes a specific consumer.
func (c *Client) CloseConsumer(consumerID uint64) error {
	cons, ok := c.consumers.getAndDelete(consumerID)
	if !ok {
		return fmt.Errorf("consumer %d not found", consumerID)
	}

	var err error
	cons.stateMu.Lock()
	if cons.state == consumerReady || cons.state == consumerSubscribed {
		cons.state = consumerClosing
		cons.stateMu.Unlock()

		reqID := c.req.newID()
		cmd := newCloseConsumerCommand(consumerID, reqID)
		err = c.conn.SendCallbackCommand(c.req, reqID, cmd)

		cons.stateMu.Lock()
		cons.state = consumerClosed
	}
	cons.stateMu.Unlock()

	return err
}

// CloseProducer closes a specific producer.
func (c *Client) CloseProducer(producerID uint64) error {
	_, ok := c.producers.getAndDelete(producerID)
	if !ok {
		return fmt.Errorf("producer %d not found", producerID)
	}

	reqID := c.req.newID()
	cmd := newCloseProducerCommand(producerID, reqID)
	return c.conn.SendCallbackCommand(c.req, reqID, cmd)
}

// Close closes all consumers, producers and the client connection.
func (c *Client) Close() error {
	c.cancel()

	c.connMutex.Lock()
	if c.conn == nil {
		c.connMutex.Unlock()
		return nil
	}
	c.connMutex.Unlock()

	// TODO: ensure no new consumers or producers are created during shutdown

	for _, cons := range c.consumers.all() {
		_ = c.CloseConsumer(cons.consumerID)
	}

	for _, prods := range c.producers.all() {
		_ = c.CloseConsumer(prods.producerID)
	}

	err := c.conn.close()

	<-c.stopped

	return err
}

func (c *Client) sendLookupTopicCommand(topic string, reqID uint64) error {
	base := &pb.BaseCommand{
		Type: pb.BaseCommand_LOOKUP.Enum(),
		LookupTopic: &pb.CommandLookupTopic{
			Topic:         proto.String(topic),
			RequestId:     proto.Uint64(reqID),
			Authoritative: proto.Bool(false),
		},
	}
	return c.conn.WriteCommand(base, nil)
}

func (c *Client) readCommands() {
	defer close(c.stopped)

	for {
		cmd, err := c.conn.readCommand()
		if err != nil {
			if err == ErrNetClosing {
				return
			}

			c.log.Printf("Reading command failed: %w", err)
			return
		}

		if err = c.processReceivedCommand(cmd); err != nil {
			c.log.Printf("Processing received command %+v failed: %w", cmd, err)
		}
	}
}

func (c *Client) processReceivedCommand(cmd *command) error {
	c.log.Printf("*** Received command: %+v", cmd)

	handler, ok := c.cmds[*cmd.Type]
	if !ok {
		return fmt.Errorf("unsupported command %q", cmd.GetType())
	}

	if handler == nil {
		return nil
	}

	return handler(cmd)
}

func newPartitionedMetadataCommand(reqID uint64, topic string) *pb.BaseCommand {
	return &pb.BaseCommand{
		Type: pb.BaseCommand_PARTITIONED_METADATA.Enum(),
		PartitionMetadata: &pb.CommandPartitionedTopicMetadata{
			Topic:     proto.String(topic),
			RequestId: proto.Uint64(reqID),
		},
	}
}