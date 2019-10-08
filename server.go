package pvaccess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quentinmit/go-pvaccess/internal/connection"
	"github.com/quentinmit/go-pvaccess/internal/ctxlog"
	"github.com/quentinmit/go-pvaccess/internal/proto"
	"github.com/quentinmit/go-pvaccess/internal/search"
	"github.com/quentinmit/go-pvaccess/pvdata"
	"golang.org/x/sync/errgroup"
)

type Server struct {
	search *search.Server
	ln     net.Listener

	mu               sync.RWMutex
	channelProviders []ChannelProvider
}

const udpAddr = ":5076"

// TODO: Pick a random TCP port for each server and announce it in beacons
const tcpAddr = ":5075"

func NewServer() (*Server, error) {
	s := &Server{}
	s.channelProviders = []ChannelProvider{&serverChannel{s}}
	return s, nil
}

func (srv *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", "")
	if err != nil {
		return err
	}
	return srv.Serve(ctx, ln)
}

// TODO: UDP beacon support
func (srv *Server) Serve(ctx context.Context, l net.Listener) error {
	srv.search = &search.Server{
		ServerAddr: l.Addr().(*net.TCPAddr),
	}
	srv.ln = l
	var g errgroup.Group
	g.Go(func() error {
		<-ctx.Done()
		ctxlog.L(ctx).Infof("PVAccess server shutting down")
		return srv.ln.Close()
	})
	g.Go(func() error {
		if err := srv.search.Serve(ctx); err != nil {
			ctxlog.L(ctx).Errorf("failed to serve search requests: %v", err)
			return err
		}
		return nil
	})
	g.Go(func() error {
		for {
			conn, err := srv.ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Temporary() {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				return err
			}
			g.Go(func() error {
				srv.handleConnection(ctx, conn)
				return nil
			})
		}
	})
	return g.Wait()
}

type serverConn struct {
	*connection.Connection
	srv *Server
	g   *errgroup.Group

	mu       sync.Mutex
	channels map[pvdata.PVInt]Channel
	requests map[pvdata.PVInt]*request
}

type connChannel struct {
	name      string
	handleRPC func(ctx context.Context, args pvdata.PVStructure) (response interface{}, err error)
}

type requestStatus int

const (
	INIT = iota
	READY
	REQUEST_IN_PROGRESS
	CANCELLED
	DESTROYED
)

var requestStatusNames = map[requestStatus]string{
	0: "INIT",
	1: "READY",
	2: "REQUEST_IN_PROGRESS",
	3: "CANCELLED",
	4: "DESTROYED",
}

func (r requestStatus) String() string {
	return requestStatusNames[r]
}

type request struct {
	doer   interface{}
	cancel func()
	status requestStatus
}

func (c *serverConn) addRequest(id pvdata.PVInt, r *request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.requests[id]; ok {
		if existing.status != DESTROYED {
			return fmt.Errorf("request ID %x already exists with status %s", id, requestStatusNames[existing.status])
		}
	}
	c.requests[id] = r
	return nil
}

func (srv *Server) newConn(conn io.ReadWriter) *serverConn {
	c := connection.New(conn, proto.FLAG_FROM_SERVER)
	return &serverConn{
		Connection: c,
		srv:        srv,
		channels:   make(map[pvdata.PVInt]Channel),
		requests:   make(map[pvdata.PVInt]*request),
	}
}

func (srv *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
		"local_addr":  srv.ln.Addr(),
		"remote_addr": conn.RemoteAddr(),
		"proto":       "tcp",
	})
	c := srv.newConn(conn)
	g, ctx := errgroup.WithContext(ctx)
	c.g = g
	g.Go(func() error {
		ctxlog.L(ctx).Infof("new connection")
		return c.serve(ctx)
	})
	if err := g.Wait(); err != nil {
		ctxlog.L(ctx).Errorf("error on connection: %v", err)
	}
}

func (c *serverConn) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.Version = pvdata.PVByte(2)
	// 0 = Ignore byte order field in header
	if err := c.SendCtrl(ctx, proto.CTRL_SET_BYTE_ORDER, 0); err != nil {
		return err
	}

	req := proto.ConnectionValidationRequest{
		ServerReceiveBufferSize:            pvdata.PVInt(c.ReceiveBufferSize()),
		ServerIntrospectionRegistryMaxSize: 0x7fff,
		AuthNZ: []string{"anonymous"},
	}
	c.SendApp(ctx, proto.APP_CONNECTION_VALIDATION, &req)

	for {
		if err := c.handleServerOnePacket(ctx); err != nil {
			if err == io.EOF {
				cancel()
				// TODO: Cleanup resources (requests, channels, etc.)
				ctxlog.L(ctx).Infof("client went away, closing connection")
				return nil
			}
			return err
		}
	}
}
func (c *serverConn) handleServerOnePacket(ctx context.Context) error {
	msg, err := c.Next(ctx)
	if err != nil {
		return err
	}
	if f, ok := serverDispatch[msg.Header.MessageCommand]; ok {
		return f(c, ctx, msg)
	}
	return nil
}

var serverDispatch = map[pvdata.PVByte]func(c *serverConn, ctx context.Context, msg *connection.Message) error{
	proto.APP_CONNECTION_VALIDATION: (*serverConn).handleConnectionValidation,
	proto.APP_CHANNEL_CREATE:        (*serverConn).handleCreateChannelRequest,
	proto.APP_CHANNEL_RPC:           (*serverConn).handleChannelRPC,
	proto.APP_SEARCH_REQUEST:        (*serverConn).handleSearchRequest,
}

func (c *serverConn) handleConnectionValidation(ctx context.Context, msg *connection.Message) error {
	var resp proto.ConnectionValidationResponse
	if err := msg.Decode(&resp); err != nil {
		return err
	}
	ctxlog.L(ctx).Infof("received connection validation %#v", resp)
	// TODO: Implement flow control
	return c.SendApp(ctx, proto.APP_CONNECTION_VALIDATED, &proto.ConnectionValidated{})
}

func (c *serverConn) handleCreateChannelRequest(ctx context.Context, msg *connection.Message) error {
	var req proto.CreateChannelRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	var resp proto.CreateChannelResponse
	if len(req.Channels) == 1 {
		ch := req.Channels[0]
		ctxlog.L(ctx).Infof("received request to create channel %q as client channel ID %x", ch.ChannelName, ch.ClientChannelID)
		resp.ClientChannelID = ch.ClientChannelID
		channel, err := c.createChannel(ctx, ch.ClientChannelID, ch.ChannelName)
		if err != nil {
			resp.Status = errorToStatus(err)
		} else if channel != nil {
			resp.ServerChannelID = ch.ClientChannelID
		} else {
			resp.Status.Type = pvdata.PVStatus_ERROR
			resp.Status.Message = pvdata.PVString(fmt.Sprintf("unknown channel %q", ch.ChannelName))
		}
		ctxlog.L(ctx).Infof("channel status = %v", resp.Status)
	} else {
		resp.Status.Type = pvdata.PVStatus_ERROR
		resp.Status.Message = "wrong number of channels"
	}
	return c.SendApp(ctx, proto.APP_CHANNEL_CREATE, &resp)
}

// asyncOperation is a sentinel error to halt the current response in favor of a later asynchronous reply.
var asyncOperation = errors.New("async operation started")

func errorToStatus(err error) pvdata.PVStatus {
	if err == nil {
		return pvdata.PVStatus{}
	}
	if s, ok := err.(pvdata.PVStatus); ok {
		return s
	}
	return pvdata.PVStatus{
		Type:    pvdata.PVStatus_FATAL,
		Message: pvdata.PVString(err.Error()),
	}
}

func (c *serverConn) handleChannelRPC(ctx context.Context, msg *connection.Message) error {
	var req proto.ChannelRPCRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Debugf("CHANNEL_RPC(%#v)", req)
	resp := &proto.ChannelRPCResponseInit{
		RequestID:  req.RequestID,
		Subcommand: req.Subcommand,
	}
	err := c.handleChannelRPCBody(ctx, req)
	if err == asyncOperation {
		return nil
	}
	if err != nil {
		ctxlog.L(ctx).Warnf("Channel RPC failed: %v", err)
	}
	resp.Status = errorToStatus(err)
	return c.SendApp(ctx, proto.APP_CHANNEL_RPC, resp)
}

func (c *serverConn) handleChannelRPCBody(ctx context.Context, req proto.ChannelRPCRequest) error {
	c.mu.Lock()
	channel := c.channels[req.ServerChannelID]
	c.mu.Unlock()
	if channel == nil {
		return fmt.Errorf("unknown channel ID %x", req.ServerChannelID)
	}
	ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
		"channel":    channel.Name(),
		"channel_id": req.ServerChannelID,
		"request_id": req.RequestID,
	})
	ctxlog.L(ctx).Debugf("channel = %#v", channel)
	args, ok := req.PVRequest.Data.(pvdata.PVStructure)
	if !ok {
		return fmt.Errorf("RPC arguments were of type %T, expected PVStructure", req.PVRequest.Data)
	}
	switch req.Subcommand {
	case proto.CHANNEL_RPC_INIT:
		ctxlog.L(ctx).Printf("received request to init channel RPC with body %v", args)
		var rpcer ChannelRPCer
		if rpcc, ok := channel.(ChannelRPCCreator); ok {
			var err error
			rpcer, err = rpcc.CreateChannelRPC(ctx, args)
			if err != nil {
				return err
			}
		} else if r, ok := channel.(ChannelRPCer); ok {
			rpcer = r
		} else {
			return fmt.Errorf("channel %q (ID %x) does not support RPC", channel.Name(), req.ServerChannelID)
		}
		if err := c.addRequest(req.RequestID, &request{doer: rpcer, status: READY}); err != nil {
			return err
		}
		return nil
	default:
		ctxlog.L(ctx).Printf("received request to execute channel RPC with body %v", args)
		c.mu.Lock()
		defer c.mu.Unlock()
		r := c.requests[req.RequestID]
		if r.status != READY {
			return pvdata.PVStatus{
				Type:    pvdata.PVStatus_ERROR,
				Message: pvdata.PVString("request not READY"),
			}
		}
		rpcer, ok := r.doer.(ChannelRPCer)
		if !ok {
			return errors.New("request not for RPC")
		}
		ctx, cancel := context.WithCancel(ctx)
		r.status = REQUEST_IN_PROGRESS
		r.cancel = cancel
		c.g.Go(func() error {
			respData, err := rpcer.ChannelRPC(ctx, args)
			resp := &proto.ChannelRPCResponse{
				RequestID:      req.RequestID,
				Subcommand:     req.Subcommand,
				Status:         errorToStatus(err),
				PVResponseData: pvdata.NewPVAny(respData),
			}
			if err := c.SendApp(ctx, proto.APP_CHANNEL_RPC, resp); err != nil {
				ctxlog.L(ctx).Errorf("sending RPC response: %v", err)
			}

			c.mu.Lock()
			defer c.mu.Unlock()
			r.status = READY
			if req.Subcommand&proto.CHANNEL_RPC_DESTROY == proto.CHANNEL_RPC_DESTROY {
				r.status = DESTROYED
			}
			return nil
		})
		return asyncOperation
	}
}

func (c *serverConn) handleSearchRequest(ctx context.Context, msg *connection.Message) error {
	var req proto.SearchRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Infof("received search request %#v", req)
	return c.srv.search.Search(ctx, c.Connection, req)
}
