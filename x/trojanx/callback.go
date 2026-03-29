package trojanx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/chainreactors/rem/x/trojanx/protocol"
	"net"
)

type (
	connectHandler        = func(ctx context.Context) bool
	authenticationHandler = func(ctx context.Context, reqHash string, serverHash string) bool
	requestHandler        = func(ctx context.Context, request protocol.Request) bool
	errorHandler          = func(ctx context.Context, err error)
)

func (s *Server) defaultConnectHandler(ctx context.Context) bool {
	return true
}

func (s *Server) defaultAuthenticationHandler(ctx context.Context, reqHash string, serverHash string) bool {
	switch reqHash {
	case sha224(serverHash):
		return true
	default:
		return false
	}
}

func sha224(password string) string {
	hash224 := sha256.New224()
	hash224.Write([]byte(password))
	sha224Hash := hash224.Sum(nil)
	return hex.EncodeToString(sha224Hash)
}

func (s *Server) defaultRequestHandler(ctx context.Context, request protocol.Request) bool {
	var remoteIP net.IP
	if request.AddressType == protocol.AddressTypeDomain {
		tcpAddr, err := net.ResolveTCPAddr("tcp", request.DescriptionAddress)
		if err != nil {
			s.logger.Errorln(err)
			return false
		}
		remoteIP = tcpAddr.IP
	} else {
		remoteIP = net.ParseIP(request.DescriptionAddress)
	}
	if remoteIP.IsLoopback() || remoteIP.IsLinkLocalUnicast() || remoteIP.IsLinkLocalMulticast() || remoteIP.IsPrivate() {
		return false
	}
	return true
}

func (s *Server) defaultErrorHandler(ctx context.Context, err error) {
	s.logger.Errorln(err)
}
