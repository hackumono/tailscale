// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package derphttp implements DERP-over-HTTP.
//
// This makes DERP look exactly like WebSockets.
// A server can implement DERP over HTTPS and even if the TLS connection
// intercepted using a fake root CA, unless the interceptor knows how to
// detect DERP packets, it will look like a web socket.
package derphttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"sync"

	"tailscale.com/derp"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Client is a DERP-over-HTTP client.
//
// It automatically reconnects on error retry. That is, a failed Send or
// Recv will report the error and not retry, but subsequent calls to
// Send/Recv will completely re-establish the connection (unless Close
// has been called).
type Client struct {
	privateKey key.Private
	logf       logger.Logf
	closed     chan struct{}
	url        *url.URL
	resp       *http.Response

	netConnMu sync.Mutex
	netConn   net.Conn

	clientMu sync.Mutex
	client   *derp.Client
}

// NewClient returns a new DERP-over-HTTP client. It connects lazily.
// To trigger a connection use Connect.
func NewClient(privateKey key.Private, serverURL string, logf logger.Logf) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("derphttp.NewClient: %v", err)
	}

	c := &Client{
		privateKey: privateKey,
		logf:       logf,
		url:        u,
		closed:     make(chan struct{}),
	}
	return c, nil
}

// Connect connects or reconnects to the server, unless already connected.
// It returns nil if there was already a good connection, or if one was made.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.connect(ctx, "derphttp.Client.Connect")
	return err
}

func (c *Client) connect(ctx context.Context, caller string) (client *derp.Client, err error) {
	// TODO: use ctx for TCP+TLS+HTTP below
	select {
	case <-c.closed:
		return nil, ErrClientClosed
	default:
	}

	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	if c.client != nil {
		return c.client, nil
	}

	c.logf("%s: connecting", caller)

	var netConn net.Conn
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s connect: %v", caller, err)
			if netConn != nil {
				netConn.Close()
			}
		}
	}()

	if c.url.Scheme == "https" {
		port := c.url.Port()
		if port == "" {
			port = "443"
		}
		config := &tls.Config{}
		var tlsConn *tls.Conn
		tlsConn, err = tls.Dial("tcp", net.JoinHostPort(c.url.Host, port), config)
		if tlsConn != nil {
			netConn = tlsConn
		}
	} else {
		netConn, err = net.Dial("tcp", c.url.Host)
	}
	if err != nil {
		return nil, err
	}

	c.netConnMu.Lock()
	c.netConn = netConn
	c.netConnMu.Unlock()

	conn := bufio.NewReadWriter(bufio.NewReader(netConn), bufio.NewWriter(netConn))

	req, err := http.NewRequest("GET", c.url.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Upgrade", "WebSocket")
	req.Header.Set("Connection", "Upgrade")
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	if err := conn.Flush(); err != nil {
		return nil, err
	}

	resp, err := http.ReadResponse(conn.Reader, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET failed: %v: %s", err, b)
	}
	resp.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))

	derpClient, err := derp.NewClient(c.privateKey, netConn, conn, c.logf)
	if err != nil {
		return nil, err
	}
	c.resp = resp
	c.client = derpClient
	return c.client, nil
}

func (c *Client) Send(dstKey key.Public, b []byte) error {
	client, err := c.connect(context.TODO(), "derphttp.Client.Send")
	if err != nil {
		return err
	}
	if err := client.Send(dstKey, b); err != nil {
		c.close()
	}
	return err
}

func (c *Client) Recv(b []byte) (derp.ReceivedMessage, error) {
	client, err := c.connect(context.TODO(), "derphttp.Client.Recv")
	if err != nil {
		return nil, err
	}
	m, err := client.Recv(b)
	if err != nil {
		c.close()
	}
	return m, err
}

// Close closes the client. It will not automatically reconnect after
// being closed.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return ErrClientClosed
	default:
	}
	close(c.closed)
	c.close()
	return nil
}

func (c *Client) close() {
	c.netConnMu.Lock()
	netConn := c.netConn
	c.netConnMu.Unlock()

	if netConn != nil {
		netConn.Close()
	}

	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client == nil {
		return
	}
	c.resp = nil
	c.client = nil
	c.netConnMu.Lock()
	c.netConn = nil
	c.netConnMu.Unlock()
}

var ErrClientClosed = errors.New("derphttp.Client closed")
