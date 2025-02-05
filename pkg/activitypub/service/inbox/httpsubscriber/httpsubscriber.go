/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package httpsubscriber

import (
	"context"
	"net/http"
	"net/url"

	wmhttp "github.com/ThreeDotsLabs/watermill-http/pkg/http"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/trustbloc/sidetree-core-go/pkg/restapi/common"

	"github.com/trustbloc/orb/internal/pkg/log"
	"github.com/trustbloc/orb/pkg/httpserver/auth"
	"github.com/trustbloc/orb/pkg/lifecycle"
)

const (
	// ActorIRIKey is the metadata key for the actor IRI.
	ActorIRIKey = "actor-iri"

	defaultBufferSize = 100

	loggerModule = "activitypub_service"
)

// Config holds the HTTP subscriber configuration parameters.
type Config struct {
	ServiceEndpoint string
	BufferSize      int
}

type signatureVerifier interface {
	VerifyRequest(req *http.Request) (bool, *url.URL, error)
}

type authTokenManager interface {
	RequiredAuthTokens(endpoint, method string) ([]string, error)
}

// Subscriber implements a subscriber for Watermill that handles HTTP requests.
type Subscriber struct {
	*lifecycle.Lifecycle
	*Config

	pubChan          chan *message.Message
	msgChan          chan *message.Message
	stopped          chan struct{}
	done             chan struct{}
	unmarshalMessage wmhttp.UnmarshalMessageFunc
	verifier         signatureVerifier
	tokenVerifier    *auth.TokenVerifier
	logger           *log.StructuredLog
}

// New returns a new HTTP subscriber.
func New(cfg *Config, sigVerifier signatureVerifier, tm authTokenManager) *Subscriber {
	if cfg.BufferSize == 0 {
		cfg.BufferSize = defaultBufferSize
	}

	s := &Subscriber{
		Config:           cfg,
		unmarshalMessage: wmhttp.DefaultUnmarshalMessageFunc,
		verifier:         sigVerifier,
		pubChan:          make(chan *message.Message, cfg.BufferSize),
		msgChan:          make(chan *message.Message, cfg.BufferSize),
		stopped:          make(chan struct{}),
		done:             make(chan struct{}),
		tokenVerifier:    auth.NewTokenVerifier(tm, cfg.ServiceEndpoint, http.MethodPost),
		logger:           log.NewStructured(loggerModule, log.WithFields(log.WithServiceName(cfg.ServiceEndpoint))),
	}

	s.Lifecycle = lifecycle.New("httpsubscriber-"+cfg.ServiceEndpoint,
		lifecycle.WithStop(s.stop),
		lifecycle.WithStart(func() {
			go s.publisher()
		}),
	)

	// Start the service immediately.
	s.Start()

	return s
}

// Subscribe returns the channel over which incoming messages are sent.
func (s *Subscriber) Subscribe(_ context.Context, _ string) (<-chan *message.Message, error) {
	return s.msgChan, nil
}

// Close stops the subscriber.
func (s *Subscriber) Close() error {
	s.Stop()

	return nil
}

// Path returns the base path of the target endpoint for this subscriber.
func (s *Subscriber) Path() string {
	return s.ServiceEndpoint
}

// Method returns the HTTP method, which is always POST.
func (s *Subscriber) Method() string {
	return http.MethodPost
}

// Handler returns the handler that should be invoked when an HTTP request is posted to the target endpoint.
// This handler must be registered with an HTTP server.
func (s *Subscriber) Handler() common.HTTPRequestHandler {
	return s.handleMessage
}

func (s *Subscriber) handleMessage(w http.ResponseWriter, r *http.Request) {
	var actorIRI *url.URL

	if !s.tokenVerifier.Verify(r) {
		s.logger.Debug("Request was not verified using authorization bearer tokens. Verifying request via HTTP signature",
			log.WithSenderURL(r.URL))

		verified, actor, err := s.verifier.VerifyRequest(r)
		if err != nil {
			s.logger.Error("Error verifying HTTP signature", log.WithError(err), log.WithSenderURL(r.URL))

			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		if !verified {
			s.logger.Info("Invalid HTTP signature", log.WithSenderURL(r.URL))

			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		actorIRI = actor
	} else {
		s.logger.Debug("Request was verified with a bearer token or no authorization was required.", log.WithSenderURL(r.URL))
	}

	msg, err := s.unmarshalMessage("", r)
	if err != nil {
		s.logger.Warn("Error reading message", log.WithError(err), log.WithSenderURL(r.URL))

		w.WriteHeader(http.StatusBadRequest)

		return
	}

	if actorIRI != nil {
		msg.Metadata[ActorIRIKey] = actorIRI.String()
	}

	s.logger.Debug("Handling message", log.WithMessageID(msg.UUID), log.WithActorIRI(actorIRI), log.WithSenderURL(r.URL))

	err = s.publish(msg)
	if err != nil {
		s.logger.Info("Message wasn't sent", log.WithMessageID(msg.UUID), log.WithError(err), log.WithSenderURL(r.URL))

		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}

	s.respond(msg, w, r)
}

func (s *Subscriber) publish(msg *message.Message) error {
	if s.State() != lifecycle.StateStarted {
		return lifecycle.ErrNotStarted
	}

	s.pubChan <- msg

	s.logger.Debug("Message was posted to publisher", log.WithMessageID(msg.UUID))

	return nil
}

func (s *Subscriber) publisher() {
	s.logger.Info("Starting publisher.")

	for {
		select {
		case msg := <-s.pubChan:
			s.msgChan <- msg

			s.logger.Debug("Message was delivered to subscriber", log.WithMessageID(msg.UUID))

		case <-s.stopped:
			s.logger.Info("Stopping publisher.")

			close(s.done)

			return
		}
	}
}

func (s *Subscriber) respond(msg *message.Message, w http.ResponseWriter, r *http.Request) {
	select {
	case <-msg.Acked():
		s.logger.Debug("Ack received for message", log.WithMessageID(msg.UUID))

		w.WriteHeader(http.StatusOK)

	case <-msg.Nacked():
		s.logger.Warn("Nack received for message", log.WithMessageID(msg.UUID))

		w.WriteHeader(http.StatusInternalServerError)

	case <-r.Context().Done():
		s.logger.Info("Timed out waiting for ack or nack for message",
			log.WithMessageID(msg.UUID), log.WithError(r.Context().Err()))

		w.WriteHeader(http.StatusInternalServerError)

	case <-s.stopped:
		s.logger.Info("Message was not handled since service was stopped", log.WithMessageID(msg.UUID))

		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (s *Subscriber) stop() {
	s.logger.Info("Stopping HTTP subscriber")

	close(s.stopped)

	// Wait for the publisher to stop so that we don't close the message channel
	// while we're trying to publish a message to it (which would result in a panic).
	<-s.done

	close(s.msgChan)

	s.logger.Info("... HTTP subscriber stopped.")
}
