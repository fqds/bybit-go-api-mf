package bybit_connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type MessageHandler func(message map[string]any) error

func (b *WebSocket) handleIncomingMessages() {
	for {
		if !b.isConnected {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		_, message, err := b.conn.ReadMessage()
		if err != nil {
			log.Println("Error reading:", err)
			b.isConnected = false
			continue
		}

		if b.onMessage != nil {
			messageData := map[string]any{}
			if err := json.Unmarshal(message, &messageData); err != nil {
				messageData = map[string]any{"btMsg": message}
			} else {
				if messageData["op"] == "pong" || messageData["ret_msg"] == "pong" {
					b.isWaitingForPing = false
					continue
				}
			}

			err := b.onMessage(messageData)
			if err != nil {
				log.Println("Error handling message:", err)
				continue
			}
		}
	}
}

func (b *WebSocket) monitorConnection() {
	ticker := time.NewTicker(b.monitorConnectionInterval) // Check every 5 seconds
	defer ticker.Stop()

	for {
		<-ticker.C
		if !b.isConnected && b.ctx.Err() == nil { // Check if disconnected and context not done
			log.Println("Attempting to reconnect...")
			err := b.Connect() // Example, adjust parameters as needed
			if err != nil {
				log.Println("Reconnection failed:", err)
			}
		}

		select {
		case <-b.ctx.Done():
			return // Stop the routine if context is done
		default:
		}
	}
}

func (b *WebSocket) SetMessageHandler(handler MessageHandler) {
	b.onMessage = handler
}

type WebSocket struct {
	conn                      *websocket.Conn
	url                       string
	apiKey                    string
	apiSecret                 string
	maxAliveTime              string
	pingInterval              time.Duration
	onMessage                 MessageHandler
	ctx                       context.Context
	cancel                    context.CancelFunc
	isConnected               bool
	isInitialized             bool
	isWaitingForPing          bool
	monitorConnectionInterval time.Duration
	onConnect                 func()
}

type WebsocketOption func(*WebSocket)

func WithPingInterval(pingInterval time.Duration) WebsocketOption {
	return func(c *WebSocket) {
		c.pingInterval = pingInterval
	}
}

func WithMaxAliveTime(maxAliveTime string) WebsocketOption {
	return func(c *WebSocket) {
		c.maxAliveTime = maxAliveTime
	}
}

func WithOnConnect(onConnect func()) WebsocketOption {
	return func(c *WebSocket) {
		c.onConnect = onConnect
	}
}

func WithMonitorConnectionInterval(monitorConnectionInterval time.Duration) WebsocketOption {
	return func(c *WebSocket) {
		c.monitorConnectionInterval = monitorConnectionInterval
	}
}

func NewBybitPrivateWebSocket(url, apiKey, apiSecret string, handler MessageHandler, options ...WebsocketOption) *WebSocket {
	c := &WebSocket{
		url:                       url,
		apiKey:                    apiKey,
		apiSecret:                 apiSecret,
		maxAliveTime:              "",
		pingInterval:              time.Second * 20,
		onMessage:                 handler,
		monitorConnectionInterval: 100 * time.Millisecond,
		onConnect:                 func() {},
	}

	// Apply the provided options
	for _, opt := range options {
		opt(c)
	}

	return c
}

func NewBybitPublicWebSocket(url string, handler MessageHandler, options ...WebsocketOption) *WebSocket {
	c := &WebSocket{
		url:                       url,
		pingInterval:              time.Second * 20,
		onMessage:                 handler,
		monitorConnectionInterval: 100 * time.Millisecond,
		onConnect:                 func() {},
	}

	// Apply the provided options
	for _, opt := range options {
		opt(c)
	}

	return c
}

func (b *WebSocket) Connect() error {
	var err error
	wssUrl := b.url
	if b.maxAliveTime != "" {
		wssUrl += "?max_alive_time=" + b.maxAliveTime
	}
	b.conn, _, err = websocket.DefaultDialer.Dial(wssUrl, nil)
	if err != nil {
		log.Println("Failed to connect to WebSocket:", err)
		return err
	}

	if b.requiresAuthentication() {
		if err = b.sendAuth(); err != nil {
			log.Println("Failed ws auth:", fmt.Sprintf("%v", err))
			return err
		}
	}
	b.isConnected = true
	b.isWaitingForPing = false

	if !b.isInitialized {
		b.isInitialized = true
		b.ctx, b.cancel = context.WithCancel(context.Background())

		go b.monitorConnection()
		go ping(b)
		go b.handleIncomingMessages()
	}

	go b.onConnect()
	log.Println("Connected to WebSocket successfully.")
	return nil
}

func (b *WebSocket) SendSubscription(args []string) (*WebSocket, error) {
	reqID := uuid.New().String()
	subMessage := map[string]interface{}{
		"req_id": reqID,
		"op":     "subscribe",
		"args":   args,
	}
	log.Println("subscribe msg:", fmt.Sprintf("%v", subMessage["args"]))
	if err := b.sendAsJson(subMessage); err != nil {
		log.Println("Failed to send subscription:", err)
		return b, err
	}
	log.Println("Subscription sent successfully.")
	return b, nil
}

// SendRequest sendRequest sends a custom request over the WebSocket connection.
func (b *WebSocket) SendRequest(op string, args map[string]interface{}, headers map[string]string, reqId ...string) (*WebSocket, error) {
	finalReqId := uuid.New().String()
	if len(reqId) > 0 && reqId[0] != "" {
		finalReqId = reqId[0]
	}

	request := map[string]interface{}{
		"reqId":  finalReqId,
		"header": headers,
		"op":     op,
		"args":   []interface{}{args},
	}
	if err := b.sendAsJson(request); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	return b, nil
}

func (b *WebSocket) SendTradeRequest(tradeTruest map[string]interface{}) (*WebSocket, error) {
	if err := b.sendAsJson(tradeTruest); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	return b, nil
}

func ping(b *WebSocket) {
	if b.pingInterval <= 0 {
		fmt.Println("Ping interval is set to a non-positive value.")
		return
	}

	ticker := time.NewTicker(b.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !b.isConnected {
				log.Println("WebSocket is not connected. do not send ping.")
				continue
			}

			if b.isWaitingForPing {
				log.Println("No pong received for the last ping. Marking as disconnected.")
				b.isConnected = false
				continue
			}

			currentTime := time.Now().Unix()
			pingMessage := map[string]string{
				"op":     "ping",
				"req_id": fmt.Sprintf("%d", currentTime),
			}
			jsonPingMessage, err := json.Marshal(pingMessage)
			if err != nil {
				log.Println("Failed to marshal ping message:", err)
				continue
			}
			if err := b.conn.WriteMessage(websocket.TextMessage, jsonPingMessage); err != nil {
				log.Println("Failed to send ping:", err)
				b.isConnected = false
				continue
			}

			b.isWaitingForPing = true
		case <-b.ctx.Done():
			fmt.Println("Ping context closed, stopping ping.")
			return
		}
	}
}

func (b *WebSocket) Disconnect() error {
	b.cancel()
	b.isConnected = false
	return b.conn.Close()
}

func (b *WebSocket) requiresAuthentication() bool {
	return b.url == WEBSOCKET_PRIVATE_MAINNET || b.url == WEBSOCKET_PRIVATE_TESTNET ||
		b.url == WEBSOCKET_TRADE_MAINNET || b.url == WEBSOCKET_TRADE_TESTNET ||
		b.url == WEBSOCKET_TRADE_DEMO || b.url == WEBSOCKET_PRIVATE_DEMO
	// v3 offline
	/*
		b.url == V3_CONTRACT_PRIVATE ||
			b.url == V3_UNIFIED_PRIVATE ||
			b.url == V3_SPOT_PRIVATE
	*/
}

func (b *WebSocket) sendAuth() error {
	// Get current Unix time in milliseconds
	expires := time.Now().UnixNano()/1e6 + 10000
	val := fmt.Sprintf("GET/realtime%d", expires)

	h := hmac.New(sha256.New, []byte(b.apiSecret))
	h.Write([]byte(val))

	// Convert to hexadecimal instead of base64
	signature := hex.EncodeToString(h.Sum(nil))

	authMessage := map[string]interface{}{
		"req_id": uuid.New(),
		"op":     "auth",
		"args":   []interface{}{b.apiKey, expires, signature},
	}
	return b.sendAsJson(authMessage)
}

func (b *WebSocket) sendAsJson(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.send(string(data))
}

func (b *WebSocket) send(message string) error {
	return b.conn.WriteMessage(websocket.TextMessage, []byte(message))
}
