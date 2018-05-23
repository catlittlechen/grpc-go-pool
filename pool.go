// Package grpcpool provides a pool of grpc clients
package grpcpool

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
)

var (
	// ErrClosed is the error when the client pool is closed
	ErrClosed = errors.New("grpc pool: client pool is closed")
	// ErrTimeout is the error when the client pool timed out
	ErrTimeout = errors.New("grpc pool: client pool timed out")
	// ErrAlreadyClosed is the error when the client conn was already closed
	ErrAlreadyClosed = errors.New("grpc pool: the connection was already closed")
	// ErrFullPool is the error when the pool is already full
	ErrFullPool = errors.New("grpc pool: closing a ClientConn into a full pool")
)

// Factory is a function type creating a grpc client
type Factory func() (*grpc.ClientConn, error)

// Pool is the grpc client pool
type Pool struct {
	clients     chan ClientConn
	factory     Factory
	idleTimeout time.Duration
	closed      bool
	mu          sync.RWMutex
}

// ClientConn is the wrapper for a grpc client conn
type ClientConn struct {
	*grpc.ClientConn
	pool      *Pool
	timeUsed  time.Time
	unhealthy bool
}

// New creates a new clients pool with the given initial amd maximum capacity,
// and the timeout for the idle clients. Returns an error if the initial
// clients could not be created
func New(factory Factory, init, capacity int, idleTimeout time.Duration) (*Pool, error) {
	if capacity <= 0 {
		capacity = 1
	}
	if init < 0 {
		init = 0
	}
	if init > capacity {
		init = capacity
	}
	p := &Pool{
		clients:     make(chan ClientConn, capacity),
		factory:     factory,
		idleTimeout: idleTimeout,
	}
	for i := 0; i < init; i++ {
		c, err := factory()
		if err != nil {
			return nil, err
		}

		p.clients <- ClientConn{
			ClientConn: c,
			pool:       p,
			timeUsed:   time.Now(),
		}
	}
	// Fill the rest of the pool with empty clients
	for i := 0; i < capacity-init; i++ {
		p.clients <- ClientConn{
			pool: p,
		}
	}
	return p, nil
}

// Close empties the pool calling Close on all its clients.
// You can call Close while there are outstanding clients.
// It waits for all clients to be returned (Close).
// The pool channel is then closed, and Get will not be allowed anymore
func (p *Pool) Close() {
	if p == nil {
		return
	}

	p.mu.Lock()
	clients := p.clients
	p.clients = nil
	p.closed = true
	p.mu.Unlock()

	if clients == nil {
		return
	}

	close(clients)
	for i := 0; i < p.Capacity(); i++ {
		client := <-clients
		if client.ClientConn == nil {
			continue
		}
		client.ClientConn.Close()
	}
}

// IsClosed returns true if the client pool is closed.
func (p *Pool) IsClosed() bool {
	return p == nil || p.closed
}

// Get will return the next available client. If capacity
// has not been reached, it will create a new one using the factory. Otherwise,
// it will wait till the next client becomes available or a timeout.
// A timeout of 0 is an indefinite wait
func (p *Pool) Get(ctx context.Context) (*ClientConn, error) {
	if p == nil {
		return nil, ErrClosed
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.IsClosed() {
		return nil, ErrClosed
	}

	wrapper := ClientConn{
		pool: p,
	}
	select {
	case wrapper = <-p.clients:
		// All good
	case <-ctx.Done():
		return nil, ErrTimeout
	}

	// If the wrapper is old, close the connection and create a new one. It's
	// safe to assume that there isn't any newer client as the client we fetched
	// is the first in the channel
	idleTimeout := p.idleTimeout
	if wrapper.ClientConn != nil && idleTimeout > 0 &&
		wrapper.timeUsed.Add(idleTimeout).Before(time.Now()) {

		wrapper.ClientConn.Close()
		wrapper.ClientConn = nil
	}

	var err error
	if wrapper.ClientConn == nil {
		wrapper.ClientConn, err = p.factory()
		if err != nil {
			// If there was an error, we want to put back a placeholder
			// client in the channel
			p.clients <- ClientConn{
				pool: p,
			}
		}
	}

	return &wrapper, err
}

func (p *Pool) put(wrapper *ClientConn) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.IsClosed() {
		return ErrClosed
	}

	select {
	case p.clients <- *wrapper:
		// All good
	default:
		return ErrFullPool
	}

	return nil
}

// Unhealthy marks the client conn as unhealthy, so that the connection
// gets reset when closed
func (c *ClientConn) Unhealthy() {
	c.unhealthy = true
}

// Close returns a ClientConn to the pool. It is safe to call multiple time,
// but will return an error after first time
func (c *ClientConn) Close() error {
	if c == nil {
		return nil
	}

	if c.ClientConn == nil {
		return ErrAlreadyClosed
	}

	if c.pool.IsClosed() {
		return ErrClosed
	}

	if c.unhealthy {
		c.ClientConn.Close()
		c.ClientConn = nil
	}

	// We're cloning the wrapper so we can set ClientConn to nil in the one
	// used by the user
	wrapper := ClientConn{
		pool:       c.pool,
		ClientConn: c.ClientConn,
		timeUsed:   time.Now(),
	}

	err := c.pool.put(&wrapper)
	if err != nil {
		return err
	}

	c.ClientConn = nil // Mark as closed
	return nil
}

// Capacity returns the capacity
func (p *Pool) Capacity() int {
	if p.IsClosed() {
		return 0
	}
	return cap(p.clients)
}

// Available returns the number of currently unused clients
func (p *Pool) Available() int {
	if p.IsClosed() {
		return 0
	}
	return len(p.clients)
}
