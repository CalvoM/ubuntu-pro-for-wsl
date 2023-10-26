// Package landscape implements a client to the Landscape Host Agent API service.
package landscape

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	landscapeapi "github.com/canonical/landscape-hostagent-api"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/config"
	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/distros/database"
	log "github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/grpc/logstreamer"
	"github.com/ubuntu/decorate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/ini.v1"
)

// Client is a client to the landscape service, served remotely.
type Client struct {
	db   *database.DistroDB
	conf Config

	// stopped indicates that the Client has been stopped and is no longer usable
	stopped chan struct{}
	once    sync.Once

	// Cached hostname
	hostname string

	// Connection
	conn   *connection
	connMu sync.RWMutex
}

type connection struct {
	ctx    context.Context
	cancel func()

	grpcConn   *grpc.ClientConn
	grpcClient landscapeapi.LandscapeHostAgent_ConnectClient
	once       sync.Once

	receivingCommands sync.WaitGroup
}

// Config is a configuration provider for ProToken and the Landscape URL.
type Config interface {
	LandscapeClientConfig(context.Context) (string, config.Source, error)

	Subscription(context.Context) (string, config.Source, error)

	LandscapeAgentUID(context.Context) (string, error)
	SetLandscapeAgentUID(context.Context, string) error
}

type options struct {
	hostname string
}

// Option is an optional argument for NewClient.
type Option = func(*options)

// NewClient creates a new Client for the Landscape service.
func NewClient(conf Config, db *database.DistroDB, args ...Option) (*Client, error) {
	var opts options

	for _, f := range args {
		f(&opts)
	}

	if opts.hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("could not get host name: %v", err)
		}
		opts.hostname = hostname
	}

	c := &Client{
		conf:     conf,
		db:       db,
		hostname: opts.hostname,
		stopped:  make(chan struct{}),
	}

	return c, nil
}

// hostagentURL parses the landscape config file to find the hostagent URL.
func (c *Client) hostagentURL(ctx context.Context) (string, error) {
	config, _, err := c.conf.LandscapeClientConfig(ctx)
	if err != nil {
		return "", err
	}

	r := strings.NewReader(config)
	data, err := ini.Load(r)
	if err != nil {
		return "", fmt.Errorf("could not parse config: %v", err)
	}

	s, err := data.GetSection("host")
	if err != nil { // Section does not exist
		return "", nil
	}

	k, err := s.GetKey("url")
	if err != nil { // Key does not exist
		return "", nil
	}

	return k.String(), nil
}

// Connect starts the connection and starts talking to the server.
// Call disconnect to deallocate resources.
func (c *Client) Connect(ctx context.Context) (err error) {
	defer decorate.OnError(&err, "could not connect to Landscape")

	if c.Connected() {
		return errors.New("already connected")
	}

	// Dummy connection to indicate that a first attempt was attempted
	c.conn = &connection{}

	address, err := c.hostagentURL(ctx)
	if err != nil {
		return err
	}
	if address == "" {
		return errors.New("no hostagent URL provided in the Landscape configuration")
	}

	defer func() {
		go c.keepConnected(ctx, address)
	}()

	// First connection
	conn, err := c.connect(ctx, address)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	return nil
}

// keepConnected supervises the connection. If it drops, reconnection is attempted.
func (c *Client) keepConnected(ctx context.Context, address string) {
	const growthFactor = 2
	const minWait = time.Second
	const maxWait = 10 * time.Minute
	wait := time.Second

	// The loop body is inside this function so that defers can be used
	keepLoooping := true
	for keepLoooping {
		keepLoooping = func() (keepLooping bool) {
			// Using a timer rather than a time.After to avoid leaking
			// the timer for up to $maxWait.
			tk := time.NewTimer(wait)
			defer tk.Stop()

			select {
			case <-tk.C:
			case <-c.stopped:
				// Stop was called
				return false
			}

			c.connMu.Lock()
			defer c.connMu.Unlock()

			if c.conn == nil {
				// Stop was called
				return false
			}

			if c.conn.connected() {
				// Connection still active
				return true
			}

			c.conn.disconnect()

			conn, err := c.connect(ctx, address)
			if err != nil {
				log.Warningf(ctx, "Landscape reconnect: %v", err)
				wait = min(growthFactor*wait, maxWait)
				return true
			}

			c.conn = conn
			wait = minWait
			return true
		}()
	}
}

func (c *Client) connect(ctx context.Context, address string) (conn *connection, err error) {
	defer decorate.OnError(&err, "could not connect to address %q", address)

	conn = &connection{}

	// A context to control the Landscape client with (needed for as long as the connection lasts)
	conn.ctx, conn.cancel = context.WithCancel(ctx)

	// A context to control only the Dial (only needed for this function)
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	creds, err := c.transportCredentials(ctx)
	if err != nil {
		return nil, err
	}

	grpcConn, err := grpc.DialContext(dialCtx, address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	conn.grpcConn = grpcConn

	cl := landscapeapi.NewLandscapeHostAgentClient(grpcConn)
	client, err := cl.Connect(conn.ctx)
	if err != nil {
		return nil, err
	}
	conn.grpcClient = client

	// Get ready to receive commands
	conn.receivingCommands.Add(1)
	go func() {
		defer conn.disconnect()
		defer conn.receivingCommands.Done()

		if err := c.receiveCommands(conn); err != nil {
			log.Errorf(conn.ctx, "Landscape receive commands exited: %v", err)
		}
	}()

	if err := c.handshake(ctx, conn); err != nil {
		conn.disconnect()
		return nil, err
	}

	log.Infof(ctx, "Connection to Landscape established")

	return conn, nil
}

// handshake executes the first few messages of a connection.
//
// The client introduces itself to the server by sending info to Landscape.
// If this is the first connection ever, the server will respond by assigning
// the host a UID. This Recv is handled by receiveCommands, but handshake
// waits until the UID is received before returning.
func (c *Client) handshake(ctx context.Context, conn *connection) error {
	// Send first message
	if err := c.sendUpdatedInfo(conn); err != nil {
		return err
	}

	// Not the first contact between client and server: done!
	if uid, err := c.conf.LandscapeAgentUID(ctx); err != nil {
		return err
	} else if uid != "" {
		return nil
	}

	// First contact. Wait to receive a client UID.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ctx, cancel := context.WithTimeout(conn.ctx, time.Minute)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			conn.disconnect()
			// Avoid races where the UID arrives just after cancelling the context
			err := c.conf.SetLandscapeAgentUID(ctx, "")
			return errors.Join(err, fmt.Errorf("Landscape server did not respond with a client UID"))
		case <-ticker.C:
		}

		if uid, err := c.conf.LandscapeAgentUID(ctx); err != nil {
			return err
		} else if uid != "" {
			// UID received: success.
			break
		}
	}

	return nil
}

// Stop terminates the connection and deallocates resources.
func (c *Client) Stop(ctx context.Context) {
	c.once.Do(func() {
		close(c.stopped)

		c.connMu.Lock()
		defer c.connMu.Unlock()

		if c.conn != nil {
			c.conn.disconnect()
			c.conn = nil
		}
	})
}

func (conn *connection) disconnect() {
	// Default constructed connection
	if conn.cancel == nil {
		return
	}

	conn.once.Do(func() {
		conn.cancel()
		conn.receivingCommands.Wait()
		_ = conn.grpcConn.Close()
	})
}

// Connected returns true if the Landscape client managed to connect to the server.
func (c *Client) Connected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()

	return c.conn.connected()
}

func (conn *connection) connected() bool {
	if conn == nil {
		return false
	}

	if conn.grpcConn == nil {
		return false
	}

	switch conn.grpcConn.GetState() {
	case connectivity.Idle:
		return false
	case connectivity.Shutdown:
		return false
	}

	return true
}

// transportCredentials reads the Landscape client config to check if a SSL public key is specified.
//
// If this credential is not specified, an insecure credential is returned.
// If the credential is specified but erroneous, an error is returned.
func (c *Client) transportCredentials(ctx context.Context) (cred credentials.TransportCredentials, err error) {
	defer decorate.OnError(&err, "Landscape credentials")

	conf, _, err := c.conf.LandscapeClientConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not obtain Landscape config: %v", err)
	}

	if conf == "" {
		// No Landscape config: default to insecure
		return insecure.NewCredentials(), nil
	}

	ini, err := ini.Load(strings.NewReader(conf))
	if err != nil {
		return insecure.NewCredentials(), fmt.Errorf("could not parse Landscape config file: %v", err)
	}

	const section = "client"
	const key = "ssl_public_key"

	sec, err := ini.GetSection(section)
	if err != nil {
		// No SSL public key provided: default to insecure
		return insecure.NewCredentials(), nil
	}

	k, err := sec.GetKey(key)
	if err != nil {
		// No SSL public key provided: default to insecure
		return insecure.NewCredentials(), nil
	}

	cert, err := os.ReadFile(k.String())
	if err != nil {
		return nil, fmt.Errorf("could not load SSL public key file: %v", err)
	}

	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(cert); !ok {
		return nil, fmt.Errorf("failed to add server CA's certificate: %v", err)
	}

	return credentials.NewTLS(&tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}), nil
}
