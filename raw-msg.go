package xwebsocket

type RawMsg struct {
	// websocket.BinaryMessage
	// websocket.TextMessage
	Type int
	Data []byte
	Done chan error
}
