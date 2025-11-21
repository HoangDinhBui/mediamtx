package webrtc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

type DataChannelMessage struct {
	Type      string    `json:"type"`       // "text" or "binary"
	Data      []byte    `json:"data"`       // Binary data (base64 encoded in JSON)
	Text      string    `json:"text"`       // Text data
	Timestamp time.Time `json:"timestamp"`
}

// WebSocketDataChannelRelay relays DataChannel messages to WebSocket clients
type WebSocketDataChannelRelay struct {
	session *session
	clients map[*websocket.Conn]bool
	mu      sync.RWMutex
	log     logger.Writer
}

func NewWebSocketDataChannelRelay(session *session, log logger.Writer) *WebSocketDataChannelRelay {
	return &WebSocketDataChannelRelay{
		session: session,
		clients: make(map[*websocket.Conn]bool),
		log:     log,
	}
}

// AddClient registers a WebSocket client
func (r *WebSocketDataChannelRelay) AddClient(ws *websocket.Conn) {
	r.mu.Lock()
	r.clients[ws] = true
	r.mu.Unlock()
	r.log.Log(logger.Info, "[WS-Relay] Client connected, total: %d", len(r.clients))
}

// RemoveClient unregisters a WebSocket client
func (r *WebSocketDataChannelRelay) RemoveClient(ws *websocket.Conn) {
	r.mu.Lock()
	delete(r.clients, ws)
	r.mu.Unlock()
	ws.Close()
	r.log.Log(logger.Info, "[WS-Relay] Client disconnected, total: %d", len(r.clients))
}

// BroadcastDataChannelMessage sends DataChannel message to all WebSocket clients
func (r *WebSocketDataChannelRelay) BroadcastDataChannelMessage(isText bool, data []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.clients) == 0 {
		return
	}

	// Send raw data to WebSocket (matching browser format)
	for ws := range r.clients {
		var err error
		if isText {
			err = ws.WriteMessage(websocket.TextMessage, data)
		} else {
			err = ws.WriteMessage(websocket.BinaryMessage, data)
		}
		
		if err != nil {
			r.log.Log(logger.Warn, "[WS-Relay] Failed to send to client: %v", err)
			go r.RemoveClient(ws)
		}
	}
}

// HandleWebSocket handles WebSocket connections for DataChannel relay
func (s *Server) HandleWebSocketDataChannelRelay(w http.ResponseWriter, r *http.Request) {
	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			s.Log(logger.Error, "[WS-Relay] PANIC recovered: %v", r)
			http.Error(w, fmt.Sprintf("Internal server error: %v", r), http.StatusInternalServerError)
		}
	}()
	
	// Get path from URL (e.g., /fpt/c02i24040003273/ws)
	pathName := r.URL.Query().Get("path")
	if pathName == "" {
		s.Log(logger.Warn, "[WS-Relay] Missing path parameter")
		http.Error(w, "Missing 'path' parameter", http.StatusBadRequest)
		return
	}

	s.Log(logger.Info, "[WS-Relay] WebSocket connection requested for path: %s", pathName)

	// Find session by path - thread-safe via channel
	s.Log(logger.Debug, "[WS-Relay] Looking up session...")
	sx, err := s.getSessionByPath(pathName)
	if err != nil {
		s.Log(logger.Warn, "[WS-Relay] Session not found for path: %s - %v", pathName, err)
		http.Error(w, fmt.Sprintf("Session not found: %v", err), http.StatusNotFound)
		return
	}
	s.Log(logger.Info, "[WS-Relay] Session found for path: %s", pathName)

	// Upgrade HTTP to WebSocket
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.Log(logger.Error, "[WS-Relay] WebSocket upgrade failed: %v", err)
		return
	}

	s.Log(logger.Info, "[WS-Relay] WebSocket upgraded for path: %s", pathName)

	// Check if session has PeerConnection ready
	sx.mutex.Lock()
	pc := sx.pc
	sx.mutex.Unlock()
	
	if pc == nil {
		ws.WriteJSON(map[string]string{
			"type":    "error",
			"message": "Session not ready - PeerConnection is nil. Please wait for MQTT session to establish.",
		})
		ws.Close()
		s.Log(logger.Error, "[WS-Relay] Session PeerConnection not ready yet for path: %s", pathName)
		return
	}
	
	// Wait for DataChannel to be ready
	s.Log(logger.Info, "[WS-Relay] Waiting for DataChannel to be ready...")
	err = pc.WaitDataChannelReady()
	if err != nil {
		ws.WriteJSON(map[string]string{
			"type":    "error",
			"message": fmt.Sprintf("DataChannel not ready: %v", err),
		})
		ws.Close()
		s.Log(logger.Error, "[WS-Relay] DataChannel not ready for path: %s - %v", pathName, err)
		return
	}
	
	s.Log(logger.Info, "[WS-Relay] DataChannel is ready!")
	
	// Create relay if not exists
	sx.mutex.Lock()
	if sx.wsRelay == nil {
		sx.wsRelay = NewWebSocketDataChannelRelay(sx, s)
	}
	relay := sx.wsRelay
	sx.mutex.Unlock()
	
	// Send ready signal to WebSocket client
	ws.WriteJSON(map[string]string{
		"type":    "session_ready",
		"message": "Session is ready for streaming",
	})

	// Register client
	relay.AddClient(ws)

	// Handle incoming messages from WebSocket (commands to camera)
	go func() {
		defer relay.RemoveClient(ws)
		
		s.Log(logger.Info, "[WS-Relay] WebSocket reader goroutine started for path: %s", pathName)

		for {
			s.Log(logger.Debug, "[WS-Relay] Waiting for message from WebSocket...")
			
			messageType, data, err := ws.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					s.Log(logger.Warn, "[WS-Relay] WebSocket read error: %v", err)
				} else {
					s.Log(logger.Info, "[WS-Relay] WebSocket closed normally")
				}
				break
			}

			s.Log(logger.Info, "[WS-Relay] Received message from WebSocket (type=%d, len=%d)", messageType, len(data))

			// Forward message to camera via DataChannel
			if messageType == websocket.TextMessage {
				s.Log(logger.Info, "[WS-Relay] Received TEXT command from WebSocket: %s", string(data))
				
				// Parse JSON to check command type
				var cmd map[string]interface{}
				json.Unmarshal(data, &cmd)
				if cmdName, ok := cmd["Command"].(string); ok && cmdName == "Stream" {
					s.Log(logger.Info, "[WS-Relay] STREAM COMMAND detected! Forwarding to camera...")
				}
				
				// Send directly to DataChannel
				pc := sx.pc
				if pc != nil {
					dc := pc.DataChannel()
					if dc != nil {
						s.Log(logger.Info, "[WS-Relay] DataChannel label: %s, readyState: %s", dc.Label(), dc.ReadyState())
						
						err = pc.SendDataChannelMessage(data, true)
						if err != nil {
							s.Log(logger.Error, "[WS-Relay] Failed to send to DataChannel: %v", err)
						} else {
							s.Log(logger.Info, "[WS-Relay] TEXT command sent to camera (label=%s)", dc.Label())
							s.Log(logger.Info, "[WS-Relay] Waiting for camera response...")
						}
					} else {
						s.Log(logger.Warn, "[WS-Relay] DataChannel is nil!")
					}
				} else {
					s.Log(logger.Warn, "[WS-Relay] PeerConnection is nil!")
				}
			} else if messageType == websocket.BinaryMessage {
				s.Log(logger.Info, "[WS-Relay] Received BINARY command from WebSocket (%d bytes)", len(data))
				
				// Send binary to DataChannel
				pc := sx.pc
				if pc != nil {
					dc := pc.DataChannel()
					if dc != nil {
						err = pc.SendDataChannelMessage(data, false)
						if err != nil {
							s.Log(logger.Error, "[WS-Relay] Failed to send binary to DataChannel: %v", err)
						} else {
							s.Log(logger.Info, "[WS-Relay] Forwarded BINARY to camera via DataChannel")
						}
					} else {
						s.Log(logger.Warn, "[WS-Relay] DataChannel not ready")
					}
				}
			} else {
				s.Log(logger.Warn, "[WS-Relay] Unknown message type: %d", messageType)
			}
		}
		
		s.Log(logger.Info, "[WS-Relay] WebSocket reader goroutine exiting for path: %s", pathName)
	}()
}