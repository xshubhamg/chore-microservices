// Package rabbitmq wraps amqp091-go with the pieces we need in production:
// reconnecting connections, publisher confirms, quorum queues, a dead-letter
// exchange and a consumer run loop that survives broker restarts.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	connectTimeout = 10 * time.Second
	confirmTimeout = 5 * time.Second
)

type Client struct {
	url string
	log *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
}

// Dial connects to the broker, retrying until ctx is cancelled or the broker
// accepts the connection.
func Dial(ctx context.Context, url string, log *slog.Logger) (*Client, error) {
	c := &Client{url: url, log: log}

	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if err := c.connect(ctx); err == nil {
			log.Info("rabbitmq connected", slog.Int("attempt", attempt))
			return c, nil
		} else {
			log.Warn("rabbitmq not ready, retrying",
				slog.Int("attempt", attempt),
				slog.Duration("retry_in", backoff),
				slog.String("error", err.Error()),
			)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to rabbitmq: %w", ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	conn, err := amqp.DialConfig(c.url, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
		Dial: func(network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		return err
	}

	closes := conn.NotifyClose(make(chan *amqp.Error, 1))
	go func() {
		for err := range closes {
			if err != nil {
				c.log.Warn("rabbitmq connection closed", slog.String("error", err.Error()))
			}
		}
	}()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	return nil
}

// Channel opens a channel, reconnecting first if the connection dropped.
func (c *Client) Channel() (*amqp.Channel, error) {
	conn, err := c.connection()
	if err != nil {
		return nil, err
	}
	return conn.Channel()
}

func (c *Client) connection() (*amqp.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil && !c.conn.IsClosed() {
		return c.conn, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := c.connect(ctx); err != nil {
		return nil, fmt.Errorf("reconnect to rabbitmq: %w", err)
	}
	return c.conn, nil
}

func (c *Client) Ping(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.conn.IsClosed() {
		return errors.New("rabbitmq connection is closed")
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.conn.IsClosed() {
		return nil
	}
	return c.conn.Close()
}

// DeclareTopology creates the durable topic exchange and its dead-letter
// exchange. It is safe to call on every start.
func (c *Client) DeclareTopology(exchange string) error {
	ch, err := c.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	if err := ch.ExchangeDeclare(exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", exchange, err)
	}
	if err := ch.ExchangeDeclare(DeadLetterExchange(exchange), amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead-letter exchange: %w", err)
	}
	return nil
}

func DeadLetterExchange(exchange string) string { return exchange + ".dlx" }
