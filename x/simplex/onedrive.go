//go:build graph
// +build graph

package simplex

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/chainreactors/logs"
)

func init() {
	RegisterSimplex("onedrive", func(addr *SimplexAddr) (Simplex, error) {
		return NewOneDriveClient(addr)
	}, func(network, address string) (Simplex, error) {
		return NewOneDriveServer(network, address)
	}, func(network, address string) (*SimplexAddr, error) {
		return ResolveOneDriveAddr(network, address)
	})
}

const (
	DefaultOneDriveInterval = 2000            // 2秒间隔
	MaxOneDriveMessageSize  = 2 * 1024 * 1024 // OneDrive 文件上传，2MB 为吞吐/延迟最优值

	// handleClient 在 idleMultiplier × polling interval 无活动后清理退出。
	// 生产: 150 × 2s = 5min; 测试 (50ms interval): 150 × 50ms = 7.5s
	oneDriveHandlerIdleMultiplier = 150
)

// driveFileInfo 文件信息 (used by Graph API ListFiles response)
type driveFileInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
}

// oneDriveStorageOps implements FileStorageOps using GraphConnector.
// GraphConnector (from graph.go) provides 429 exponential backoff + OAuth2 token refresh.
type oneDriveStorageOps struct {
	config    *GraphConfig
	connector GraphConnector
}

func newOneDriveStorageOps(config *GraphConfig, connector GraphConnector) FileStorageOps {
	return &oneDriveStorageOps{
		config:    config,
		connector: connector,
	}
}

func (op *oneDriveStorageOps) ReadFile(filePath string) ([]byte, error) {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/drive/items/root:/%s:/content", op.config.SiteID, filePath)

	request, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := op.connector.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("file not found %s: %w", filePath, os.ErrNotExist)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to pull file %s, status: %d", filePath, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (op *oneDriveStorageOps) WriteFile(filePath string, data []byte) error {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/drive/items/root:/%s:/content", op.config.SiteID, filePath)

	request, err := http.NewRequest(http.MethodPut, apiURL, strings.NewReader(string(data)))
	if err != nil {
		return err
	}

	resp, err := op.connector.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("failed to push file, status: %d", resp.StatusCode)
	}

	return nil
}

func (op *oneDriveStorageOps) DeleteFile(filePath string) error {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/drive/items/root:/%s", op.config.SiteID, filePath)
	return op.connector.Delete(apiURL)
}

func (op *oneDriveStorageOps) ListFiles(folderPath string) ([]string, error) {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/drive/items/root:/%s:/children", op.config.SiteID, folderPath)

	var response struct {
		Value []driveFileInfo `json:"value"`
	}

	err := op.connector.Get(apiURL, &response)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(response.Value))
	for _, f := range response.Value {
		names = append(names, f.Name)
	}
	return names, nil
}

func (op *oneDriveStorageOps) FileExists(path string) (bool, error) {
	_, err := op.ReadFile(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func ResolveOneDriveAddr(network, address string) (*SimplexAddr, error) {
	return ResolveGraphAddr(address, DefaultOneDriveInterval, MaxOneDriveMessageSize,
		func(u *url.URL) string {
			dir := strings.TrimPrefix(u.Path, "/")
			if dir == "" {
				if p := u.Query().Get("prefix"); p != "" {
					dir = p
				} else {
					dir = randomString(8)
				}
				u.Path = "/" + dir
			}
			return dir
		})
}

// OneDriveServer 实现服务端 — thin wrapper around FileTransportServer
type OneDriveServer struct {
	GraphBase // 嵌入：提供 addr, config, connector, ctx, cancel, Addr(), Close()
	fts       *FileTransportServer
}

// OneDriveClient 实现客户端 — thin wrapper around FileTransportClient
type OneDriveClient struct {
	GraphBase // 嵌入：提供 addr, config, connector, ctx, cancel, Addr(), Close()
	ftc       *FileTransportClient
}

func oneDriveFileTransportConfig() FileTransportConfig {
	return FileTransportConfig{
		SendSuffix:     "_send.txt",
		RecvSuffix:     "_recv.txt",
		IdleMultiplier: oneDriveHandlerIdleMultiplier,
		MaxFailures:    10,
		LogPrefix:      "[OneDrive]",
	}
}

func NewOneDriveServer(network, address string) (*OneDriveServer, error) {
	addr, err := ResolveOneDriveAddr(network, address)
	if err != nil {
		return nil, err
	}
	addr.Scheme = "simplex+" + addr.Scheme

	base, err := NewGraphBase(addr, nil)
	if err != nil {
		return nil, err
	}

	ops := newOneDriveStorageOps(base.config, base.connector)
	dirPath := addr.id

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = base.config.Interval
	cfg.MaxBodySize = base.config.MaxBodySize

	fts := NewFileTransportServer(ops, dirPath+"/", cfg, addr, base.ctx, base.cancel)
	fts.StartPolling()

	server := &OneDriveServer{
		GraphBase: *base,
		fts:       fts,
	}

	return server, nil
}

func NewOneDriveClient(addr *SimplexAddr) (*OneDriveClient, error) {
	base, err := NewGraphBase(addr, nil)
	if err != nil {
		return nil, err
	}

	clientID := getOption(addr, "client_id")
	if clientID == "" {
		clientID = randomString(8)
	}

	dirPath := addr.id
	ops := newOneDriveStorageOps(base.config, base.connector)

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = base.config.Interval
	cfg.MaxBodySize = base.config.MaxBodySize

	sendFile := fmt.Sprintf("%s/%s_send.txt", dirPath, clientID)
	recvFile := fmt.Sprintf("%s/%s_recv.txt", dirPath, clientID)

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(ops, cfg, buffer, sendFile, recvFile, addr, base.ctx, base.cancel)
	ftc.StartMonitoring()

	logs.Log.Infof("[OneDrive] Client created with clientID=%s", clientID)

	client := &OneDriveClient{
		GraphBase: *base,
		ftc:       ftc,
	}

	return client, nil
}

func (s *OneDriveServer) Receive() (*SimplexPacket, *SimplexAddr, error) {
	return s.fts.Receive()
}

func (s *OneDriveServer) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	return s.fts.Send(pkts, addr)
}

func (s *OneDriveServer) Close() error {
	s.fts.CloseAll()
	return s.GraphBase.Close()
}

func (c *OneDriveClient) Receive() (*SimplexPacket, *SimplexAddr, error) {
	return c.ftc.Receive()
}

func (c *OneDriveClient) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	return c.ftc.Send(pkts, addr)
}
