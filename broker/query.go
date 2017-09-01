/**********************************************************************************
* Copyright (c) 2009-2017 Misakai Ltd.
* This program is free software: you can redistribute it and/or modify it under the
* terms of the GNU Affero General Public License as published by the  Free Software
* Foundation, either version 3 of the License, or(at your option) any later version.
*
* This program is distributed  in the hope that it  will be useful, but WITHOUT ANY
* WARRANTY;  without even  the implied warranty of MERCHANTABILITY or FITNESS FOR A
* PARTICULAR PURPOSE.  See the GNU Affero General Public License  for  more details.
*
* You should have  received a copy  of the  GNU Affero General Public License along
* with this program. If not, see<http://www.gnu.org/licenses/>.
************************************************************************************/

package broker

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emitter-io/emitter/broker/subscription"
	"github.com/emitter-io/emitter/security"
	"github.com/weaveworks/mesh"
)

const (
	idSystem = uint32(0)
	idQuery  = uint32(3939663052)
)

// QueryHandler represents a query handler.
type QueryHandler func(queryType string, request []byte) (response []byte, ok bool)

// QueryManager represents a request-response manager.
type QueryManager struct {
	service  *Service       // The service to use.
	luid     security.ID    // The locally unique id of the manager.
	next     uint32         // The next available query identifier.
	awaiters *sync.Map      // The map of the awaiters.
	handlers []QueryHandler // The handlers array.
}

// newQueryManager creates a new request-response manager.
func newQueryManager(s *Service) *QueryManager {
	return &QueryManager{
		service:  s,
		luid:     security.NewID(),
		next:     0,
		awaiters: new(sync.Map),
		handlers: make([]QueryHandler, 0),
	}
}

// Start subscribes the manager to the query channel.
func (c *QueryManager) Start() {
	ssid := subscription.Ssid{idSystem, idQuery}
	if ok := c.service.onSubscribe(ssid, c); ok {
		c.service.cluster.NotifySubscribe(c.luid, ssid)
	}
}

// HandleFunc adds a handler for a query.
func (c *QueryManager) HandleFunc(handler QueryHandler) {
	c.handlers = append(c.handlers, handler)
}

// ID returns the unique identifier of the subsriber.
func (c *QueryManager) ID() string {
	return c.luid.String()
}

// Type returns the type of the subscriber
func (c *QueryManager) Type() subscription.SubscriberType {
	return subscription.SubscriberDirect
}

// Send occurs when we have received a message.
func (c *QueryManager) Send(ssid subscription.Ssid, channel []byte, payload []byte) error {
	if len(ssid) != 3 {
		return errors.New("Invalid query received")
	}

	switch string(channel) {
	case "response":
		// We received a response, find the awaiter and forward a message to it
		return c.onResponse(ssid[2], payload)

	default:
		// We received a request, need to handle that by calling the appropriate handler
		return c.onRequest(ssid, string(channel), payload)
	}
}

// onRequest handles an incoming request
func (c *QueryManager) onResponse(id uint32, payload []byte) error {
	if awaiter, ok := c.awaiters.Load(id); ok {
		awaiter.(*QueryAwaiter).receive <- payload
	}
	return nil
}

// onRequest handles an incoming request
func (c *QueryManager) onRequest(ssid subscription.Ssid, channel string, payload []byte) error {
	// Get the query and reply node
	ch := strings.Split(channel, "/")
	query := ch[0]
	reply, err := strconv.ParseInt(ch[1], 10, 64)
	if err != nil {
		return err
	}

	// Get the peer to reply to
	peer := c.service.cluster.FindPeer(mesh.PeerName(reply))

	// Go through all the handlers and execute the first matching one
	for _, handle := range c.handlers {
		if response, ok := handle(query, payload); ok {
			return peer.Send(ssid, []byte("response"), response)
		}
	}

	return errors.New("No query handler found for " + channel)
}

// Request issues a cluster-wide request.
func (c *QueryManager) Request(query string, payload []byte) (*QueryAwaiter, error) {

	// Create an awaiter
	// TODO: replace the max with the total number of cluster nodes
	awaiter := &QueryAwaiter{
		id:      atomic.AddUint32(&c.next, 1),
		receive: make(chan []byte),
		maximum: c.service.NumPeers(),
		manager: c,
	}

	// Store an awaiter
	c.awaiters.Store(awaiter.id, awaiter)

	// Prepare a channel with the reply-to address
	channel := fmt.Sprintf("%v/%v", query, c.service.LocalName())

	// Publish the query as a message
	c.service.publish(subscription.Ssid{idSystem, idQuery, awaiter.id}, []byte(channel), payload)
	return awaiter, nil
}

// QueryAwaiter represents an asynchronously awaiting response channel.
type QueryAwaiter struct {
	id      uint32        // The identifier of the query.
	maximum int           // The maximum number of responses to wait for.
	receive chan []byte   // The receive channel to use.
	manager *QueryManager // The query manager used.
}

// Gather awaits for the responses to be received, blocking until we're done.
func (a *QueryAwaiter) Gather(timeout time.Duration) (r [][]byte) {
	defer func() { a.manager.awaiters.Delete(a.id) }()
	r = make([][]byte, 0, 4)
	t := time.After(timeout)
	c := a.maximum

	for {
		select {
		case msg := <-a.receive:
			r = append(r, msg)
			c-- // Decrement the counter
			if c == 0 {
				return // We got all the responses we needed
			}

		case <-t:
			return // We timed out
		}
	}
}
