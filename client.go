package xwebsocket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-x-pkg/log"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type ReaderCb func(t int, raw []byte) error

var ErrChanIsFull = errors.New("chan is full")

type Client struct {
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	isZapLogger bool

	rawMsgChan chan *RawMsg
}

// RawMsgSend send RawMsg.
func (wsc *Client) RawMsgSend(msg *RawMsg) {
	wsc.rawMsgChan <- msg
}

// TextMsgSend sends text message.
func (wsc *Client) TextMsgSend(text []byte) {
	wsc.TextMsgSendWithDone(text, nil)
}

// TextMsgSendWithDone sends text message with done send chan.
func (wsc *Client) TextMsgSendWithDone(text []byte, done chan error) {
	wsc.rawMsgChan <- &RawMsg{
		Type: websocket.TextMessage,
		Data: text,
		Done: done,
	}
}

// RawMsgSendNonBlock sends msg and do not block.
func (wsc *Client) RawMsgSendNonBlock(msg *RawMsg) {
	select {
	case wsc.rawMsgChan <- msg:
	case <-wsc.ctx.Done():
	default:
		select {
		case msg.Done <- ErrChanIsFull:
		case <-wsc.ctx.Done():
		}
	}
}

// Disconnect cancel ctx.
// y d better cancel parent context in order to disconnect.
func (wsc *Client) Disconnect() { wsc.cancel() }

// WriteWorker handles all writes operations for websocket connection.
// Some ws endpoints don't support ping/pong.
// enablePing == false. pingPeriod, writeTimeout are useless.
// enablePongHandler == false. readTimeout is useless.
func (wsc *Client) WriteWorker(fnLog log.FnT, traceID string,
	// ping
	enablePing bool,
	pingPeriod,
	writeTimeout time.Duration,

	// pong
	enablePongHandler bool,
	readTimeout time.Duration,
) {
	lastPingAt := time.Now()

	var chanPing <-chan time.Time

	if enablePing {
		ticker := time.NewTicker(pingPeriod)

		chanPing = ticker.C

		defer ticker.Stop()
	}

	defer func() {
		wsc.cancel()

		if err := wsc.conn.Close(); err != nil {
			fnLog(log.Error, "error close websocket connection",
				zap.String("traceId", traceID),
				zap.Error(err))
		}
	}()

	start := time.Now()

	fnLog(log.Info, "writer start",
		zap.Bool("enablePing", enablePing),
		zap.Duration("pingPeriod", pingPeriod),
		zap.Duration("writeTimeout", writeTimeout),

		zap.Bool("enablePongHandler", enablePongHandler),
		zap.Duration("readTimeout", readTimeout),

		zap.String("traceId", traceID))

	defer func() {
		fnLog(log.Info, "writer stop",
			zap.Duration("uptime", time.Since(start)),
			zap.String("traceId", traceID))
	}()

	if enablePongHandler {
		wsc.conn.SetPongHandler(func(string) error {
			if err := wsc.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				err := fmt.Errorf("error set read deadline: %w")

				fnLog(log.Debug, "set read deadline",
					zap.Duration("latency", time.Since(lastPingAt)),
					zap.String("traceId", traceID),
					zap.Error(err))

				return err
			}

			fnLog(log.Debug, "pong",
				zap.Duration("latency", time.Since(lastPingAt)),
				zap.String("traceId", traceID))

			return nil
		})
	}

	for {
		select {
		case msg := <-wsc.rawMsgChan:
			if msg.Data != nil {
				fnLog(log.Debug, "message write",
					zap.ByteString("data", msg.Data),
					zap.String("traceId", traceID))
			}

			err := wsc.conn.WriteMessage(msg.Type, msg.Data)
			if err != nil {
				fnLog(log.Error, "write error",
					zap.String("traceId", traceID),
					zap.Error(err))
			}

			if msg.Done != nil {
				msg.Done <- err
			}

		case <-chanPing:
			if err := wsc.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				fnLog(log.Error, "error set write deadline",
					zap.Duration("writeTimeout", writeTimeout),
					zap.String("traceId", traceID),
					zap.Error(err))
			}

			if err := wsc.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				if err != websocket.ErrCloseSent {
					err = fmt.Errorf("write ping message error: %w", err)
					fnLog(log.Error, "ping error",
						zap.String("traceId", traceID),
						zap.Error(err))
				}

				return
			}

			lastPingAt = time.Now()

			fnLog(log.Debug, "ping",
				zap.Duration("period", pingPeriod),
				zap.String("traceId", traceID))

		case <-wsc.ctx.Done():
			return
		}
	}
}

// ReadWorker handles all reads operations for websocket connection.
//
// prefix => log-prefix.
func (wsc *Client) ReadWorker(fnLog log.FnT, traceID string, cb ReaderCb) {
	defer func() {
		wsc.cancel()
	}()

	for {
		select {
		case <-wsc.ctx.Done():
			return
		default:
		}

		t, raw, err := wsc.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(
				err,

				// 1001 indicates that an endpoint is "going away", such as a server
				// going down or a browser having navigated away from a page.
				websocket.CloseGoingAway,
			) {
				select {
				case <-wsc.ctx.Done():
					// websocket read message error: "read tcp ...: use of closed network connection"
				default:
					fnLog(log.Error, "websocket read message error",
						zap.String("traceId", traceID),
						zap.Error(err))
				}
			}

			break
		}

		if t != websocket.TextMessage {
			fnLog(log.Warn, "got unsupported websocket message type",
				zap.String("traceId", traceID))

			continue
		}

		if cb != nil {
			fnLog(log.Debug, "message read",
				zap.Int("type", t),
				zap.ByteString("data", raw),
				zap.String("traceId", traceID))

			// execute callback
			if err := cb(t, raw); err != nil {
				fnLog(log.Error, "websocket processing message error",
					zap.String("traceId", traceID),
					zap.Error(err))

				return
			}
		}
	}
}

// OnWSUpgrade should be triggered whe websocket upgraded.
func (wsc *Client) OnWSUpgrade(ctx context.Context, conn *websocket.Conn, rawMsgChan chan *RawMsg) {
	if ctx == nil {
		ctx = context.Background()
	}

	wsc.ctx, wsc.cancel = context.WithCancel(ctx)

	wsc.conn = conn

	wsc.rawMsgChan = rawMsgChan
}
