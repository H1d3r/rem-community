# Rem WASM Client

这是一个基于WebAssembly的rem客户端实现，允许在浏览器中直接建立rem连接。

## 特性

- 基于WebSocket的传输层实现
- 使用专业的wasmws库提供稳定的WebSocket连接
- 与服务器端WebSocket listener兼容
- 暴露类似`RemDial`的JavaScript API
- 支持完整的rem命令行参数
- 浏览器中的实时连接管理
- 支持二进制数据传输和大消息流式处理

## 架构

### 传输层实现
- **Listener**: 使用WebSocket实现，与`protocol/tunnel/websocket`保持一致
- **Dialer**: 使用wasmws库在WASM环境中建立WebSocket连接
- **协议**: 完全兼容rem的WebSocket传输协议
- **库依赖**: 基于成熟的wasmws WebSocket库，提供更好的稳定性和性能

### JavaScript API
- `RemDial(cmdline)`: 建立rem连接，参数为命令行字符串
- `RemClose(agentID)`: 关闭指定的agent连接

## 构建

### Linux/macOS
```bash
chmod +x build.sh
./build.sh
```

### Windows
```cmd
build.bat
```

构建完成后会生成：
- `static/main.wasm` - WASM二进制文件
- `static/wasm_exec.js` - Go WASM运行时
- `static/index.html` - 测试页面

## 运行

1. 启动测试服务器：
```bash
cd server
go run server.go
```

2. 在浏览器中打开：
```
https://localhost:8080/static/index.html
```

## 使用示例

### 基本连接
在浏览器中输入命令行：
```
-l wasm://localhost:8080/ws
```

### 参数说明
- `-l wasm://server:port/path`: 连接地址（使用wasm传输层）
- 其他rem标准参数都支持

### JavaScript调用示例
```javascript
// 建立连接
try {
    const result = await RemDial("-l wasm://localhost:8080/ws");
    console.log("Connected:", result.agentID);
} catch (error) {
    console.error("Connection failed:", error);
}

// 关闭连接
const closeResult = RemClose(agentID);
if (closeResult.success) {
    console.log("Connection closed");
}
```

## 开发说明

### 文件结构
```
cmd/wasm/
├── main.go              # WASM客户端主程序
├── build.sh             # Linux/macOS构建脚本
├── build.bat            # Windows构建脚本
├── static/
│   └── index.html       # 测试页面
├── server/
│   └── server.go        # 测试服务器
└── README.md            # 说明文档

protocol/tunnel/wasm/
├── wasm.go              # WASM传输层实现
└── conn.go              # WASM连接实现
```

### 关键实现
1. **传输层注册**: 在`protocol/core/consts.go`中添加`WASMTunnel`常量
2. **WebSocket库**: 使用wasmws库提供专业的WebSocket实现
3. **WebSocket兼容**: Listener使用标准WebSocket实现，Dialer使用wasmws库
4. **命令行解析**: 复用`runner.Options`进行参数解析
5. **Agent管理**: 使用sync.Map管理活跃的agent连接

## 注意事项

1. **HTTPS要求**: 由于浏览器安全限制，WebSocket连接需要HTTPS
2. **CORS设置**: 确保服务器正确设置CORS头
3. **证书配置**: 测试服务器需要有效的TLS证书
4. **浏览器兼容**: 需要支持WebAssembly的现代浏览器

## wasmws库优势

使用wasmws库替代自定义实现的优势：

1. **稳定性**: 经过充分测试的WebSocket实现
2. **性能**: 优化的二进制数据处理和大消息流式传输
3. **兼容性**: 完全符合net.Conn接口规范
4. **错误处理**: 更好的错误处理和超时机制
5. **维护性**: 减少自定义代码，降低维护成本

## 测试构建

使用测试脚本验证构建过程：

### Linux/macOS
```bash
chmod +x test-wasmws.sh
./test-wasmws.sh
```

### Windows
```cmd
test.bat
```

## 故障排除

### 常见问题

1. **WASM加载失败**
   - 检查服务器MIME类型设置
   - 确保main.wasm文件存在且可访问
   - 检查浏览器控制台错误信息

2. **WebSocket连接失败**
   - 检查URL格式（必须是wss://）
   - 确认服务器WebSocket端点正常工作
   - 检查防火墙和网络设置

3. **证书错误**
   - 确保使用有效的TLS证书
   - 对于测试环境，可以在浏览器中手动接受自签名证书

4. **连接状态错误 (CONNECTING state)**
   - 这个问题已在最新版本中修复
   - 确保使用最新的conn.go实现
   - WebSocket会等待连接建立后再发送数据

5. **构建错误**
   - 确保Go版本支持WASM (Go 1.11+)
   - 检查GOOS和GOARCH环境变量设置
   - 确保所有依赖包都可用

### 调试步骤

1. **检查WASM模块**
   ```javascript
   // 在浏览器控制台中检查
   console.log('WASM ready:', wasmReady);
   ```

2. **检查WebSocket连接**
   - 打开浏览器开发者工具
   - 查看网络标签页的WebSocket连接
   - 检查连接状态和消息传输

3. **查看详细日志**
   - 所有操作都会在页面日志区域显示
   - 检查Go程序的console.log输出
   - 查看服务器端日志

4. **测试基本连接**
   ```bash
   # 测试WebSocket端点
   wscat -c wss://localhost:8080/ws
   ```

### 性能优化

1. **减小WASM文件大小**
   ```bash
   # 使用构建标签减少依赖
   GOOS=js GOARCH=wasm go build -ldflags="-s -w" -o static/main.wasm main.go
   ```

2. **启用压缩**
   - 在服务器端启用gzip压缩
   - 对WASM文件进行压缩传输
