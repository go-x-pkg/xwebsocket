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

var ErrRawMsgChanIsFull = errors.New("raw-msg-chan-is-full")

type Client struct {
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	isZapLogger bool

	rawMsgChan chan *RawMsg
}

func (wsc *Client) RawMsgSend(msg *RawMsg) {
	wsc.rawMsgChan <- msg
}

func (wsc *Client) TextMsgSend(text []byte) {
	wsc.TextMsgSendWithDone(text, nil)
}

func (wsc *Client) WithZapLogger(isZapLogger bool) {
	wsc.isZapLogger = isZapLogger
}

func (wsc *Client) TextMsgSendWithDone(text []byte, done chan error) {
	wsc.rawMsgChan <- &RawMsg{
		Type: websocket.TextMessage,
		Data: text,
		Done: done,
	}
}

func (wsc *Client) RawMsgSendNonBlock(msg *RawMsg) {
	select {
	case wsc.rawMsgChan <- msg:
	case <-wsc.ctx.Done():
	default:
		msg.Done <- ErrRawMsgChanIsFull
	}
}

// y would better cancel parent context in order to disconnect
func (wsc *Client) Disconnect() { wsc.cancel() }

// handles all writes operations for websocket connection
//
// prefix => log-prefix.
func (wsc *Client) WriteWorker(fnLog log.FnT, prefix string,
	pingPeriod, pongWait, writeWait time.Duration) {
	lastPingAt := time.Now()
	ticker := time.NewTicker(pingPeriod)

	defer ticker.Stop()

	defer func() {
		wsc.cancel()

		if err := wsc.conn.Close(); err != nil {
			err = fmt.Errorf("error close websocket connection: %w", err)
			if wsc.isZapLogger {
				fnLog(log.Error, "error close websocket", zap.String("traceId", prefix), zap.Error(err))
			} else {
				fnLog(log.Error, "%s | [-] | %s", prefix, err)
			}
		}
	}()

	if wsc.isZapLogger {
		fnLog(log.Debug, `%s | [+] | start writer. ping-period: %s, pong-wait: %s, write-wait: %s`,
			zap.String("traceId", prefix), zap.Duration("ping-period", pingPeriod),
			zap.Duration("pong-wait", pongWait), zap.Duration("write-wait", writeWait))
	} else {
		fnLog(log.Debug, `%s | [+] | start writer. ping-period: %s, pong-wait: %s, write-wait: %s`,
			prefix, pingPeriod, pongWait, writeWait)
	}

	defer func() {
		if wsc.isZapLogger {
			fnLog(log.Debug, "stop writer", zap.String("traceId", prefix))
		} else {
			fnLog(log.Debug, `%s | [-] | stop writer`, prefix)
		}
	}()

	wsc.conn.SetPongHandler(func(string) error {
		if err := wsc.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			return err
		}
		if wsc.isZapLogger {
			fnLog(log.Trace, "pong", zap.String("traceId", prefix), zap.Duration("latency", time.Since(lastPingAt)))
		} else {
			fnLog(log.Trace, "%s | [<] | pong, ping/pong latency: %s", prefix, time.Since(lastPingAt))
		}

		return nil
	})

	for {
		select {
		case msg := <-wsc.rawMsgChan:
			if msg.Data != nil {
				if wsc.isZapLogger {
					fnLog(log.Trace, "message", zap.String("traceId", prefix), zap.ByteString("data", msg.Data))
				} else {
					fnLog(log.Trace, `%s | [>] | %s`, prefix, msg.Data)
				}
			}

			err := wsc.conn.WriteMessage(msg.Type, msg.Data)
			if err != nil {
				if wsc.isZapLogger {
					fnLog(log.Error, "websocket write error", zap.String("traceId", prefix), zap.Error(err))
				} else {
					fnLog(log.Error, `%s | [-] | websocket write error: "%s"`, prefix, err)
				}
			}

			if msg.Done != nil {
				msg.Done <- err
			}

		case <-ticker.C:
			if err := wsc.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				if wsc.isZapLogger {
					fnLog(log.Error, "error in write deadline", zap.String("traceId", prefix), zap.Error(err))
				} else {
					fnLog(log.Error, `%s | %s`, prefix, err)
				}
			}

			if err := wsc.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				if err != websocket.ErrCloseSent {
					err = fmt.Errorf("websocket write ping-message error: %w", err)
					if wsc.isZapLogger {
						fnLog(log.Error, "ping problem", zap.String("traceId", prefix), zap.Error(err))
					} else {
						fnLog(log.Error, `%s | [-] | %s`, prefix, err)
					}
				}

				return
			}

			lastPingAt = time.Now()

			if wsc.isZapLogger {
				fnLog(log.Trace, "ping", zap.String("traceId", prefix))
			} else {
				fnLog(log.Trace, "%s | [>] | ping", prefix)
			}

		case <-wsc.ctx.Done():
			return
		}
	}
}

// handles all reads operations for websocket connection
//
// prefix => log-prefix.
func (wsc *Client) ReadWorker(fnLog log.FnT, prefix string, cb ReaderCb) {
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
					fnLog(log.Error, `%s | [-] | websocket read message error: "%s"`, prefix, err)
				}
			}

			break
		}

		if t != websocket.TextMessage {
			fnLog(log.Warn, `%s | [~] | got unsupported websocket message type`, prefix)

			continue
		}

		if cb != nil {
			fnLog(log.Trace, "%s | [<] | raw-msg-sz: %d, raw-msg: %q",
				prefix, t, raw)

			// execute callback
			if err := cb(t, raw); err != nil {
				fnLog(log.Error, `%s | [-] | websocket processing message error: "%s"`, prefix, err)

				return
			}
		}
	}
}

func (wsc *Client) OnWSUpgrade(ctx context.Context, conn *websocket.Conn, rawMsgChan chan *RawMsg) {
	if ctx == nil {
		ctx = context.Background()
	}

	wsc.ctx, wsc.cancel = context.WithCancel(ctx)

	wsc.conn = conn

	//! TODO: configure channel capacity
	wsc.rawMsgChan = rawMsgChan
}
