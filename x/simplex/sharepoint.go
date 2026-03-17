//go:build graph
// +build graph

package simplex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chainreactors/logs"
)

func init() {
	RegisterSimplex("sharepoint", func(addr *SimplexAddr) (Simplex, error) {
		return NewSharePointClient(addr)
	}, func(network, address string) (Simplex, error) {
		return NewSharePointServer(network, address)
	}, func(network, address string) (*SimplexAddr, error) {
		return ResolveSharePointAddr(network, address)
	})
}

const (
	DefaultSharePointInterval = 5000   // 5秒间隔
	MaxSharePointMessageSize  = 190000 // SharePoint List 列限制 ~255KB，base64 后安全上限 ~190KB
)

// randomColumnName returns a random letters-only identifier, which is always
// a valid OData field name for SharePoint Graph API $select queries.
func randomColumnName(length int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	result := make([]byte, length)
	for i := range result {
		result[i] = letters[seededRand.Intn(len(letters))]
	}
	return string(result)
}

// clientListEntry caches the client's response list created on first connection.
type clientListEntry struct {
	id   string
	name string
}

// clientListRegistry persists client response list info across reconnects,
// keyed by "tenantID/siteID/serverListID" so reconnects reuse the same list
// instead of creating a new random one every time.
var clientListRegistry sync.Map

func clientListRegistryKey(config *GraphConfig) string {
	return config.TenantID + "/" + config.SiteID + "/" + config.ListID
}

// Microsoft Graph 通用接口
type GraphConnector interface {
	Do(request *http.Request) (*http.Response, error)
	Get(apiURL string, response interface{}) error
	Post(apiURL string, body interface{}, response interface{}) (int, error)
	Delete(apiURL string) error
}

// SharePoint List 结构定义
type SharePointList struct {
	DisplayName string                 `json:"displayName"`
	Columns     []SharePointListColumn `json:"columns"`
}

type SharePointListColumn struct {
	Name string                   `json:"name"`
	Text SharePointListColumnText `json:"text"`
}

type SharePointListColumnText struct {
	AllowMultipleLines bool `json:"allowMultipleLines"`
}

type SharePointListResponse struct {
	ID string `json:"id"`
}

type SharePointListItemFields struct {
	Input     string `json:"Input,omitempty"`
	Timestamp string `json:"Timestamp,omitempty"`
	UID       string `json:"UID,omitempty"` // 客户端标识
}

type SharePointListItem struct {
	Fields SharePointListItemFields `json:"fields"`
}

type SharePointItemInfo struct {
	ID string `json:"id"`
}

// 创建标准的 SharePoint List 列定义
func createStandardColumns(dataColumn string) []SharePointListColumn {
	return []SharePointListColumn{
		{
			Name: dataColumn,
			Text: SharePointListColumnText{AllowMultipleLines: true},
		},
		{
			Name: "Timestamp",
			Text: SharePointListColumnText{},
		},
		{
			Name: "UID",
			Text: SharePointListColumnText{},
		},
	}
}

// 创建 ListItem 用于发送数据
func createListItem(data []byte, uid string, dataColumn string) interface{} {
	encodedData := base64.StdEncoding.EncodeToString(data)
	fields := map[string]string{
		dataColumn:  encodedData,
		"Timestamp": time.Now().Format(time.RFC3339),
		"UID":       uid,
	}
	return map[string]interface{}{"fields": fields}
}

// 处理接收到的 item 数据
func processReceivedItem(item map[string]interface{}, dataColumn string) (*SimplexPackets, error) {
	input, _ := item[dataColumn].(string)
	if input == "" {
		return nil, nil
	}

	data, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return nil, fmt.Errorf("failed to decode input data: %v", err)
	}

	packets, err := ParseSimplexPackets(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse simplex packets: %v", err)
	}

	return packets, nil
}

func ResolveSharePointAddr(network, address string) (*SimplexAddr, error) {
	return ResolveGraphAddr(address, DefaultSharePointInterval, MaxSharePointMessageSize,
		func(u *url.URL) string {
			name := strings.TrimPrefix(u.Path, "/")
			if name == "" {
				name = randomString(8)
			}
			return name
		})
}

// trackedSimplexBuffer wraps SimplexBuffer with last-active tracking for session cleanup.
type trackedSimplexBuffer struct {
	*SimplexBuffer
	lastActive time.Time
}

func newTrackedSimplexBuffer(addr *SimplexAddr) *trackedSimplexBuffer {
	return &trackedSimplexBuffer{
		SimplexBuffer: NewSimplexBuffer(addr),
		lastActive:    time.Now(),
	}
}

func (tb *trackedSimplexBuffer) Touch() {
	tb.lastActive = time.Now()
}

func (tb *trackedSimplexBuffer) LastActive() time.Time {
	return tb.lastActive
}

// SharePointServer 实现服务端
type SharePointServer struct {
	GraphBase          // 嵌入：提供 addr, config, connector, ctx, cancel, Addr(), Close()
	buffers   sync.Map
	cur       int // 当前读取的 item ID，只有成功时才递增
}

// SharePointClient 实现客户端
type SharePointClient struct {
	GraphBase                 // 嵌入：提供 addr, config, connector, ctx, cancel, Addr(), Close()
	buffer         *SimplexBuffer
	serverListID   string // 服务端的 List ID，用于发送数据
	clientListID   string // 客户端的 List ID，用于接收数据
	clientListName string // 客户端的 List Display Name，用于发送时标识
	cur            int    // 当前读取的 item ID，只有成功时才递增
}

func NewSharePointServer(network, address string) (*SharePointServer, error) {
	addr, err := ResolveSharePointAddr(network, address)
	if err != nil {
		return nil, err
	}
	addr.Scheme = "simplex+" + addr.Scheme

	// DataColumn 预处理（在 NewGraphBase 之前，因为需要验证）
	dataCol := getOption(addr, "data_col")
	listID := getOption(addr, "list_id")
	if dataCol == "" {
		if listID != "" {
			return nil, fmt.Errorf("missing required data_col parameter for resume (list_id=%s)", listID)
		}
		dataCol = randomColumnName(8)
	}

	base, err := NewGraphBase(addr, func(c *GraphConfig) {
		c.ListName = addr.id
		c.ListID = listID
		c.DataColumn = dataCol
	})
	if err != nil {
		return nil, err
	}

	server := &SharePointServer{GraphBase: *base}

	// 将 DataColumn 存储到 options 中，以便客户端可以使用（与 list_id 同级）
	addr.options.Set("data_col", base.config.DataColumn)

	// 服务端负责创建 List（如果还没有 ListID）
	if server.config.ListID == "" {
		newListID, err := server.createSharePointList()
		if err != nil {
			return nil, err
		}
		server.config.ListID = newListID
		logs.Log.Infof("[SharePoint] Server created list: %s (data_col=%s)", newListID, server.config.DataColumn)

		// 将 ListID 更新到 options 中，以便客户端可以使用
		addr.options.Set("list_id", newListID)
	} else {
		// 如果已经有 ListID，通过 getLastItemID 恢复 cur 值。
		// 不能用 cur=0：全新 server 没有 ARQ 状态，重新处理旧 login 会创建
		// 幽灵 session，向 client 发送基于旧数据的响应，破坏协议状态。
		// 同时异步发送 RST 通知已连接的 client 立即重连，避免等待 ARQ 超时。
		lastItemID, err := getLastItemID(server.connector, server.config.SiteID, server.config.ListID)
		if err != nil {
			logs.Log.Warnf("[SharePoint] Failed to get last item ID, starting from 0: %v", err)
			server.cur = 0
		} else {
			server.cur = lastItemID
			logs.Log.Infof("[SharePoint] Server resumed from cur=%d (from getLastItemID)", server.cur)
		}
		go server.notifyClientsOfRestart()
	}

	go server.polling()

	// 启动会话清理 (10分钟超时，SharePoint 轮询间隔更长)
	StartSessionCleanup(base.ctx, &server.buffers, 1*time.Minute, 10*time.Minute, "[SharePoint]")

	return server, nil
}

func NewSharePointClient(addr *SimplexAddr) (*SharePointClient, error) {
	dataCol := getOption(addr, "data_col")
	if dataCol == "" {
		return nil, fmt.Errorf("missing required data_col parameter")
	}
	listID := getOption(addr, "list_id")
	if listID == "" {
		return nil, fmt.Errorf("missing required list_id parameter")
	}

	base, err := NewGraphBase(addr, func(c *GraphConfig) {
		c.ListName = addr.id
		c.ListID = listID
		c.DataColumn = dataCol
	})
	if err != nil {
		return nil, err
	}

	// Check if we have a cached client response list for this server connection.
	// If so, reuse it to avoid creating a new random list on every reconnect.
	regKey := clientListRegistryKey(base.config)
	var clientListName string
	var hasCached bool
	var cached clientListEntry
	if v, ok := clientListRegistry.Load(regKey); ok {
		cached = v.(clientListEntry)
		clientListName = cached.name
		hasCached = true
	} else {
		clientListName = fmt.Sprintf("client-%s", randomString(8))
	}

	client := &SharePointClient{
		GraphBase:      *base,
		buffer:         NewSimplexBuffer(addr),
		serverListID:   base.config.ListID,
		clientListName: clientListName,
		cur:            0,
	}

	var clientListID string
	if hasCached {
		// Reconnect: reuse the existing response list, resume from last item
		clientListID = cached.id
		logs.Log.Infof("[SharePoint] Reconnecting: reusing client list %s (%s)", clientListName, clientListID)
		lastItemID, err := getLastItemID(client.connector, client.config.SiteID, clientListID)
		if err != nil {
			logs.Log.Warnf("[SharePoint] Failed to get last item ID for client list: %v, starting from 0", err)
		} else {
			client.cur = lastItemID
		}
	} else {
		// First connection: create a new client response list
		var err error
		clientListID, err = client.createClientResponseList()
		if err != nil {
			client.Close()
			return nil, err
		}
		logs.Log.Infof("[SharePoint] Client created response list: %s", clientListID)

		// Wait for the list to be queryable (SharePoint eventual consistency)
		logs.Log.Debugf("[SharePoint] Waiting for list to be queryable...")
		var verifyErr error
		for attempt := 0; attempt < 10; attempt++ {
			_, verifyErr = findListByName(client.connector, client.config.SiteID, client.clientListName)
			if verifyErr == nil {
				logs.Log.Debugf("[SharePoint] List verified queryable after %d attempts", attempt+1)
				break
			}
			if attempt < 9 {
				time.Sleep(2 * time.Second)
			}
		}
		if verifyErr != nil {
			logs.Log.Warnf("[SharePoint] List may not be immediately queryable: %v, continuing anyway", verifyErr)
		}

		// Store in registry for future reconnects
		clientListRegistry.Store(regKey, clientListEntry{id: clientListID, name: client.clientListName})
	}

	client.clientListID = clientListID

	return client, nil
}

// SharePoint List 创建 (仅服务端)
func (s *SharePointServer) createSharePointList() (string, error) {
	list := SharePointList{
		DisplayName: s.config.ListName,
		Columns:     createStandardColumns(s.config.DataColumn),
	}

	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists", s.config.SiteID)
	var response SharePointListResponse

	statusCode, err := s.connector.Post(apiURL, list, &response)
	if err != nil {
		return "", err
	}

	// 409 冲突状态表示 List 已存在
	if statusCode == http.StatusConflict {
		return s.config.ListName, nil
	}

	// 其他非 201 状态码都是错误
	if statusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create SharePoint list, status: %d", statusCode)
	}

	return response.ID, nil
}

func (s *SharePointServer) polling() {
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.pollSharePointList()
		}
	}
}

func (s *SharePointServer) pollSharePointList() {
	// 如果没有 ListID，无法轮询
	if s.config.ListID == "" {
		logs.Log.Warnf("[SharePoint] Server has no ListID, cannot poll")
		return
	}

	// 连续读取所有可用的 item，而不是每次 poll 只读一个。
	// 这大幅提升了吞吐量：如果客户端在两次 poll 之间发送了 N 个 item，
	// 服务端可以在一个 poll 周期内全部处理，而不是需要 N 个周期。
	for {
		if err := s.pollOneItem(); err != nil {
			break
		}
	}
}

// pollOneItem 尝试读取下一个 item (cur+1)。成功返回 nil，无数据或出错返回 error。
func (s *SharePointServer) pollOneItem() error {
	itemURL := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items/%d?$expand=fields($select=%s,Timestamp,UID)",
		s.config.SiteID, s.config.ListID, s.cur+1, s.config.DataColumn)
	itemReq, err := http.NewRequest(http.MethodGet, itemURL, nil)
	if err != nil {
		logs.Log.Errorf("[SharePoint] Failed to create item request: %v", err)
		return err
	}

	itemResp, err := s.connector.Do(itemReq)
	if err != nil {
		logs.Log.Errorf("[SharePoint] Failed to get item: %v", err)
		return err
	}
	defer itemResp.Body.Close()

	if itemResp.StatusCode == http.StatusNotFound {
		// 404 表示没有新数据，停止继续读取
		return fmt.Errorf("no more items")
	}

	if itemResp.StatusCode != http.StatusOK {
		logs.Log.Warnf("[SharePoint] Unexpected status code %d, will retry", itemResp.StatusCode)
		return fmt.Errorf("unexpected status %d", itemResp.StatusCode)
	}

	var rawItem struct {
		Fields map[string]interface{} `json:"fields"`
	}
	err = json.NewDecoder(itemResp.Body).Decode(&rawItem)
	if err != nil {
		logs.Log.Errorf("[SharePoint] Failed to decode item: %v", err)
		return err
	}

	// item 已从 API 成功读取，立即递增 cur
	s.cur++

	// 后续处理（查找 client list 等）失败不影响 cur
	input, _ := rawItem.Fields[s.config.DataColumn].(string)
	uid, _ := rawItem.Fields["UID"].(string)

	// 检查数据是否有效
	if input == "" || uid == "" {
		return nil
	}

	packets, err := processReceivedItem(rawItem.Fields, s.config.DataColumn)
	if err != nil {
		logs.Log.Errorf("[SharePoint] %v", err)
		return nil // cur 已递增，继续处理下一个
	}

	if packets == nil {
		return nil
	}

	clientDisplayName := uid

	// 先从 buffers 中查询是否已经存在这个客户端
	var buffer *trackedSimplexBuffer

	if existingBuf, exists := s.buffers.Load(clientDisplayName); exists {
		// 客户端已存在，直接复用
		buffer = existingBuf.(*trackedSimplexBuffer)
		buffer.Touch()
	} else {
		// 新客户端，生成地址并查询其 List ID
		clientAddr := generateAddrFromPath(clientDisplayName, s.addr)

		// 查询并缓存客户端的 List ID，重试以应对 SharePoint eventual consistency
		var listID string
		var findErr error
		for attempt := 0; attempt < 5; attempt++ {
			listID, findErr = findListByName(s.connector, s.config.SiteID, clientDisplayName)
			if findErr == nil {
				break
			}
			if attempt < 4 {
				logs.Log.Debugf("[SharePoint] findList(%s) attempt %d failed: %v, retrying...", clientDisplayName, attempt+1, findErr)
				time.Sleep(3 * time.Second)
			}
		}
		if findErr != nil {
			logs.Log.Warnf("[SharePoint] Failed to find client list %s after retries: %v, skipping item", clientDisplayName, findErr)
			return nil // cur 已递增，继续处理下一个
		}
		clientAddr.options.Set("list_id", listID)
		logs.Log.Infof("[SharePoint] New client connected: %s", clientDisplayName)

		// 创建新的缓冲区
		buffer = newTrackedSimplexBuffer(clientAddr)
		s.buffers.Store(clientDisplayName, buffer)
	}

	// 将数据写入缓冲区
	for _, pkt := range packets.Packets {
		buffer.PutPacket(pkt)
	}

	return nil
}

func (s *SharePointServer) Receive() (*SimplexPacket, *SimplexAddr, error) {
	return GraphServerReceive(s.ctx, &s.buffers,
		func(v interface{}) *SimplexBuffer { return v.(*trackedSimplexBuffer).SimplexBuffer },
		func(v interface{}) *SimplexAddr { return v.(*trackedSimplexBuffer).Addr() },
	)
}

func (s *SharePointServer) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	if pkts == nil || pkts.Size() == 0 {
		return 0, nil
	}

	// 从 addr.options 中获取客户端的 List ID
	clientListID := addr.options.Get("list_id")
	if clientListID == "" {
		return 0, fmt.Errorf("client list ID not found for %s", addr.Path)
	}

	data := pkts.Marshal()
	listItem := createListItem(data, addr.Path, s.config.DataColumn)

	// 使用 addr.options 中的 List ID 发送到客户端的 List
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items", s.config.SiteID, clientListID)
	statusCode, err := s.connector.Post(apiURL, listItem, nil)
	if err != nil {
		return 0, err
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return 0, fmt.Errorf("send failed with status: %d", statusCode)
	}

	return len(data), nil
}

func (c *SharePointClient) Receive() (*SimplexPacket, *SimplexAddr, error) {
	select {
	case <-c.ctx.Done():
		return nil, nil, io.ErrClosedPipe
	default:
		pkt, err := c.buffer.GetPacket()
		if err != nil || pkt == nil {
			c.pollForResponse()
			pkt, err = c.buffer.GetPacket()
		}
		return pkt, c.addr, err
	}
}

func (c *SharePointClient) pollForResponse() {
	// 连续读取所有可用的 item，而不是每次只读一个。
	// 服务端可能在两次 poll 之间发送了多个 item，一次性读完提升吞吐量。
	for {
		if err := c.pollOneResponse(); err != nil {
			break
		}
	}
}

// pollOneResponse 尝试读取下一个响应 item (cur+1)。成功返回 nil，无数据或出错返回 error。
func (c *SharePointClient) pollOneResponse() error {
	itemURL := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items/%d?$expand=fields($select=%s,Timestamp,UID)",
		c.config.SiteID, c.clientListID, c.cur+1, c.config.DataColumn)
	itemReq, err := http.NewRequest(http.MethodGet, itemURL, nil)
	if err != nil {
		return err
	}

	itemResp, err := c.connector.Do(itemReq)
	if err != nil {
		return err
	}
	defer itemResp.Body.Close()

	if itemResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no more items")
	}

	if itemResp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", itemResp.StatusCode)
	}

	var rawItem struct {
		Fields map[string]interface{} `json:"fields"`
	}
	err = json.NewDecoder(itemResp.Body).Decode(&rawItem)
	if err != nil {
		return err
	}

	// 成功获取数据，递增 cur
	c.cur++

	packets, err := processReceivedItem(rawItem.Fields, c.config.DataColumn)
	if err != nil {
		return nil // cur 已递增，继续处理下一个
	}

	if packets != nil {
		for _, pkt := range packets.Packets {
			c.buffer.PutPacket(pkt)
		}
	}

	return nil
}



func (c *SharePointClient) Send(pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	if pkts == nil || pkts.Size() == 0 {
		return 0, nil
	}

	data := pkts.Marshal()
	listItem := createListItem(data, c.clientListName, c.config.DataColumn)

	// 发送数据到服务端的 List
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items", c.config.SiteID, c.serverListID)
	statusCode, err := c.connector.Post(apiURL, listItem, nil)
	if err != nil {
		return 0, err
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return 0, fmt.Errorf("send failed with status: %d", statusCode)
	}

	return len(data), nil
}

// 客户端创建响应 List
func (c *SharePointClient) createClientResponseList() (string, error) {
	list := SharePointList{
		DisplayName: c.clientListName,
		Columns:     createStandardColumns(c.config.DataColumn),
	}

	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists", c.config.SiteID)
	var response SharePointListResponse

	statusCode, err := c.connector.Post(apiURL, list, &response)
	if err != nil {
		return "", err
	}

	// 处理不同的状态码
	if statusCode == http.StatusOK || statusCode == http.StatusConflict {
		// 200 或 409 表示 List 已存在，重新生成名称并重试
		c.clientListName = fmt.Sprintf("client-%s", randomString(8))
		return c.createClientResponseList() // 递归调用重新创建
	}

	// 201 表示创建成功
	if statusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create SharePoint client list, status: %d", statusCode)
	}

	return response.ID, nil
}

// ListID 返回服务端 List 的 ID（创建后填充，可传给客户端连接）
func (s *SharePointServer) ListID() string { return s.config.ListID }

// DataColumn 返回服务端使用的数据列名（可传给客户端连接）
func (s *SharePointServer) DataColumn() string { return s.config.DataColumn }

// Cur 返回服务端当前已处理的最后一个 item ID
func (s *SharePointServer) Cur() int { return s.cur }

// notifyClientsOfRestart 扫描 server list 最近几个 items 提取 client UID，
// 向每个 client 的 response list 写入 RST 包，触发 client 立即重连。
func (s *SharePointServer) notifyClientsOfRestart() {
	seenUIDs := map[string]bool{}
	for i := s.cur; i > 0 && i > s.cur-5; i-- {
		uid := s.fetchItemUID(i)
		if uid != "" && !seenUIDs[uid] {
			seenUIDs[uid] = true
		}
	}

	for uid := range seenUIDs {
		listID, err := findListByName(s.connector, s.config.SiteID, uid)
		if err != nil {
			logs.Log.Debugf("[SharePoint] RST: cannot find list for client %s: %v", uid, err)
			continue
		}

		rstPkt := NewSimplexPacket(SimplexPacketTypeRST, nil)
		pkts := &SimplexPackets{Packets: []*SimplexPacket{rstPkt}}
		data := pkts.Marshal()
		listItem := createListItem(data, uid, s.config.DataColumn)
		apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items",
			s.config.SiteID, listID)
		_, err = s.connector.Post(apiURL, listItem, nil)
		if err != nil {
			logs.Log.Warnf("[SharePoint] RST: failed to send to client %s: %v", uid, err)
			continue
		}
		logs.Log.Infof("[SharePoint] Sent RST to client %s", uid)
	}
}

// fetchItemUID reads the UID field from a single item in the server list.
func (s *SharePointServer) fetchItemUID(itemID int) string {
	apiURL := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items/%d?$expand=fields($select=UID)",
		s.config.SiteID, s.config.ListID, itemID)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return ""
	}
	resp, err := s.connector.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var rawItem struct {
		Fields map[string]interface{} `json:"fields"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawItem); err != nil {
		return ""
	}
	uid, _ := rawItem.Fields["UID"].(string)
	return uid
}

// ClientListName 返回客户端响应 List 的显示名称（每次重启均不同）
func (c *SharePointClient) ClientListName() string { return c.clientListName }


// findListByName 通过显示名称查找 SharePoint List，返回 List ID
func findListByName(connector GraphConnector, siteID, displayName string) (string, error) {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists?$filter=displayName+eq+'%s'", siteID, displayName)

	var response struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}

	err := connector.Get(apiURL, &response)
	if err != nil {
		return "", err
	}

	if len(response.Value) == 0 {
		return "", fmt.Errorf("list with name %s not found", displayName)
	}

	return response.Value[0].ID, nil
}

// getLastItemID 查询指定 List 的最新 item ID，用于断线续传
func getLastItemID(connector GraphConnector, siteID, listID string) (int, error) {
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s/lists/%s/items?$orderby=lastModifiedDateTime+desc&$top=1", siteID, listID)

	var response struct {
		Value []SharePointItemInfo `json:"value"`
	}

	err := connector.Get(apiURL, &response)
	if err != nil {
		return 0, err
	}

	if len(response.Value) == 0 {
		return 0, nil
	}

	lastItemID, err := strconv.Atoi(response.Value[0].ID)
	if err != nil {
		return 0, fmt.Errorf("failed to parse item ID %s: %v", response.Value[0].ID, err)
	}

	return lastItemID, nil
}

