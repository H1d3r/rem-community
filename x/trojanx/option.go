package trojanx

import (
	"context"
	"github.com/sirupsen/logrus"
	"net"
)

type Option func(s *Server)

func WithLogger(l *logrus.Logger) Option {
	return func(s *Server) {
		s.logger = l
	}
}

func WithConfig(config *TrojanConfig) Option {
	return func(s *Server) {
		s.config = config
	}
}

func WithDial(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(s *Server) {
		s.dial = dial
	}
}

func WithConnectHandler(handler connectHandler) Option {
	return func(s *Server) {
		s.connectHandler = handler
	}
}

func WhichAuthenticationHandler(handler authenticationHandler) Option {
	return func(s *Server) {
		s.authenticationHandler = handler
	}
}

func WhichRequestHandler(handler requestHandler) Option {
	return func(s *Server) {
		s.requestHandler = handler
	}
}

func WhichErrorHandler(handler errorHandler) Option {
	return func(s *Server) {
		s.errorHandler = handler
	}
}
