package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ProxyNode 表示一个代理节点
type ProxyNode struct {
	ID          string    `json:"id"`
	Address     string    `json:"address"`
	Type        string    `json:"type"` 
	Status      string    `json:"status"`
	LastSeen    time.Time `json:"lastSeen"`
	Connections int       `json:"connections"`
	BytesSent   int64     `json:"bytesSent"`
	BytesRecv   int64     `json:"bytesRecv"`
}

type IPWhitelistEntry struct {
	IP          string    `json:"ip"`
	Description string    `json:"description"`
	LastSeen    time.Time `json:"lastSeen"`
	MaxConns    int       `json:"maxConns"`
	CurrConns   int       `json:"currConns"`
}

// 统计信息
type Stats struct {
	ActiveConnections int
	TotalConnections  int
	BytesSent         int64
	BytesReceived     int64
	mutex             sync.Mutex
}

// 配置消息处理结构
type ConfigMessage struct {
	NodePort string `json:"nodePort"` 
}

// 代理客户端配置
type ProxyClientConfig struct {
	ID        string `json:"id"`
	ServerURL string `json:"serverUrl"` 
	HttpAddr  string `json:"httpAddr"`  
	LastSeen  string `json:"lastSeen"`
}

// 代理客户端
type ProxyClient struct {
	ID               string
	ServerURL        string
	HttpAddr         string
	Stats            Stats
	UpdateInterval   time.Duration
	httpServer       *http.Server
	wsConn           *websocket.Conn             // WebSocket连接
	wsConnMutex      sync.Mutex                  // WebSocket连接锁
	whitelistCache   map[string]IPWhitelistEntry // 本地白名单缓存
	cacheMutex       sync.RWMutex                // 缓存读写锁
	whitelistEnabled bool                        // 本地缓存的白名单开关状态
	configFilePath   string                      // 配置文件路径
}

func NewProxyClient(id, serverURL, httpAddr string, updateInterval time.Duration) *ProxyClient {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("无法获取可执行文件路径: %v, 使用当前目录", err)
		exePath = "."
	}
	exeDir := filepath.Dir(exePath)
	configFilePath := filepath.Join(exeDir, "proxy_client.json")

	// 创建新客户端
	client := &ProxyClient{
		ID:               id,
		ServerURL:        serverURL,
		HttpAddr:         httpAddr,
		UpdateInterval:   updateInterval,
		whitelistCache:   make(map[string]IPWhitelistEntry),
		whitelistEnabled: false,
		configFilePath:   configFilePath,
	}

	// 尝试加载配置文件
	if config := client.loadConfig(); config != nil {
		// 如果提供的ID为空，则使用配置文件中的ID
		if id == "" {
			client.ID = config.ID
			log.Printf("从配置文件加载节点ID: %s", client.ID)
		}
	}

	// 保存当前配置
	client.saveConfig()

	return client
}

// 启动代理服务
func (pc *ProxyClient) Start() error {
	// 启动WebSocket连接
	if err := pc.connectWebSocket(); err != nil {
		return fmt.Errorf("无法连接WebSocket: %v", err)
	}

	// 启动HTTP代理
	if pc.HttpAddr != "" {
		go pc.startHTTPProxy()
	}

	// 处理WebSocket消息
	go pc.handleWebSocket()

	return nil
}

// 连接WebSocket
func (pc *ProxyClient) connectWebSocket() error {
	// 获取本机IP地址
	localIP, err := getLocalIP()
	if err != nil {
		log.Printf("获取本地IP失败: %v, 使用配置的地址", err)
		// 如果获取失败，继续使用配置的地址
	} else {
		// 组合IP和端口
		_, port, err := net.SplitHostPort(pc.HttpAddr)
		if err == nil && port != "" {
			pc.HttpAddr = localIP + ":" + port
			log.Printf("使用本地IP和端口: %s", pc.HttpAddr)
		} else {
			log.Printf("无法解析端口，使用配置的地址: %s", pc.HttpAddr)
		}
	}

	// 将HTTP URL转换为WebSocket URL
	wsURL := strings.Replace(pc.ServerURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = fmt.Sprintf("%s/api/ws?id=%s&addr=%s", wsURL, url.QueryEscape(pc.ID), url.QueryEscape(pc.HttpAddr))

	log.Printf("连接WebSocket: %s", wsURL)

	// 建立WebSocket连接
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}

	pc.wsConnMutex.Lock()
	pc.wsConn = conn
	pc.wsConnMutex.Unlock()

	log.Println("WebSocket连接已建立")

	// 发送初始状态
	return pc.sendStatsUpdate()
}

// 重启HTTP代理服务器
func (pc *ProxyClient) restartHTTPServer() {
	// 如果已有HTTP服务器在运行，先关闭它
	if pc.httpServer != nil {
		log.Printf("关闭当前HTTP代理服务器...")
		// 使用一个独立的goroutine优雅地关闭服务器
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := pc.httpServer.Shutdown(ctx); err != nil {
				log.Printf("关闭HTTP服务器失败: %v", err)
			}
			log.Printf("HTTP服务器已关闭")
		}()
	}
	
	// 重新启动HTTP代理服务
	go pc.startHTTPProxy()
}

// 启动HTTP代理服务
func (pc *ProxyClient) startHTTPProxy() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

		if !pc.checkAccess(clientIP) {
			http.Error(w, "访问被拒绝", http.StatusForbidden)
			return
		}

		if r.Method == http.MethodConnect {
			pc.handleHTTPS(w, r)
		} else {
			pc.handleHTTP(w, r)
		}
	})

	pc.httpServer = &http.Server{
		Addr:    pc.HttpAddr,
		Handler: handler,
	}

	log.Printf("HTTP代理服务器启动在 %s\n", pc.HttpAddr)
	if err := pc.httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Printf("HTTP代理服务器错误: %v\n", err)
	}
}

// 处理HTTP请求
func (pc *ProxyClient) handleHTTP(w http.ResponseWriter, r *http.Request) {
	pc.Stats.mutex.Lock()
	pc.Stats.ActiveConnections++
	pc.Stats.TotalConnections++
	pc.Stats.mutex.Unlock()

	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	defer func() {
		pc.Stats.mutex.Lock()
		pc.Stats.ActiveConnections--
		pc.Stats.mutex.Unlock()

		// 减少白名单IP的连接计数
		pc.releaseConnection(clientIP)
	}()

	// 提取域名
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	// 如果包含端口，只保留域名部分
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}

	// 删除一些不需要的头
	r.RequestURI = ""
	r.Header.Del("Proxy-Connection")

	// 创建一个新的HTTP请求
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// 复制响应头
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 复制响应体
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Printf("复制响应体错误: %v\n", err)
		return
	}

	pc.Stats.mutex.Lock()
	pc.Stats.BytesReceived += n
	pc.Stats.mutex.Unlock()

	// 向服务器报告域名访问统计
	// 我们假设发送的数据量为请求头的大小（粗略估计）
	requestSize := int64(estimateRequestSize(r))
	go pc.reportDomainStats(host, requestSize, n)
}

// 处理HTTPS请求
func (pc *ProxyClient) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	pc.Stats.mutex.Lock()
	pc.Stats.ActiveConnections++
	pc.Stats.TotalConnections++
	pc.Stats.mutex.Unlock()

	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	defer func() {
		pc.Stats.mutex.Lock()
		pc.Stats.ActiveConnections--
		pc.Stats.mutex.Unlock()

		// 减少白名单IP的连接计数
		pc.releaseConnection(clientIP)
	}()

	targetAddr := r.Host
	// 提取域名（去掉端口部分）
	host := targetAddr
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}

	if !strings.Contains(targetAddr, ":") {
		targetAddr = targetAddr + ":443"
	}

	// 连接目标服务器
	targetConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer targetConn.Close()

	// 通知客户端连接已经准备好
	w.WriteHeader(http.StatusOK)

	// 获取底层的TCP连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持Hijacking", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	// 用于记录传输的字节数
	var bytesSent, bytesRecv int64

	// 双向复制数据
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(targetConn, clientConn)
		pc.Stats.mutex.Lock()
		pc.Stats.BytesSent += n
		pc.Stats.mutex.Unlock()
		bytesSent = n
	}()

	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientConn, targetConn)
		pc.Stats.mutex.Lock()
		pc.Stats.BytesReceived += n
		pc.Stats.mutex.Unlock()
		bytesRecv = n
	}()

	wg.Wait()

	// 向服务器报告域名访问统计
	go pc.reportDomainStats(host, bytesSent, bytesRecv)
}

// 跟踪连接，用于统计
type TrackConn struct {
	net.Conn
	pc *ProxyClient
}

func (tc *TrackConn) Read(b []byte) (int, error) {
	n, err := tc.Conn.Read(b)
	if n > 0 {
		tc.pc.Stats.mutex.Lock()
		tc.pc.Stats.BytesReceived += int64(n)
		tc.pc.Stats.mutex.Unlock()
	}
	return n, err
}

func (tc *TrackConn) Write(b []byte) (int, error) {
	n, err := tc.Conn.Write(b)
	if n > 0 {
		tc.pc.Stats.mutex.Lock()
		tc.pc.Stats.BytesSent += int64(n)
		tc.pc.Stats.mutex.Unlock()
	}
	return n, err
}

func (tc *TrackConn) Close() error {
	tc.pc.Stats.mutex.Lock()
	tc.pc.Stats.ActiveConnections--
	tc.pc.Stats.mutex.Unlock()
	return tc.Conn.Close()
}

// 检查客户端是否有权访问代理
func (pc *ProxyClient) checkAccess(clientIP string) bool {
	pc.cacheMutex.RLock()
	defer pc.cacheMutex.RUnlock()

	// 如果白名单未启用，则允许所有访问
	if !pc.whitelistEnabled {
		return true
	}

	// 检查IP是否在白名单中
	entry, exists := pc.whitelistCache[clientIP]
	if !exists {
		log.Printf("IP %s 不在白名单中，访问被拒绝", clientIP)
		return false
	}

	// 检查连接数限制
	if entry.MaxConns > 0 && entry.CurrConns >= entry.MaxConns {
		log.Printf("IP %s 达到最大连接数限制 (%d)，访问被拒绝", clientIP, entry.MaxConns)
		return false
	}

	// 增加连接计数
	pc.cacheMutex.RUnlock()
	pc.cacheMutex.Lock()

	// 再次检查，以防在解锁和重新锁定期间发生变化
	entry, stillExists := pc.whitelistCache[clientIP]
	if stillExists {
		entry.CurrConns++
		entry.LastSeen = time.Now()
		pc.whitelistCache[clientIP] = entry

		// 通过WebSocket发送连接计数更新
		go func(ip string, count int) {
			msg := struct {
				IP        string `json:"ip"`
				CurrConns int    `json:"currConns"`
			}{
				IP:        ip,
				CurrConns: count,
			}

			data, err := json.Marshal(msg)
			if err == nil {
				pc.wsConnMutex.Lock()
				defer pc.wsConnMutex.Unlock()
				if pc.wsConn != nil {
					wsMsg := WebSocketMessage{
						Type: "connection_update",
						Data: json.RawMessage(data),
					}
					msgData, _ := json.Marshal(wsMsg)
					pc.wsConn.WriteMessage(websocket.TextMessage, msgData)
				}
			}
		}(clientIP, entry.CurrConns)
	}

	pc.cacheMutex.Unlock()
	pc.cacheMutex.RLock()

	return stillExists
}

// 添加心跳检测确保WebSocket连接活跃
func (pc *ProxyClient) startHeartbeat() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// 最后一次成功的心跳时间
	lastSuccessfulHeartbeat := time.Now()

	for {
		<-ticker.C

		// 检查上次成功心跳时间，如果超过60秒没有成功的心跳，强制重连
		if time.Since(lastSuccessfulHeartbeat) > 60*time.Second {
			log.Println("超过60秒没有成功的心跳，强制重连WebSocket...")
			pc.wsConnMutex.Lock()
			if pc.wsConn != nil {
				pc.wsConn.Close()
				pc.wsConn = nil
			}
			pc.wsConnMutex.Unlock()

			if err := pc.reconnectWebSocket(); err != nil {
				log.Printf("WebSocket强制重连失败: %v", err)
			} else {
				lastSuccessfulHeartbeat = time.Now()
			}
			continue
		}

		pc.wsConnMutex.Lock()
		conn := pc.wsConn
		pc.wsConnMutex.Unlock()

		if conn == nil {
			// 尝试重新连接
			if err := pc.reconnectWebSocket(); err != nil {
				log.Printf("WebSocket心跳重连失败: %v", err)
			} else {
				lastSuccessfulHeartbeat = time.Now()
			}
			continue
		}

		// 发送心跳消息
		timestamp := time.Now().Format(time.RFC3339)
		msg := WebSocketMessage{
			Type: "heartbeat",
			Data: json.RawMessage(`{"timestamp":"` + timestamp + `"}`),
		}

		data, err := json.Marshal(msg)
		if err != nil {
			log.Printf("序列化心跳消息失败: %v", err)
			continue
		}

		pc.wsConnMutex.Lock()
		if pc.wsConn != nil {
			if err := pc.wsConn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("发送心跳失败: %v", err)
				pc.wsConn.Close()
				pc.wsConn = nil
				pc.wsConnMutex.Unlock()
				continue
			}
			pc.wsConnMutex.Unlock()

			// 心跳发送成功，更新时间
			lastSuccessfulHeartbeat = time.Now()

			// 顺便发送一次最新统计数据
			if err := pc.sendStatsUpdate(); err != nil {
				log.Printf("发送统计数据更新失败: %v", err)
			}
		} else {
			pc.wsConnMutex.Unlock()
		}
	}
}

// WebSocket消息类型
const (
	MsgTypeWhitelist = "whitelist"
	MsgTypeStats     = "stats"
	MsgTypeCommand   = "command"
	MsgTypeConfig    = "config"    // 配置消息
)

// WebSocket消息结构
type WebSocketMessage struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
}

// 处理WebSocket消息
func (pc *ProxyClient) handleWebSocket() {
	// 用于退避重连的时间
	backoffTime := 5 * time.Second
	maxBackoff := 60 * time.Second

	for {
		// 使用原子操作检查是否有连接，避免锁争用
		pc.wsConnMutex.Lock()
		conn := pc.wsConn
		pc.wsConnMutex.Unlock()

		if conn == nil {
			log.Println("WebSocket连接已关闭，尝试重连...")
			if err := pc.reconnectWebSocket(); err != nil {
				log.Printf("WebSocket重连失败: %v，等待%v后重试", err, backoffTime)
				time.Sleep(backoffTime)

				// 指数退避，最大到1分钟
				backoffTime = time.Duration(float64(backoffTime) * 1.5)
				if backoffTime > maxBackoff {
					backoffTime = maxBackoff
				}
				continue
			}
			// 重连成功，重置退避时间
			backoffTime = 5 * time.Second

			// 重新获取连接
			pc.wsConnMutex.Lock()
			conn = pc.wsConn
			pc.wsConnMutex.Unlock()

			// 检查重连后的连接是否成功建立
			if conn == nil {
				log.Println("重连后WebSocket连接仍为空，等待下一次尝试")
				time.Sleep(2 * time.Second)
				continue
			}
		}

		// 读取消息前再次检查连接是否有效
		if conn == nil {
			log.Println("WebSocket连接无效，跳过读取")
			time.Sleep(2 * time.Second)
			continue
		}

		// 读取消息
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket读取错误: %v", err)

			// 关闭当前连接并设为nil，准备重连
			pc.wsConnMutex.Lock()
			if pc.wsConn != nil {
				pc.wsConn.Close()
				pc.wsConn = nil
			}
			pc.wsConnMutex.Unlock()

			// 不立即重试，等待一小段时间
			time.Sleep(2 * time.Second)
			continue
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var wsMsg WebSocketMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			log.Printf("解析WebSocket消息失败: %v", err)
			continue
		}

		// 处理不同类型的消息
		switch wsMsg.Type {
		case MsgTypeWhitelist:
			pc.handleWhitelistUpdate(wsMsg.Data)
		case MsgTypeCommand:
			pc.handleCommand(wsMsg.Data)
		case MsgTypeConfig:
			pc.handleConfigUpdate(wsMsg.Data)
		}

		// 发送统计数据
		if err := pc.sendStatsUpdate(); err != nil {
			log.Printf("发送统计数据失败: %v", err)
		}
	}
}

// 处理白名单更新
func (pc *ProxyClient) handleWhitelistUpdate(data json.RawMessage) {
	var update struct {
		Enabled bool                        `json:"enabled"`
		Entries map[string]IPWhitelistEntry `json:"entries"`
	}

	if err := json.Unmarshal(data, &update); err != nil {
		log.Printf("解析白名单更新失败: %v", err)
		return
	}

	pc.cacheMutex.Lock()
	pc.whitelistEnabled = update.Enabled
	pc.whitelistCache = update.Entries
	pc.cacheMutex.Unlock()

	status := "已禁用"
	if update.Enabled {
		status = "已启用"
	}

	log.Printf("白名单已更新 - 状态: %s, 条目数: %d", status, len(update.Entries))
}

// 处理配置更新
func (pc *ProxyClient) handleConfigUpdate(data json.RawMessage) {
	var config ConfigMessage
	
	if err := json.Unmarshal(data, &config); err != nil {
		log.Printf("解析配置更新失败: %v", err)
		return
	}
	
	// 只有当端口有变化时才更新
	if config.NodePort != "" {
		currentIP, _, _ := net.SplitHostPort(pc.HttpAddr)
		if currentIP == "" {
			currentIP = "0.0.0.0" // 默认绑定所有接口
		}
		
		newAddr := currentIP + ":" + config.NodePort
		
		// 如果地址有变化，需要重启HTTP服务器
		if newAddr != pc.HttpAddr {
			log.Printf("端口更新：从 %s 更改为 %s", pc.HttpAddr, newAddr)
			
			// 更新地址
			pc.HttpAddr = newAddr
			
			// 保存配置
			pc.saveConfig()
			
			// 重启HTTP服务器
			pc.restartHTTPServer()
			
			log.Printf("已使用新端口 %s 重启HTTP代理服务器", config.NodePort)
		}
	}
}

// 处理服务端命令
func (pc *ProxyClient) handleCommand(data json.RawMessage) {
	var cmd struct {
		Action string `json:"action"`
		Param  string `json:"param,omitempty"`
	}

	if err := json.Unmarshal(data, &cmd); err != nil {
		log.Printf("解析命令失败: %v", err)
		return
	}

	log.Printf("收到命令: %s", cmd.Action)

	switch cmd.Action {
	case "restart":
		// 处理重启命令
		log.Println("重启代理服务...")
		// 实际重启逻辑可以根据需要添加
	case "shutdown":
		// 处理关闭命令
		log.Println("收到关闭命令，正在退出...")
		pc.Stop()
		os.Exit(0)
	}
}

// 发送统计数据更新
func (pc *ProxyClient) sendStatsUpdate() error {
	pc.wsConnMutex.Lock()
	conn := pc.wsConn
	pc.wsConnMutex.Unlock()

	if conn == nil {
		return fmt.Errorf("WebSocket连接未建立")
	}

	// 创建统计数据
	node := ProxyNode{
		ID:          pc.ID,
		Address:     pc.HttpAddr,
		Type:        "http",
		Status:      "active",
		LastSeen:    time.Now(),
		Connections: pc.Stats.ActiveConnections,
		BytesSent:   pc.Stats.BytesSent,
		BytesRecv:   pc.Stats.BytesReceived,
	}

	statsData, err := json.Marshal(node)
	if err != nil {
		return err
	}

	msg := WebSocketMessage{
		Type: MsgTypeStats,
		Data: json.RawMessage(statsData),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return conn.WriteMessage(websocket.TextMessage, data)
}

// 获取本地IP地址
func getLocalIP() (string, error) {
	// 尝试连接一个公共IP来获取本地地址，这样可以得到真实的出口IP
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		// 备选方法
		return getLocalIPAlternative()
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

// 获取本地IP备选方法
func getLocalIPAlternative() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}
	return "", fmt.Errorf("没有找到合适的IPv4地址")
}

// 重新连接WebSocket
func (pc *ProxyClient) reconnectWebSocket() error {
	pc.wsConnMutex.Lock()

	// 确保旧连接被正确关闭
	if pc.wsConn != nil {
		pc.wsConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "客户端主动重连"))
		pc.wsConn.Close()
		pc.wsConn = nil
	}
	pc.wsConnMutex.Unlock()

	// 尝试通知服务器这是一个重连
	// 如果服务器在线，这会让服务器释放旧连接资源
	go func() {
		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		reconnectURL := strings.Replace(pc.ServerURL, "ws://", "http://", 1)
		reconnectURL = strings.Replace(reconnectURL, "wss://", "https://", 1)
		reconnectURL = fmt.Sprintf("%s/api/reconnect", reconnectURL)

		data := map[string]string{
			"nodeId": pc.ID,
		}

		jsonData, _ := json.Marshal(data)
		resp, err := client.Post(reconnectURL, "application/json", bytes.NewBuffer(jsonData))
		if err == nil {
			resp.Body.Close()
		}
	}()

	// 在尝试重新连接之前短暂等待，确保服务器有时间处理旧连接
	time.Sleep(1 * time.Second)

	return pc.connectWebSocket()
}

// 估算HTTP请求大小
func estimateRequestSize(r *http.Request) int {
	size := 0

	// 请求行
	size += len(r.Method) + len(r.URL.Path) + len("HTTP/1.1") + 4 // 4是空格和回车换行

	// 请求头
	for name, values := range r.Header {
		for _, value := range values {
			size += len(name) + len(value) + 4 // 4是冒号、空格和回车换行
		}
	}

	// 估算请求体大小
	if r.ContentLength > 0 {
		size += int(r.ContentLength)
	}

	return size
}

// 报告域名访问统计
func (pc *ProxyClient) reportDomainStats(domain string, bytesSent, bytesRecv int64) {
	// 创建域名统计消息
	domainStat := struct {
		Domain    string `json:"domain"`
		BytesSent int64  `json:"bytesSent"`
		BytesRecv int64  `json:"bytesRecv"`
	}{
		Domain:    domain,
		BytesSent: bytesSent,
		BytesRecv: bytesRecv,
	}

	statBytes, err := json.Marshal(domainStat)
	if err != nil {
		log.Printf("序列化域名统计数据失败: %v\n", err)
		return
	}

	// 创建WebSocket消息
	msg := WebSocketMessage{
		Type: "domain_update",
		Data: json.RawMessage(statBytes),
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("序列化WebSocket消息失败: %v\n", err)
		return
	}

	// 发送消息
	pc.wsConnMutex.Lock()
	defer pc.wsConnMutex.Unlock()

	if pc.wsConn != nil {
		if err := pc.wsConn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
			log.Printf("发送域名统计数据失败: %v\n", err)
		}
	}
}

// 释放连接（减少当前连接计数）
func (pc *ProxyClient) releaseConnection(clientIP string) {
	pc.cacheMutex.Lock()
	defer pc.cacheMutex.Unlock()

	if entry, exists := pc.whitelistCache[clientIP]; exists && entry.CurrConns > 0 {
		entry.CurrConns--
		pc.whitelistCache[clientIP] = entry

		// 通过WebSocket发送连接计数更新
		go func(ip string, count int) {
			msg := struct {
				IP        string `json:"ip"`
				CurrConns int    `json:"currConns"`
			}{
				IP:        ip,
				CurrConns: count,
			}

			data, err := json.Marshal(msg)
			if err == nil {
				pc.wsConnMutex.Lock()
				defer pc.wsConnMutex.Unlock()
				if pc.wsConn != nil {
					wsMsg := WebSocketMessage{
						Type: "connection_update",
						Data: json.RawMessage(data),
					}
					msgData, _ := json.Marshal(wsMsg)
					pc.wsConn.WriteMessage(websocket.TextMessage, msgData)
				}
			}
		}(clientIP, entry.CurrConns)
	}
}

// 加载客户端配置
func (pc *ProxyClient) loadConfig() *ProxyClientConfig {
	data, err := os.ReadFile(pc.configFilePath)
	if err != nil {
		// 如果文件不存在，不报错
		if !os.IsNotExist(err) {
			log.Printf("读取配置文件失败: %v", err)
		}
		return nil
	}

	var config ProxyClientConfig
	if err := json.Unmarshal(data, &config); err != nil {
		log.Printf("解析配置文件失败: %v", err)
		return nil
	}

	// 验证配置有效性
	if config.ID == "" {
		log.Println("配置文件中的节点ID为空")
		return nil
	}

	return &config
}

// 保存客户端配置
func (pc *ProxyClient) saveConfig() error {
	config := ProxyClientConfig{
		ID:        pc.ID,
		ServerURL: pc.ServerURL,
		HttpAddr:  pc.HttpAddr,
		LastSeen:  time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.Printf("序列化配置失败: %v", err)
		return err
	}

	if err := os.WriteFile(pc.configFilePath, data, 0644); err != nil {
		log.Printf("保存配置文件失败: %v", err)
		return err
	}

	log.Printf("配置文件已保存: %s", pc.configFilePath)
	return nil
}

// 停止代理服务
func (pc *ProxyClient) Stop() {
	pc.wsConnMutex.Lock()
	if pc.wsConn != nil {
		pc.wsConn.Close()
		pc.wsConn = nil
	}
	pc.wsConnMutex.Unlock()

	if pc.httpServer != nil {
		pc.httpServer.Close()
	}

	// 保存最新配置
	pc.saveConfig()
}

func main() {
	var (
		id             = flag.String("id", "", "代理节点ID (如果为空，将尝试从配置文件加载)")
		serverURL      = flag.String("server", "http://localhost:8080", "管理服务器URL")
		httpAddr       = flag.String("http", ":8081", "HTTP代理监听地址")
		updateInterval = flag.Duration("update", 30*time.Second, "更新间隔")
	)
	flag.Parse()

	// 尝试从配置文件加载节点ID（如果未指定）
	idFromFlag := *id

	// 创建代理客户端（会自动处理ID的加载/生成）
	client := NewProxyClient(idFromFlag, *serverURL, *httpAddr, *updateInterval)

	// 如果既没有通过命令行指定ID，也没有从配置加载到，则生成新ID
	if client.ID == "" {
		client.ID = fmt.Sprintf("node-%d", time.Now().Unix())
		log.Printf("生成新的节点ID: %s", client.ID)
		// 保存新生成的ID
		client.saveConfig()
	}

	// 启动代理服务
	if err := client.Start(); err != nil {
		log.Fatalf("启动代理服务失败: %v", err)
	}

	// 启动心跳检测
	go client.startHeartbeat()

	log.Printf("代理客户端 %s 已启动\n", client.ID)
	log.Printf("管理服务器: %s\n", client.ServerURL)
	log.Printf("HTTP代理地址: %s\n", client.HttpAddr)
	log.Printf("配置文件: %s\n", client.configFilePath)

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("正在关闭代理服务...")
	client.Stop()
	log.Println("代理服务已关闭")
}
