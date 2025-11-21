package webrtc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/google/uuid"
)

type FPTCameraAdapter struct {
	cli       mqtt.Client
	brokerURL string
	username  string
	password  string
	ctx       context.Context
	ctxCancel context.CancelFunc
	logger    logger.Writer
	server    *Server  // Reference to server for session lookup
	
	// Store session info for trickle ICE
	sessions map[string]*sessionInfo  // serial -> session info
	sessionsMu sync.RWMutex
	
	activeCameras map[string]*cameraState
	camerasMu sync.RWMutex
	
	// Track pending camera requests to filter out responses from wrong cameras
	// Map: clientID -> serial (so we can match OFFER responses by clientID)
	pendingRequests map[string]string  // clientID -> serial
	pendingMu       sync.RWMutex
	
	// Track which cameras we're currently subscribed to (separate from sessions)
	// This ensures we can re-subscribe if connection is lost
	subscribedCameras map[string]bool  // serial -> subscribed
	subscriptionMu    sync.RWMutex
	
	//tlsEnabled bool
	onOffer func(serial, clientID, sdp string) (answerSDP string, err error)
}

type sessionInfo struct {
	pathName string
	secret   uuid.UUID
	ClientID string
}

type cameraState struct {
	serial string
	lastSeen time.Time
	activeConn bool
}

type fptSession struct {
	clientID   string
	serial     string
	createdAt  time.Time
	offerSDP   string
	answerSDP  string
}

func NewFPTCameraAdapter(
	brokerURL, username, password string,
	log logger.Writer,
) *FPTCameraAdapter {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &FPTCameraAdapter{
		brokerURL: brokerURL,
		username:  username,
		password:  password,
		ctx:       ctx,
		ctxCancel: cancel,
		logger:    log,
		sessions: make(map[string]*sessionInfo),
		activeCameras: make(map[string]*cameraState),
		pendingRequests: make(map[string]string),
		subscribedCameras: make(map[string]bool),  // NEW: track subscriptions
	}
}

func (a *FPTCameraAdapter) Start() error {
	a.Log(logger.Info, "[FPT] ========================================")
	a.Log(logger.Info, "[FPT] Original Broker URL: %s", a.brokerURL)
	a.Log(logger.Info, "[FPT] Username: %s", a.username)
	a.Log(logger.Info, "[FPT] Password: %s", strings.Repeat("*", len(a.password)))
	a.Log(logger.Info, "[FPT] ========================================")
	
	brokerURL := a.brokerURL
	
	if strings.Contains(brokerURL, ":8084") {
		if strings.HasPrefix(brokerURL, "tcp://") {
			brokerURL = "wss://" + strings.TrimPrefix(brokerURL, "tcp://")
		} else if strings.HasPrefix(brokerURL, "mqtts://") {
			brokerURL = "wss://" + strings.TrimPrefix(brokerURL, "mqtts://")
		} else if !strings.HasPrefix(brokerURL, "wss://") {
			brokerURL = "wss://" + brokerURL
		}
		
		a.Log(logger.Info, "[FPT] Port 8084 detected -> Using secure WebSocket")
	}
	
	testPaths := []string{"/mqtt", "", "/ws"}
	var finalBrokerURL string
	
	a.Log(logger.Info, "[FPT] Testing connection paths...")
	
	for _, path := range testPaths {
		testURL := strings.TrimSuffix(brokerURL, "/mqtt")
		testURL = strings.TrimSuffix(testURL, "/ws")
		testURL = strings.TrimSuffix(testURL, "/")
		
		if path != "" {
			testURL += path
		}
		
		a.Log(logger.Info, "[FPT] Testing: %s", testURL)
		
		if a.testConnection(testURL) {
			finalBrokerURL = testURL
			a.Log(logger.Info, "[FPT] Found working path: %s", testURL)
			break
		} else {
			a.Log(logger.Warn, "[FPT] Failed: %s", testURL)
		}
	}
	
	if finalBrokerURL == "" {
		a.Log(logger.Error, "[FPT] All connection attempts failed!")
		return fmt.Errorf("failed to find working WebSocket path")
	}
	
	opts := mqtt.NewClientOptions().
		AddBroker(finalBrokerURL).
		SetClientID(fmt.Sprintf("mediamtx-fpt-%d", time.Now().UnixNano())).
		SetUsername(a.username).
		SetPassword(a.password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(1 * time.Second).
		SetMaxReconnectInterval(15 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetPingTimeout(5 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetWriteTimeout(5 * time.Second).
		SetCleanSession(false).
		SetOrderMatters(false).
		SetProtocolVersion(4)
	
	if strings.HasPrefix(finalBrokerURL, "ws://") || strings.HasPrefix(finalBrokerURL, "wss://") {
		a.Log(logger.Info, "[FPT] Adding WebSocket headers")
		opts.SetWebsocketOptions(&mqtt.WebsocketOptions{
			ReadBufferSize:  8192,
			WriteBufferSize: 8192,
		})
	}
	
	if strings.HasPrefix(finalBrokerURL, "wss://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
		}
		opts.SetTLSConfig(tlsConfig)
		a.Log(logger.Info, "[FPT] TLS certificate verification: DISABLED (InsecureSkipVerify=true)")
	}
	
	connectSuccess := make(chan struct{}, 1)  // Buffer = 1 to prevent deadlock
	
	opts.OnConnect = func(c mqtt.Client) {
		a.Log(logger.Info, "[FPT] MQTT connected! (OnConnect callback triggered)")
		
		// Signal connection success
		select {
		case connectSuccess <- struct{}{}:
			a.Log(logger.Debug, "[FPT] Connection signal sent")
		default:
			a.Log(logger.Debug, "[FPT] Connection signal already sent")
		}							
	}
	
	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		a.Log(logger.Error, "[FPT] Connection lost: %v", err)
	}
	
	opts.OnReconnecting = func(_ mqtt.Client, _ *mqtt.ClientOptions) {
		a.Log(logger.Warn, "[FPT] Reconnecting...")
	}
	
	a.cli = mqtt.NewClient(opts)
	
	a.Log(logger.Info, "[FPT] Connecting to broker...")
	token := a.cli.Connect()
	
	if !token.WaitTimeout(15 * time.Second) {
		a.Log(logger.Error, "[FPT] Connection timeout (15s)")
		return fmt.Errorf("MQTT connection timeout")
	}
	
	if token.Error() != nil {
		return fmt.Errorf("MQTT connection failed: %w", token.Error())
	}
	
	select {
	case <-connectSuccess:
		a.Log(logger.Info, "[FPT] Adapter started successfully")
		
	case <-time.After(5 * time.Second):
		a.Log(logger.Warn, "[FPT] OnConnect callback timeout (connection unstable)")
	}
	
	go func() {
		<-a.ctx.Done()
		a.Log(logger.Info, "[FPT] Shutting down adapter...")
		a.cli.Disconnect(250)
	}()
	
	return nil
}

func (a *FPTCameraAdapter) testConnection(brokerURL string) bool {
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(fmt.Sprintf("test-%d", time.Now().UnixNano())).
		SetUsername(a.username).
		SetPassword(a.password).
		SetConnectTimeout(5 * time.Second).
		SetProtocolVersion(4)
	
	if strings.HasPrefix(brokerURL, "wss://") {
		opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	}
	
	if strings.HasPrefix(brokerURL, "ws://") || strings.HasPrefix(brokerURL, "wss://") {
		opts.SetWebsocketOptions(&mqtt.WebsocketOptions{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		})
	}
	
	client := mqtt.NewClient(opts)
	token := client.Connect()
	
	success := token.WaitTimeout(5 * time.Second)
	if !success || token.Error() != nil {
		return false
	}
	
	client.Disconnect(100)
	return true
}

func (a *FPTCameraAdapter) Close() {
	a.ctxCancel()
}

func (a *FPTCameraAdapter) onCameraResponse(_ mqtt.Client, msg mqtt.Message) {
	a.Log(logger.Debug, "[FPT] [MQTT] onCameraResponse callback triggered for topic: %s", msg.Topic())
	
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 4 {
		a.Log(logger.Error, "[FPT] Invalid topic format: %s", msg.Topic())
		return
	}

	serial := topicParts[2]
	a.Log(logger.Info, "[FPT] [MQTT] <- Response from camera: %s", serial)

	a.registerCamera(serial)
	
	// Parse FPTMessage (NEW format)
	var fptMsg FPTMessage
	if err := json.Unmarshal(msg.Payload(), &fptMsg); err != nil {
		a.Log(logger.Error, "[FPT] Failed to unmarshal: %v", err)
		return
	}
	
	a.Log(logger.Info, "[FPT] [MQTT] <- Method=%s, MessageType=%s, Serial=%s", 
		fptMsg.Method, fptMsg.MessageType, fptMsg.Serial)
	
	// Log Result if present (camera acknowledgment)
	if fptMsg.Result != nil {
		a.Log(logger.Info, "[FPT] <- Result: Ret=%d, Msg=%s", 
			fptMsg.Result.Ret, fptMsg.Result.Message)
	}
	
	// Parse Data field to detect message type
	var typeCheck struct {
		Type string `json:"Type"`
		Candidate string `json:"candidate"`
	}
	
	if err := json.Unmarshal(fptMsg.Data, &typeCheck); err != nil {
		a.Log(logger.Error, "[FPT] Failed to parse Data.Type: %v", err)
		return
	}

	typeCheck.Type = strings.ToLower(strings.TrimSpace(typeCheck.Type))
	
	switch typeCheck.Type {
	case "offer":
		a.handleOffer(serial, &fptMsg)

	case "ice":
		if typeCheck.Candidate != "" {
			a.handleICE(serial, &fptMsg)
		}
	
	case "":
		if typeCheck.Candidate != "" {
			a.handleICE(serial, &fptMsg)
		}
		
	case "deny":
		a.handleDeny(serial, &fptMsg)
		
	case "ccu", "success":
		a.handleSuccess(&fptMsg)
		
	default:
		a.Log(logger.Warn, "[FPT] Unknown message type: '%s' (ignoring)", typeCheck.Type)
	}
}

func (a*FPTCameraAdapter) registerCamera(serial string) {
	a.camerasMu.Lock()
	defer a.camerasMu.Unlock()

	if _, exist := a.activeCameras[serial]; !exist {
		a.activeCameras[serial] = &cameraState{
			serial: serial,
			lastSeen: time.Now(),
			activeConn: false,
		}
		a.Log(logger.Info, "[FPT] New camera register: %s (total: %d)", serial, len(a.activeCameras))
	} else {
		a.activeCameras[serial].lastSeen = time.Now()
	}
}

// handleICE - Process trickle ICE candidates sent after offer/answer
func (a *FPTCameraAdapter) handleICE(serial string, fptMsg *FPTMessage) {
	a.Log(logger.Info, "[FPT] Processing ICE candidate for camera: %s", serial)
	
	// Parse ICE candidate data
	iceData, err := fptMsg.ParseICEData()
	if err != nil {
		a.Log(logger.Error, "[FPT] Failed to parse ICE data: %v", err)
		return
	}
	
	if iceData.Candidate == "" {
		a.Log(logger.Debug, "[FPT] Empty ICE candidate, skipping")
		return
	}
	
	a.Log(logger.Info, "[FPT] ICE Candidate: %s", iceData.Candidate)
	
	a.sessionsMu.RLock()
	sessInfo, ok := a.sessions[serial]
	a.sessionsMu.RUnlock()
	
	if !ok {
		a.Log(logger.Warn, "[FPT] No session found for camera: %s", serial)
		return
	}
	
	a.Log(logger.Info, "[FPT] Found session for camera %s (path: %s)", serial, sessInfo.pathName)
	
	candidate := &pwebrtc.ICECandidateInit{
		Candidate:     iceData.Candidate,
		SDPMid:        &iceData.SdpMid,
		SDPMLineIndex: (*uint16)(func() *uint16 { idx := uint16(iceData.SdpMLineIndex); return &idx }()),
	}
	
	if a.server == nil {
		a.Log(logger.Error, "[FPT] Server reference is nil, cannot add candidate")
		return
	}
	
	req := webRTCAddSessionCandidatesReq{
		pathName:   sessInfo.pathName,
		secret:     sessInfo.secret,
		candidates: []*pwebrtc.ICECandidateInit{candidate},
		res:        make(chan webRTCAddSessionCandidatesRes),
	}
	
	select {
	case a.server.chAddSessionCandidates <- req:
		res := <-req.res
		if res.err != nil {
			a.Log(logger.Error, "[FPT] Failed to add ICE candidate: %v", res.err)
			return
		}
		a.Log(logger.Info, "[FPT] ICE candidate added successfully")
	case <-a.ctx.Done():
		a.Log(logger.Warn, "[FPT] Context cancelled while adding ICE candidate")
	}
}

func (a*FPTCameraAdapter) handleICE_DISABLED(serial string, fptMsg *FPTMessage) {
	a.Log(logger.Info, "[FPT] ICE message received but IGNORED (trickle disabled): serial=%s", serial)
}

func (a *FPTCameraAdapter) handleOffer(serial string, fptMsg *FPTMessage) {
	startTime := time.Now()
	a.Log(logger.Info, "[FPT] OFFER START: %s", serial)
	
	if a.onOffer == nil {
		a.Log(logger.Error, "[FPT] FATAL: onOffer callback is NIL!")
		return
	}
	
	offerData, err := fptMsg.ParseOfferData()
	if err != nil {
		a.Log(logger.Error, "[FPT] Failed to parse OfferData: %v", err)
		return
	}
	
	a.pendingMu.RLock()
	expectedSerial, isPending := a.pendingRequests[offerData.ClientID]
	a.pendingMu.RUnlock()
	
	if !isPending {
		a.Log(logger.Warn, "[FPT] IGNORED: OFFER with clientID %s from camera %s (not our trigger, probably mobile app)", offerData.ClientID, serial)
		return
	}
	
	if expectedSerial != serial {
		a.Log(logger.Error, "[FPT] [OFFER] Serial mismatch! Expected %s for clientID %s, got %s", expectedSerial, offerData.ClientID, serial)
		return
	}
	
	a.Log(logger.Info, "[FPT] OFFER from expected camera %s (clientID=%s)", serial, offerData.ClientID)
	
	// Remove from pending after we start processing
	a.pendingMu.Lock()
	delete(a.pendingRequests, offerData.ClientID)
	a.pendingMu.Unlock()
	
	a.Log(logger.Info, "[FPT] Parsed offer: +%dms", time.Since(startTime).Milliseconds())
	
	// Log offer details
	ccu := 0
	if offerData.CurrentClients != nil {
		ccu = offerData.CurrentClients.Total
	}
	
	a.Log(logger.Info, "[FPT] OFFER details:")
	a.Log(logger.Info, "[FPT] 	- Camera: %s", serial)
	a.Log(logger.Info, "[FPT] 	- ClientID: %s", offerData.ClientID)
	a.Log(logger.Info, "[FPT] 	- CCU: %d", ccu)
	a.Log(logger.Info, "[FPT] 	- SDP: %d bytes", len(offerData.Sdp))
	
	candidateCount := strings.Count(offerData.Sdp, "a=candidate:")
	a.Log(logger.Info, "[FPT] 	- ICE candidates in offer: %d", candidateCount)
	
	a.camerasMu.Lock()
	if cam, exist := a.activeCameras[serial]; exist {
		cam.activeConn = true
	}
	a.camerasMu.Unlock()

	// Process offer asynchronously (avoid blocking MQTT callback)
	go func() {
		a.Log(logger.Info, "[FPT] [OFFER] Calling onOffer callback...")

		answerSDP, err := a.onOffer(serial, offerData.ClientID, offerData.Sdp)
		if err != nil {
			a.Log(logger.Error, "[FPT] [OFFER] Callback failed: %v", err)
			return
		}

		a.Log(logger.Info, "[FPT] [OFFER] Answer received (%d bytes)", len(answerSDP))
		a.sendAnswer(serial, offerData.ClientID, answerSDP)
	}()
}

func (a *FPTCameraAdapter) handleDeny(serial string, fptMsg *FPTMessage) {
	denyData, err := fptMsg.ParseDenyData()
	if err != nil {
		a.Log(logger.Error, "[FPT] [DENY] Failed to parse: %v", err)
		return
	}

	a.Log(logger.Warn, "[FPT] [DENY] Camera %s: %s", serial, denyData.Message)
}

func (a *FPTCameraAdapter) handleSuccess(fptMsg *FPTMessage) {
	successData, err := fptMsg.ParseSuccessData()
	if err != nil {
		a.Log(logger.Error, "[FPT] [SUCCESS] Failed to parse: %v", err)
		return
	}
	
	ccu := 0
	if successData.CurrentClients != nil {
		ccu = successData.CurrentClients.Total
	}
	
	a.Log(logger.Info, "[FPT] [SUCCESS] Camera: %s (CCU=%d)", fptMsg.Serial, ccu)
}

func (a *FPTCameraAdapter) sendAnswer(serial, clientID, sdp string) {
	sendStart := time.Now()
	a.Log(logger.Info, "[FPT] [ANSWER] Sending to camera %s", serial)
	
	candidateCount := strings.Count(sdp, "a=candidate:")
	a.Log(logger.Info, "[FPT] [ANSWER] Contains %d ICE candidates", candidateCount)
	
	fptMsg := NewFPTAnswerMessage(serial, clientID, sdp)
	payload, err := json.Marshal(fptMsg)
	if err != nil {
		a.Log(logger.Error, "[FPT] [ANSWER] Marshal failed: %v", err)
		return
	}
	
	a.Log(logger.Info, "[FPT] [ANSWER] Marshal done: +%dms (%d bytes)", 
		time.Since(sendStart).Milliseconds(), len(payload))

	requestTopic := fmt.Sprintf("ipc/fss/%s/request/signaling", serial)
	
	if !a.cli.IsConnected() {
		a.Log(logger.Error, "[FPT] [ANSWER] MQTT not connected!")
		return
	}
	
	publishStart := time.Now()
	token := a.cli.Publish(requestTopic, 0, false, payload)
	
	if !token.WaitTimeout(10 * time.Second) {
		a.Log(logger.Error, "[FPT] [ANSWER] Timeout (10s)")
		return
	}
	
	if token.Error() != nil {
		a.Log(logger.Error, "[FPT] [ANSWER] Failed: %v", token.Error())
		return
	}
	
	a.Log(logger.Info, "[FPT] [ANSWER] SENT! (publish=%dms, total=%dms)", 
		time.Since(publishStart).Milliseconds(),
		time.Since(sendStart).Milliseconds())
}

func (a *FPTCameraAdapter) SetOnOffer(fn func(serial, clientID, sdp string) (string, error)) {
	a.onOffer = fn
}

func (a *FPTCameraAdapter) RegisterSession(serial, pathName string, secret uuid.UUID, clientID string) {
	a.sessionsMu.Lock()
    defer a.sessionsMu.Unlock()
    
    a.sessions[serial] = &sessionInfo{
        pathName: pathName,
        secret:   secret,
        ClientID: clientID,
    }
    
    a.Log(logger.Info, "[FPT] Session registered: serial=%s, path=%s, clientID=%s", 
        serial, pathName, clientID)
}

func (a *FPTCameraAdapter) Log(level logger.Level, format string, args ...interface{}) {
	a.logger.Log(level, format, args...)
}

func (a *FPTCameraAdapter) GetActiveCameras() []string {
	a.camerasMu.RLock()
	defer a.camerasMu.RUnlock()

	cameras := make([]string, 0, len(a.activeCameras))
	for serial := range a.activeCameras {
		cameras = append(cameras, serial)
	}
	return cameras
}

// func extractSerialFromPath(pathName string) string {
// 	if !strings.HasPrefix(pathName, "fpt/") {
// 		return "unknown"
// 	}
	
// 	serial := strings.TrimPrefix(pathName, "fpt/")
	
// 	serial = strings.TrimSuffix(serial, "/")
	
// 	if serial == "" {
// 		return "unknown"
// 	}
	
// 	return serial
// }

func (a *FPTCameraAdapter) stripDataChannelFromSDP(sdp string) (string, bool) {
	if !strings.HasSuffix(sdp, "\r\n") {
		sdp += "\r\n"
	}
	
	lines := strings.Split(sdp, "\r\n")
	
	var dataMid string
	startIdx := -1
	endIdx := -1
	
	for i, line := range lines {
		if strings.HasPrefix(line, "m=application") {
			startIdx = i
		} else if startIdx >= 0 && strings.HasPrefix(line, "m=") {
			endIdx = i
			break
		} else if startIdx >= 0 && strings.HasPrefix(line, "a=mid:") {
			dataMid = strings.TrimSpace(strings.TrimPrefix(line, "a=mid:"))
		}
	}
	
	if startIdx >= 0 && endIdx == -1 {
		for i := len(lines) - 1; i > startIdx; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				endIdx = i + 1
				break
			}
		}
		if endIdx == -1 {
			endIdx = len(lines)
		}
	}
	
	if startIdx == -1 {
		return sdp, false
	}
	
	newLines := append(lines[:startIdx], lines[endIdx:]...)
	
	if dataMid != "" {
		for i, line := range newLines {
			if strings.HasPrefix(line, "a=group:BUNDLE") {
				parts := strings.Fields(line)
				filtered := make([]string, 0, len(parts))
				
				for _, part := range parts {
					if part != dataMid {
						filtered = append(filtered, part)
					}
				}
				
				newLines[i] = strings.Join(filtered, " ")
				break
			}
		}
	}
	
	cleaned := strings.Join(newLines, "\r\n")
	cleaned = strings.TrimRight(cleaned, "\r\n")
	cleaned += "\r\n"
	cleaned = strings.ReplaceAll(cleaned, "\r\n\r\n\r\n", "\r\n\r\n")
	
	return cleaned, true
}

func (s *Server) APITriggerFPTStream(w http.ResponseWriter, r *http.Request) {
	if s.fptAdapter == nil {
		http.Error(w, "FPT adapter not enabled", http.StatusBadRequest)
		return
	}

	serial := r.URL.Query().Get("serial")
	if serial == "" {
		http.Error(w, "Missing 'serial' parameter", http.StatusBadRequest)
		return
	}

	if len(serial) < 10 {
		http.Error(w, "Invalid serial format (too short)", http.StatusBadRequest)
		return
	}

	clientID := uuid.New().String()
	s.Log(logger.Info, "[FPT] [API] Trigger stream: camera=%s, clientID=%s", serial, clientID)

	err := s.fptAdapter.SubscribeToCameraTopics(serial)
	if err != nil {
		s.Log(logger.Error, "[FPT] [API] Subscribe failed: %v", err)
		http.Error(w, fmt.Sprintf("Subscribe failed: %v", err), http.StatusInternalServerError)
		return
	}
	
	s.Log(logger.Info, "[FPT] [API] Subscribed to camera response topic")

	// Register pending request
	s.fptAdapter.pendingMu.Lock()
	s.fptAdapter.pendingRequests[clientID] = serial
	s.fptAdapter.pendingMu.Unlock()
	
	s.Log(logger.Info, "[FPT] [API] Registered pending: clientID=%s -> serial=%s", clientID, serial)

	fptMsg := NewFPTRequestMessage(serial, clientID)
	payload, err := json.Marshal(fptMsg)
	if err != nil {
		s.fptAdapter.pendingMu.Lock()
		delete(s.fptAdapter.pendingRequests, clientID)
		s.fptAdapter.pendingMu.Unlock()
		
		http.Error(w, fmt.Sprintf("Marshal failed: %v", err), http.StatusInternalServerError)
		return
	}

	requestTopic := fmt.Sprintf("ipc/fss/%s/request/signaling", serial)
	s.Log(logger.Info, "[FPT] [API] Publishing to: %s", requestTopic)
	
	// Track timing for performance analysis
	publishStartTime := time.Now()
	
	token := s.fptAdapter.cli.Publish(requestTopic, 1, false, payload)
	
	waitStartTime := time.Now()
	if !token.WaitTimeout(5 * time.Second) {
		// Remove from pending if publish failed
		s.fptAdapter.pendingMu.Lock()
		delete(s.fptAdapter.pendingRequests, clientID)
		s.fptAdapter.pendingMu.Unlock()
		
		waitDuration := time.Since(waitStartTime)
		totalDuration := time.Since(publishStartTime)
		s.Log(logger.Error, "[FPT] [API] MQTT publish timeout. Total: %v, WaitTimeout: %v", totalDuration, waitDuration)
		http.Error(w, "MQTT publish timeout (5s)", http.StatusGatewayTimeout)
		return
	}

	if token.Error() != nil {
		s.fptAdapter.pendingMu.Lock()
		delete(s.fptAdapter.pendingRequests, clientID)
		s.fptAdapter.pendingMu.Unlock()
		
		totalDuration := time.Since(publishStartTime)
		s.Log(logger.Error, "[FPT] [API] Publish failed after %v: %v", totalDuration, token.Error())
		http.Error(w, fmt.Sprintf("Publish failed: %v", token.Error()), http.StatusInternalServerError)
		return
	}
	
	totalDuration := time.Since(publishStartTime)
	s.Log(logger.Info, "[FPT] [API] Request sent successfully (elapsed: %v)", totalDuration)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "requested",
		"serial":   serial,
		"clientId": clientID,
		"message":  "Stream request sent to camera",
	})
}

func (a *FPTCameraAdapter) debugPrintSDP(sdp string, label string) {
	a.Log(logger.Debug, "[FPT] === %s SDP START ===", label)
	lines := strings.Split(sdp, "\r\n")
	for i, line := range lines {
		if i > 40 {
			a.Log(logger.Debug, "... (%d more lines)", len(lines)-i)
			break
		}
		a.Log(logger.Debug, "%s", line)
	}
	a.Log(logger.Debug, "[FPT] === %s SDP END ===", label)
}

func (s *Server) APIListFPTCameras(w http.ResponseWriter, r *http.Request) {
	if s.fptAdapter == nil {
		http.Error(w, "FPT adapter not enabled", http.StatusBadRequest)
		return
	}

	cameras := s.fptAdapter.GetActiveCameras()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"cameras": cameras,
		"total":   len(cameras),
	})
}

func (s *Server) APIConnectCamera(w http.ResponseWriter, r *http.Request) {
	if s.fptAdapter == nil {
		http.Error(w, "FPT adapter not enabled", http.StatusBadRequest)
		return
	}
	
	serial := r.URL.Query().Get("serial")
	if serial == "" {
		http.Error(w, "Missing 'serial' parameter", http.StatusBadRequest)
		return
	}
	
	offerBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read offer: %v", err), http.StatusBadRequest)
		return
	}
	
	s.Log(logger.Info, "[FPT] [Connect] Browser -> camera %s (offer: %d bytes)", serial, len(offerBytes))
	
	http.Error(w, "Not implemented - use trigger + WHEP", http.StatusNotImplemented)
}

func (s *Server) APITestDataChannel(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "DataChannel support enabled",
		"info":   "Automatic handling when camera opens DataChannel",
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func NewFPTICEMessage_DISABLED(serial, candidate string) *FPTMessage {
    parts := strings.Fields(candidate)

	sdpMid := "0"
	sdpMLineIndex := 0

	for i, part := range parts {
		if strings.HasPrefix(part, "a=mid:") {
			sdpMid = strings.TrimPrefix(part, "a=mid:")
			break
		}
		if part == "sdpMLineIndex" && i+1 < len(parts) {
			if idx, err := strconv.Atoi(parts[i+1]); err == nil {
				sdpMLineIndex = idx
			}
		}
	}

	data := FPTICEData{
        Type:      "ice",
        Candidate: candidate,
		SdpMid: sdpMid,
		SdpMLineIndex: sdpMLineIndex,
    }
    dataBytes, _ := json.Marshal(data)
    
    return &FPTMessage{
        Method:      "ACT",
        MessageType: "Signaling",
        Serial:      serial,
        Data:        dataBytes,
        Timestamp:   time.Now().Unix(),
    }
}

// PublishLocalICE publishes local ICE candidate to camera via MQTT
func (a *FPTCameraAdapter) PublishLocalICE(serial, candidate string) error {
	if a.cli == nil || !a.cli.IsConnected() {
		return fmt.Errorf("MQTT not connected")
	}

	a.sessionsMu.RLock()
	sessionInfo, ok := a.sessions[serial]
	a.sessionsMu.RUnlock()
	
	if !ok {
		return fmt.Errorf("no active session for serial %s", serial)
	}
	
	// FPT camera signaling topic
	topic := fmt.Sprintf("ipc/fss/%s/request/signaling", serial)
	
	// Parse candidate string to extract fields
	parts := strings.Fields(candidate)
	sdpMid := "0"
	sdpMLineIndex := 0
	
	// Extract from candidate if available
	for i, part := range parts {
		if strings.HasPrefix(part, "a=mid:") {
			sdpMid = strings.TrimPrefix(part, "a=mid:")
		}
		if part == "sdpMLineIndex" && i+1 < len(parts) {
			if idx, err := strconv.Atoi(parts[i+1]); err == nil {
				sdpMLineIndex = idx
			}
		}
	}
	
	// Create FPT ICE message
	data := FPTICEData{
		Type:          "ice",
		ClientID:      sessionInfo.ClientID,
		Candidate:     candidate,
		SdpMid:        sdpMid,
		SdpMLineIndex: sdpMLineIndex,
	}
	dataBytes, _ := json.Marshal(data)
	
	msg := &FPTMessage{
		Method:      "ACT",
		MessageType: "Signaling",
		Serial:      serial,
		Data:        dataBytes,
		Timestamp:   time.Now().Unix(),
	}
	
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal ICE message: %w", err)
	}
	
	a.Log(logger.Info, "[FPT] [ICE-Local] Publishing to %s", topic)
	a.Log(logger.Debug, "[FPT] [ICE-Local] Candidate: %s", candidate)
	
	token := a.cli.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(3 * time.Second) {
		return fmt.Errorf("publish timeout (3s)")
	}
	
	if token.Error() != nil {
		return fmt.Errorf("publish error: %w", token.Error())
	}
	
	a.Log(logger.Info, "[FPT] [ICE-Local] Published successfully")
	return nil
}

func (a *FPTCameraAdapter) getActiveSerials() []string {
	a.sessionsMu.RLock()
	defer a.sessionsMu.RUnlock()
	
	serials := make([]string, 0, len(a.sessions))
	for serial := range a.sessions {
		serials = append(serials, serial)
	}
	return serials
}

func (a *FPTCameraAdapter) SubscribeToCameraTopics(serial string) error {
	if a.cli == nil || !a.cli.IsConnected() {
		return fmt.Errorf("MQTT not connected")
	}
	
	// Check if already subscribed (based on subscription state, NOT session state!)
	a.subscriptionMu.RLock()
	alreadySubscribed := a.subscribedCameras[serial]
	a.subscriptionMu.RUnlock()
	
	if alreadySubscribed {
		return nil
	}
	
	// Subscribe to camera response topic
	responseTopic := fmt.Sprintf("ipc/fss/%s/response/signaling", serial)

	a.Log(logger.Debug, "[FPT] [MQTT] Subscribing to: %s (QoS=1)", responseTopic)

	token := a.cli.Subscribe(responseTopic, 1, a.onCameraResponse)

	if !token.WaitTimeout(5 * time.Second) {
		a.Log(logger.Error, "[FPT] [MQTT] Subscribe timeout (5s) for topic: %s", responseTopic)
		return fmt.Errorf("subscribe timeout (5s)")
	}
	
	if token.Error() != nil {
		a.Log(logger.Error, "[FPT] [MQTT] Subscribe failed for topic %s: %v", responseTopic, token.Error())
		return fmt.Errorf("subscribe failed: %w", token.Error())
	}
	
	a.Log(logger.Debug, "[FPT] [MQTT] Subscribe successful for topic: %s", responseTopic)
	
	// Mark as subscribed
	a.subscriptionMu.Lock()
	a.subscribedCameras[serial] = true
	a.subscriptionMu.Unlock()
	
	a.Log(logger.Info, "[FPT] Subscribed to camera response topic: %s", responseTopic)
	
	return nil
}