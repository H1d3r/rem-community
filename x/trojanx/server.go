package trojanx

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/chainreactors/rem/x/trojanx/internal/common"
	"github.com/chainreactors/rem/x/trojanx/internal/pipe"
	"github.com/chainreactors/rem/x/trojanx/internal/tunnel"
	"github.com/chainreactors/rem/x/trojanx/protocol"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"net"
	"strconv"
)

type Server struct {
	ctx                   context.Context
	config                *TrojanConfig
	tlsListener           net.Listener
	logger                *logrus.Logger
	dial                  func(ctx context.Context, network, addr string) (net.Conn, error)
	connectHandler        connectHandler
	authenticationHandler authenticationHandler
	requestHandler        requestHandler
	errorHandler          errorHandler
}

func (s *Server) ListenAndServe(network, addr string) error {
	var err error
	tcpListener, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	var tlsCertificates []tls.Certificate
	if s.config.TLSConfig != nil {
		tlsCertificates = append(tlsCertificates, s.config.TLSConfig.Certificate)
		s.tlsListener = tls.NewListener(tcpListener, &tls.Config{
			Certificates: tlsCertificates,
		})
	} else {
		return errors.New("TLSConfig is nil")
	}
	for {
		var conn net.Conn
		conn, err = s.tlsListener.Accept()
		if err != nil {
			s.errorHandler(s.ctx, err)
			continue
		}
		go s.ServeConn(conn)
	}
}

func (s *Server) ParseRequest(conn net.Conn) (*protocol.Request, error) {
	ctx := context.Background()
	if !s.connectHandler(ctx) {
		return nil, fmt.Errorf("connect handler failed")
	}
	token, err := protocol.GetToken(conn)
	if err != nil {
		s.errorHandler(ctx, err)
		return nil, err
	}
	if !s.authenticationHandler(ctx, token, s.config.Password) {
		return nil, fmt.Errorf("authentication failed")
	}

	req, err := protocol.ParseRequest(conn)
	if err != nil {
		s.errorHandler(ctx, err)
		return nil, err
	}
	return req, nil
}

func (s *Server) ServeConn(conn net.Conn) {
	defer conn.Close()
	ctx := context.Background()
	if !s.connectHandler(ctx) {
		return
	}
	token, err := protocol.GetToken(conn)
	if err != nil {
		s.errorHandler(ctx, err)
		return
	}
	if !s.authenticationHandler(ctx, token, s.config.Password) {
		s.logger.Debugln("authentication not passed", conn.RemoteAddr())
		if s.config.ReverseProxyConfig == nil {
			return
		}
		s.relayReverseProxy(ctx, conn, token)
		return
	}
	req, err := protocol.ParseRequest(conn)
	if err != nil {
		s.errorHandler(ctx, err)
		return
	}
	s.handler(ctx, req, conn)
}

func (s *Server) relayReverseProxy(ctx context.Context, conn net.Conn, token string) {
	remoteURL := net.JoinHostPort(s.config.ReverseProxyConfig.Host, strconv.Itoa(s.config.ReverseProxyConfig.Port))
	dst, err := net.Dial("tcp", remoteURL)
	if err != nil {
		s.errorHandler(ctx, err)
		return
	}
	s.logger.Debugln("reverse proxy policy", conn.RemoteAddr(), dst.LocalAddr())
	defer dst.Close()
	if _, err := dst.Write([]byte(token)); err != nil {
		s.errorHandler(ctx, err)
		return
	}
	go pipe.Copy(dst, conn)
	pipe.Copy(conn, dst)
}

func (s *Server) relayConnLoop(ctx context.Context, conn net.Conn, req *protocol.Request) {
	dial := s.dial
	if dial == nil {
		dial = func(ctx context.Context, net_, addr string) (net.Conn, error) {
			return net.Dial(net_, addr)
		}
	}
	dst, err := dial(ctx, "tcp", net.JoinHostPort(req.DescriptionAddress, strconv.Itoa(req.DescriptionPort)))
	if err != nil {
		s.errorHandler(ctx, err)
		return
	}
	defer dst.Close()
	go pipe.Copy(dst, conn)
	pipe.Copy(conn, dst)
}

func (s *Server) handler(ctx context.Context, req *protocol.Request, conn net.Conn) {
	if req.Command == protocol.CommandUDP {
		s.relayPacketLoop(conn)
	} else if req.Command == protocol.CommandConnect {
		s.relayConnLoop(ctx, conn, req)
	}
}

func (s *Server) relayPacketLoop(conn net.Conn) {
	udpConn, _ := net.ListenPacket("udp4", "")
	defer udpConn.Close()
	defer conn.Close()
	outbound := tunnel.UDPConn{udpConn.(*net.UDPConn)}
	inbound := tunnel.TrojanConn{conn}
	errChan := make(chan error, 2)
	copyPacket := func(a, b common.PacketConn) {
		for {
			buf := make([]byte, common.MaxPacketSize)
			n, metadata, err := a.ReadWithMetadata(buf)
			if err != nil {
				errChan <- err
				return
			}
			if n == 0 {
				errChan <- nil
				return
			}
			_, err = b.WriteWithMetadata(buf[:n], metadata)
			if err != nil {
				errChan <- err
				return
			}
		}
	}
	go copyPacket(&inbound, &outbound)
	go copyPacket(&outbound, &inbound)
	select {
	case err := <-errChan:
		if err != nil {
			s.logger.Error(err)
		}
	}
}

func NewServer(opts ...Option) *Server {
	srv := &Server{
		logger: logrus.New(),
	}

	for _, opt := range opts {
		opt(srv)
	}
	if srv.authenticationHandler == nil {
		srv.authenticationHandler = srv.defaultAuthenticationHandler
	}
	if srv.requestHandler == nil {
		srv.requestHandler = srv.defaultRequestHandler
	}
	if srv.errorHandler == nil {
		srv.errorHandler = srv.defaultErrorHandler
	}
	if srv.connectHandler == nil {
		srv.connectHandler = srv.defaultConnectHandler
	}
	return srv
}
