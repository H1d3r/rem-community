package wrapper

//func NewWebsocketWrapper(opt map[string]string) *WebsocketWrapper {
//	return &WebsocketWrapper{
//		firstClientHeader: []byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: vAh876rpLu6p92ksdGbInQ==\r\nOrigin: http://%s\r\nSec-WebSocket-Version: 13\r\n\r\n", opt["host"], opt["host"])),
//		firstServerHeader: []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: opR2RscMuMKa7DBr+NTinftdxTg=\r\n\r\n"),
//		clientHeader:      []byte("\x81\x8c\x77\x7b\x49\xdd"),
//	}
//}
//
//type WebsocketWrapper struct {
//	clientHeader      []byte
//	serverHeader      []byte
//	firstClientHeader []byte
//	firstServerHeader []byte
//}
//
//func (w *WebsocketWrapper) headerSize(isClient bool) int {
//	if isClient {
//		return len(w.firstClientHeader) + greetLength
//	} else {
//		return len(w.firstServerHeader) + greetLength
//	}
//}
//
//func (w *WebsocketWrapper) headerStr(isClient bool) []byte {
//	if isClient {
//		return w.firstClientHeader
//	} else {
//		return w.firstServerHeader
//	}
//}
//
//func (w *WebsocketWrapper) Name() string {
//	return "websocket"
//}
//
//func (w *WebsocketWrapper) Wrap(data []byte) ([]byte, error) {
//	return data, nil
//}
//
//func (w *WebsocketWrapper) UnWrap(data []byte) ([]byte, error) {
//	return data, nil
//}
//
//func (w *WebsocketWrapper) ReadGreet(conn net.Conn) (int8, uint32, error) {
//	var greet []byte
//	header := make([]byte, 5)
//	n, err := io.ReadFull(conn, header)
//	if err != nil {
//		return 0, 0, err
//	}
//	if bytes.Equal([]byte("HTTP/"), header) || bytes.Equal([]byte("GET /"), header) {
//		reader, err := readWithStop(conn, []byte{'\r', '\n', '\r', '\n'})
//		if err != nil {
//			return 0, 0, err
//		}
//		utils.Log.Logf(consts.DUMPLog, "[read.greet] %v", append(header, reader...))
//		greet = make([]byte, greetLength)
//		n, err = io.ReadFull(conn, greet)
//		if err != nil || n != greetLength {
//			return 0, 0, err
//		}
//	} else {
//		greet = header
//	}
//	return int8(greet[0]), uint32(binary.LittleEndian.Uint32(greet[1:])), nil
//}
//
//func (w *WebsocketWrapper) GenGreet(msgtype int8, length uint32, isClient bool) []byte {
//	var reader bytes.Buffer
//	if msgtype == int8(message.LoginRespMsg) || msgtype == int8(message.LoginReqMsg) {
//		reader.Write(w.headerStr(isClient))
//	}
//	reader.WriteByte(byte(msgtype))
//	l := make([]byte, 4)
//	binary.LittleEndian.PutUint32(l, length)
//	reader.Write(l)
//	return reader.Bytes()
//}
//
//func readWithStop(conn io.Reader, stop []byte) ([]byte, error) {
//	var err error
//	var n int
//	length := len(stop)
//	stopSigh := make([]byte, length)
//	ch := make([]byte, 1)
//	var reader bytes.Buffer
//	for {
//		n, err = conn.Read(ch)
//		if err != nil {
//			return nil, err
//		}
//		if n == 1 {
//			reader.Write(ch)
//			if length == 1 {
//				stopSigh = ch
//			} else {
//				for j := 0; j <= length-2; j++ {
//					stopSigh[j] = stopSigh[j+1]
//				}
//				stopSigh[length-1] = ch[0]
//			}
//			if bytes.Equal(stop, stopSigh) {
//				return reader.Bytes(), nil
//			}
//		}
//	}
//}
