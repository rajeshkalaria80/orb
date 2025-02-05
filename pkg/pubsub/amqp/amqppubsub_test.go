/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package amqp

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-amqp/v2/pkg/amqp"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/stretchr/testify/require"

	"github.com/trustbloc/orb/pkg/internal/testutil/rabbitmqtestutil"
	"github.com/trustbloc/orb/pkg/lifecycle"
	"github.com/trustbloc/orb/pkg/pubsub/spi"
)

// mqURI will get set in the TestMain function.
var mqURI = ""

func TestAMQP(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		const topic = "some-topic"

		p := New(Config{URI: mqURI})
		require.NotNil(t, p)

		require.True(t, p.IsConnected())

		msgChan, err := p.Subscribe(context.Background(), topic)
		require.NoError(t, err)

		msg := message.NewMessage(watermill.NewUUID(), []byte("some payload"))
		require.NoError(t, p.PublishWithOpts(topic, msg))

		select {
		case m := <-msgChan:
			require.Equal(t, msg.UUID, m.UUID)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timed out waiting for message")
		}

		require.NoError(t, p.Close())

		_, err = p.Subscribe(context.Background(), topic)
		require.True(t, errors.Is(err, lifecycle.ErrNotStarted))
		require.True(t, errors.Is(p.Publish(topic, msg), lifecycle.ErrNotStarted))
	})

	t.Run("Publish with delivery delay -> Success", func(t *testing.T) {
		const topic = "delayed-topic"

		p := New(Config{URI: mqURI})
		require.NotNil(t, p)

		require.True(t, p.IsConnected())

		msgChan, err := p.Subscribe(context.Background(), topic)
		require.NoError(t, err)

		payload := []byte("payload for delayed delivery")

		msg := message.NewMessage(watermill.NewUUID(), payload)
		require.NoError(t, p.PublishWithOpts(topic, msg, spi.WithDeliveryDelay(time.Second)))

		select {
		case m := <-msgChan:
			require.Equal(t, payload, []byte(m.Payload))
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for message")
		}

		require.NoError(t, p.Close())

		require.True(t, errors.Is(p.PublishWithOpts(topic, msg), lifecycle.ErrNotStarted))
	})

	t.Run("Connection failure", func(t *testing.T) {
		require.Panics(t, func() {
			p := New(Config{URI: "amqp://guest:guest@localhost:9999/", MaxConnectRetries: 3})
			require.NotNil(t, p)
		})
	})

	t.Run("Pooled subscriber -> success", func(t *testing.T) {
		const (
			n     = 100
			topic = "pooled"
		)

		publishedMessages := &sync.Map{}
		receivedMessages := &sync.Map{}

		p := New(Config{
			URI:                   mqURI,
			MaxConnectionChannels: 5,
		})
		require.NotNil(t, p)
		defer func() {
			require.NoError(t, p.Close())
		}()

		msgChan, err := p.SubscribeWithOpts(context.Background(), topic, spi.WithPool(10))
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(n)

		go func(msgChan <-chan *message.Message) {
			for m := range msgChan {
				go func(msg *message.Message) {
					// Randomly fail 33% of the messages to test redelivery.
					if rand.Int31n(10) < 3 { //nolint:gosec
						msg.Nack()

						return
					}

					receivedMessages.Store(msg.UUID, msg)

					// Add a delay to simulate processing.
					time.Sleep(100 * time.Millisecond)

					msg.Ack()

					wg.Done()
				}(m)
			}
		}(msgChan)

		for i := 0; i < n; i++ {
			go func() {
				msg := message.NewMessage(watermill.NewUUID(), []byte("some payload"))
				publishedMessages.Store(msg.UUID, msg)

				require.NoError(t, p.Publish(topic, msg))
			}()
		}

		wg.Wait()

		publishedMessages.Range(func(msgID, _ interface{}) bool {
			_, ok := receivedMessages.Load(msgID)
			require.Truef(t, ok, "message not received: %s", msgID)

			return true
		})
	})

	t.Run("Redelivery attempts reached", func(t *testing.T) {
		const topic = "topic_redelivery"

		p := New(Config{
			URI:                   mqURI,
			MaxConnectionChannels: 5,
			MaxRedeliveryAttempts: 5,
			MaxRedeliveryInterval: 200 * time.Millisecond,
		})
		require.NotNil(t, p)
		defer func() {
			require.NoError(t, p.Close())
		}()

		msgChan2, err := p.SubscribeWithOpts(context.Background(), topic)
		require.NoError(t, err)

		var attempts uint32

		go func(msgChan <-chan *message.Message) {
			for m := range msgChan {
				go func(msg *message.Message) {
					// Always fail to test maximum redelivery attempts.
					msg.Nack()

					atomic.AddUint32(&attempts, 1)
				}(m)
			}
		}(msgChan2)

		go func() {
			require.NoError(t, p.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("some payload"))))
		}()

		time.Sleep(5 * time.Second)

		require.Equal(t, uint32(6), atomic.LoadUint32(&attempts))
	})
}

func TestAMQP_Error(t *testing.T) {
	const topic = "some-topic"

	t.Run("Subscriber factory error", func(t *testing.T) {
		errExpected := errors.New("injected subscriber subscriberFactory error")

		p := &PubSub{
			Lifecycle: lifecycle.New(""),
			connMgr:   &mockConnectionMgr{},
			subscriberFactory: func(connection) (initializingSubscriber, error) {
				return nil, errExpected
			},
			createPublisher: func(cfg *amqp.Config, conn connection) (publisher, error) {
				return &mockPublisher{}, nil
			},
			createWaitPublisher: func(connection) (publisher, error) {
				return &mockPublisher{}, nil
			},
		}

		p.Start()

		require.NoError(t, p.connect(), errExpected.Error())

		_, err := p.Subscribe(context.Background(), "topic")
		require.EqualError(t, err, errExpected.Error())
	})

	t.Run("Publisher factory error", func(t *testing.T) {
		errExpected := errors.New("injected publisher factory error")

		p := &PubSub{
			connMgr: &mockConnectionMgr{},
			subscriberFactory: func(connection) (initializingSubscriber, error) {
				return &mockSubscriber{}, nil
			},
			createPublisher: func(cfg *amqp.Config, conn connection) (publisher, error) {
				return nil, errExpected
			},
		}

		err := p.connect()
		require.Error(t, err)
		require.Contains(t, err.Error(), errExpected.Error())
	})

	t.Run("Subscribe error", func(t *testing.T) {
		errSubscribe := errors.New("injected subscribe error")
		errClose := errors.New("injected close error")

		p := &PubSub{
			Lifecycle:            lifecycle.New("ampq"),
			connMgr:              &mockConnectionMgr{},
			subscriber:           &mockSubscriber{err: errSubscribe, mockClosable: &mockClosable{err: errClose}},
			publisher:            &mockPublisher{mockClosable: &mockClosable{}},
			waitSubscriber:       &mockSubscriber{err: errSubscribe, mockClosable: &mockClosable{err: errClose}},
			waitPublisher:        &mockPublisher{mockClosable: &mockClosable{}},
			redeliverySubscriber: &mockSubscriber{err: errSubscribe, mockClosable: &mockClosable{err: errClose}},
		}

		p.Start()
		defer p.stop()

		_, err := p.Subscribe(context.Background(), topic)
		require.EqualError(t, err, errSubscribe.Error())
	})

	t.Run("Publisher error", func(t *testing.T) {
		errPublish := errors.New("injected publish error")
		errClose := errors.New("injected close error")

		p := &PubSub{
			Lifecycle:            lifecycle.New("ampq"),
			connMgr:              &mockConnectionMgr{},
			subscriber:           &mockSubscriber{mockClosable: &mockClosable{}},
			publisher:            &mockPublisher{err: errPublish, mockClosable: &mockClosable{err: errClose}},
			waitSubscriber:       &mockSubscriber{mockClosable: &mockClosable{}},
			waitPublisher:        &mockPublisher{err: errPublish, mockClosable: &mockClosable{err: errClose}},
			redeliverySubscriber: &mockSubscriber{mockClosable: &mockClosable{}},
		}

		p.Start()
		defer p.stop()

		err := p.Publish(topic)
		require.Error(t, err)
		require.Contains(t, err.Error(), errPublish.Error())
	})
}

func TestExtractEndpoint(t *testing.T) {
	require.Equal(t, "example.com:5671/mq",
		extractEndpoint("amqps://user:password@example.com:5671/mq"))

	require.Equal(t, "example.com:5671/mq",
		extractEndpoint("amqps://example.com:5671/mq"))

	require.Equal(t, "",
		extractEndpoint("example.com:5671/mq"))
}

func TestPubSub_GetInterval(t *testing.T) {
	p := &PubSub{
		Config: Config{
			RedeliveryMultiplier:      defaultRedeliveryMultiplier,
			RedeliveryInitialInterval: defaultRedeliveryInitialInterval,
			MaxRedeliveryInterval:     defaultMaxRedeliveryInterval,
		},
	}

	require.Equal(t, time.Duration(0), p.getRedeliveryInterval(0))
	require.Equal(t, defaultRedeliveryInitialInterval, p.getRedeliveryInterval(1))
	require.Equal(t, 3*time.Second, p.getRedeliveryInterval(2))
	require.Equal(t, 4500*time.Millisecond, p.getRedeliveryInterval(3))
}

func TestMain(m *testing.M) {
	code := 1

	defer func() { os.Exit(code) }()

	mqURIDocker, stopRabbitMQ := rabbitmqtestutil.StartRabbitMQ()
	defer stopRabbitMQ()

	mqURI = mqURIDocker

	code = m.Run()
}

type mockClosable struct {
	err error
}

func (m *mockClosable) Close() error {
	return m.err
}

type mockSubscriber struct {
	*mockClosable

	err error
}

func (m *mockSubscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if m.err != nil {
		return nil, m.err
	}

	return nil, nil
}

func (m *mockSubscriber) SubscribeInitialize(string) error {
	return m.err
}

type mockPublisher struct {
	*mockClosable

	err error
}

func newMockPublisher() *mockPublisher {
	return &mockPublisher{
		mockClosable: &mockClosable{},
	}
}

func (m *mockPublisher) Publish(topic string, messages ...*message.Message) error {
	if m.err != nil {
		return m.err
	}

	return nil
}

type mockConnectionMgr struct {
	err error
}

func (m *mockConnectionMgr) close() error {
	return m.err
}

func (m *mockConnectionMgr) getConnection(bool) (connection, error) {
	if m.err != nil {
		return nil, m.err
	}

	return &mockConnection{}, nil
}

func (m *mockConnectionMgr) isConnected() bool {
	return m.err == nil
}

type mockConnection struct{}

func (m *mockConnection) amqpConnection() *amqp.ConnectionWrapper {
	return nil
}

func (m *mockConnection) incrementChannelCount() uint32 {
	return 0
}

func (m *mockConnection) numChannels() uint32 {
	return 0
}
