// Package webrtc contains a WebRTC server.
package webrtc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"

	// "net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/gortsplib/v5/pkg/readbuffer"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/restrictnetwork"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	webrtcTurnSecretExpiration = 24 * time.Hour
)

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

func interfaceIsEmpty(i any) bool {
	return reflect.ValueOf(i).Kind() != reflect.Ptr || reflect.ValueOf(i).IsNil()
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

var webrtcNilLogger = logging.NewDefaultLeveledLoggerForScope("", 0, &nilWriter{})

func randInt63() (int64, error) {
	var b [8]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return 0, err
	}

	return int64(uint64(b[0]&0b01111111)<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])), nil
}

// https://cs.opensource.google/go/go/+/refs/tags/go1.20.4:src/math/rand/rand.go;l=119
func randInt63n(n int64) (int64, error) {
	if n&(n-1) == 0 { // n is power of two, can mask
		r, err := randInt63()
		if err != nil {
			return 0, err
		}
		return r & (n - 1), nil
	}

	maxVal := int64((1 << 63) - 1 - (1<<63)%uint64(n))

	v, err := randInt63()
	if err != nil {
		return 0, err
	}

	for v > maxVal {
		v, err = randInt63()
		if err != nil {
			return 0, err
		}
	}

	return v % n, nil
}

func randomTurnUser() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz1234567890"
	b := make([]byte, 20)
	for i := range b {
		j, err := randInt63n(int64(len(charset)))
		if err != nil {
			return "", err
		}

		b[i] = charset[int(j)]
	}

	return string(b), nil
}

type serverAPISessionsListRes struct {
	data *defs.APIWebRTCSessionList
	err  error
}

type serverAPISessionsListReq struct {
	res chan serverAPISessionsListRes
}

type serverAPISessionsGetRes struct {
	data *defs.APIWebRTCSession
	err  error
}

type serverAPISessionsGetReq struct {
	uuid uuid.UUID
	res  chan serverAPISessionsGetRes
}

type serverAPISessionsKickRes struct {
	err error
}

type serverAPISessionsKickReq struct {
	uuid uuid.UUID
	res  chan serverAPISessionsKickRes
}

type webRTCNewSessionRes struct {
	sx            *session
	answer        []byte
	errStatusCode int
	err           error
}

type webRTCNewSessionReq struct {
	pathName    string
	remoteAddr  string
	offer       []byte
	publish     bool
	httpRequest *http.Request
	res         chan webRTCNewSessionRes
	answerCh    chan *pwebrtc.SessionDescription
	clientID string
}

type webRTCAddSessionCandidatesRes struct {
	sx  *session
	err error
}

type webRTCAddSessionCandidatesReq struct {
	pathName   string
	secret     uuid.UUID
	candidates []*pwebrtc.ICECandidateInit
	res        chan webRTCAddSessionCandidatesRes
}

type webRTCDeleteSessionRes struct {
	err error
}

type webRTCDeleteSessionReq struct {
	pathName string
	secret   uuid.UUID
	res      chan webRTCDeleteSessionRes
}

type webRTCGetSessionByPathRes struct {
	sx  *session
	err error
}

type webRTCGetSessionByPathReq struct {
	pathName string
	res      chan webRTCGetSessionByPathRes
}

type serverMetrics interface {
	SetWebRTCServer(defs.APIWebRTCServer)
}

type serverPathManager interface {
	FindPathConf(req defs.PathFindPathConfReq) (*conf.Path, error)
	AddPublisher(req defs.PathAddPublisherReq) (defs.Path, *stream.Stream, error)
	AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
}

type serverParent interface {
	logger.Writer
}

// Server is a WebRTC server.
type Server struct {
	Address               string
	Encryption            bool
	ServerKey             string
	ServerCert            string
	AllowOrigins          []string
	TrustedProxies        conf.IPNetworks
	ReadTimeout           conf.Duration
	WriteTimeout          conf.Duration
	UDPReadBufferSize     uint
	UDPMaxPayloadSize     int
	LocalUDPAddress       string
	LocalTCPAddress       string
	IPsFromInterfaces     bool
	IPsFromInterfacesList []string
	AdditionalHosts       []string
	ICEServers            []conf.WebRTCICEServer
	HandshakeTimeout      conf.Duration
	TrackGatherTimeout    conf.Duration
	STUNGatherTimeout     conf.Duration
	ExternalCmdPool       *externalcmd.Pool
	Metrics               serverMetrics
	PathManager           serverPathManager
	Parent                serverParent

	ctx              context.Context
	ctxCancel        func()
	httpServer       *httpServer
	udpMuxLn         net.PacketConn
	tcpMuxLn         net.Listener
	iceUDPMux        ice.UDPMux
	iceTCPMux        *webrtc.TCPMuxWrapper
	sessions         map[*session]struct{}
	sessionsBySecret map[uuid.UUID]*session

	// --- MQTT signaling config / state (ADD) ---
	MQTTEnabled      bool
	MQTTBrokerURL    string
	MQTTTopicPrefix  string

	mqttSignaler     *MQTTSignaler
	sessionsByPath   map[string]*session
	sessionsMutex    sync.RWMutex // Protect sessionsByPath for WebSocket access

	// FPT Camera adapter (NEW)
	fptAdapter *FPTCameraAdapter
	recorder   *Recorder

	// in
	chNewSession           chan webRTCNewSessionReq
	chCloseSession         chan *session
	chAddSessionCandidates chan webRTCAddSessionCandidatesReq
	chDeleteSession        chan webRTCDeleteSessionReq
	chAPISessionsList      chan serverAPISessionsListReq
	chAPISessionsGet       chan serverAPISessionsGetReq
	chAPIConnsKick         chan serverAPISessionsKickReq
	chGetSessionByPath     chan webRTCGetSessionByPathReq

	// out
	done chan struct{}
}

// ---- MQTT PubAPI implementation (ADD) ----
type mqttPubAPI struct{ 
	s *Server
	mu sync.Mutex
    	sessKeyByPath map[string]uuid.UUID
 }

// Create publisher (Offer -> Answer) via Server.newSession
func (a *mqttPubAPI) CreatePublisherFromOffer(path, offerSDP string) (string, error) {
	a.s.Log(logger.Info, "[MQTT-API] CreatePublisherFromOffer called for path=%s", path)

	// Create answer channel
	answerCh := make(chan *pwebrtc.SessionDescription, 1)
	a.s.Log(logger.Info, "[MQTT-API] Created answerCh (buffered size=1)")

	// Create session request
	req := webRTCNewSessionReq{
		pathName:    path,
		remoteAddr:  "127.0.0.1:0",
		offer:       []byte(offerSDP),
		publish:     true,
		httpRequest: nil,
		answerCh:    answerCh,
	}

	// Request Server to create session
	a.s.Log(logger.Info, "[MQTT-API] Calling newSession...")
	res := a.s.newSession(req)
	if res.err != nil {
		a.s.Log(logger.Error, "[MQTT-API] newSession failed: %v", res.err)
		return "", res.err
	}
	if res.sx == nil {
		a.s.Log(logger.Error, "[MQTT-API] session is nil")
		return "", fmt.Errorf("session is nil after newSession")
	}

	a.s.Log(logger.Info, "[MQTT-API] Session created successfully, waiting for answer...")

	// Save session key for AddRemoteCandidate
	a.mu.Lock()
	if a.sessKeyByPath == nil {
		a.sessKeyByPath = make(map[string]uuid.UUID)
	}
	a.sessKeyByPath[path] = res.sx.secret
	a.mu.Unlock()

	// Wait for answer from channel
	select {
	case ans := <-answerCh:
		if ans == nil {
			a.s.Log(logger.Error, "[MQTT-API] Received nil answer")
			return "", fmt.Errorf("received nil answer")
		}
		if ans.SDP == "" {
			a.s.Log(logger.Error, "[MQTT-API] Received empty SDP")
			return "", fmt.Errorf("received empty SDP")
		}

		a.s.Log(logger.Info, "[MQTT-API] Got answer from channel (len=%d), returning", len(ans.SDP))
		return ans.SDP, nil

	case <-time.After(30 * time.Second):
		a.s.Log(logger.Error, "[MQTT-API] Timeout waiting for answer (30s)")
		return "", fmt.Errorf("timeout waiting for answer")
	}
}

// Add remote ICE candidate from publisher (MQTT -> server)
func (a *mqttPubAPI) AddRemoteCandidate(path, cand string) error {
    a.mu.Lock()
    key, ok := a.sessKeyByPath[path]
    a.mu.Unlock()
    if !ok {
        return fmt.Errorf("session key not found for path=%s", path)
    }

    init := &pwebrtc.ICECandidateInit{Candidate: cand}
    resp := a.s.addSessionCandidates(webRTCAddSessionCandidatesReq{
        pathName:   path,
        secret:     key,
        candidates: []*pwebrtc.ICECandidateInit{init},
    })
    if resp.err != nil {
        a.s.Log(logger.Error, "[WebRTC][MQTT] addCandidate error path=%s err=%v", path, resp.err)
    }
    return resp.err
}

// (Phase-2) cho phép server đẩy ICE local ra MQTT (callback đăng ký từ session)
func (a *mqttPubAPI) OnLocalICE(path string, fn func(cand string)) {
}

// Initialize initializes the server.
func (s *Server) Initialize() error {
	ctx, ctxCancel := context.WithCancel(context.Background())

	s.ctx = ctx
	s.ctxCancel = ctxCancel
	s.sessions = make(map[*session]struct{})
	s.sessionsBySecret = make(map[uuid.UUID]*session)
	s.sessionsByPath = make(map[string]*session)
	s.chNewSession = make(chan webRTCNewSessionReq)
	s.chCloseSession = make(chan *session)
	s.chAddSessionCandidates = make(chan webRTCAddSessionCandidatesReq)
	s.chDeleteSession = make(chan webRTCDeleteSessionReq)
	s.chAPISessionsList = make(chan serverAPISessionsListReq)
	s.chAPISessionsGet = make(chan serverAPISessionsGetReq)
	s.chAPIConnsKick = make(chan serverAPISessionsKickReq)
	s.chGetSessionByPath = make(chan webRTCGetSessionByPathReq)
	s.done = make(chan struct{})

	s.httpServer = &httpServer{
		address:        s.Address,
		encryption:     s.Encryption,
		serverKey:      s.ServerKey,
		serverCert:     s.ServerCert,
		allowOrigins:   s.AllowOrigins,
		trustedProxies: s.TrustedProxies,
		readTimeout:    s.ReadTimeout,
		writeTimeout:   s.WriteTimeout,
		pathManager:    s.PathManager,
		parent:         s,
	}
	err := s.httpServer.initialize()
	if err != nil {
		ctxCancel()
		return err
	}

	if s.LocalUDPAddress != "" {
		s.udpMuxLn, err = net.ListenPacket(restrictnetwork.Restrict("udp", s.LocalUDPAddress))
		if err != nil {
			s.httpServer.close()
			ctxCancel()
			return err
		}

		if s.UDPReadBufferSize != 0 {
			err = readbuffer.SetReadBuffer(s.udpMuxLn.(*net.UDPConn), int(s.UDPReadBufferSize))
			if err != nil {
				s.udpMuxLn.Close()
				s.httpServer.close()
				ctxCancel()
				return err
			}
		}

		s.iceUDPMux = pwebrtc.NewICEUDPMux(webrtcNilLogger, s.udpMuxLn)
	}

	if s.LocalTCPAddress != "" {
		s.tcpMuxLn, err = net.Listen(restrictnetwork.Restrict("tcp", s.LocalTCPAddress))
		if err != nil {
			if s.udpMuxLn != nil {
				s.udpMuxLn.Close()
			}
			s.httpServer.close()
			ctxCancel()
			return err
		}

		s.iceTCPMux = &webrtc.TCPMuxWrapper{
			Mux: pwebrtc.NewICETCPMux(webrtcNilLogger, s.tcpMuxLn, 8),
			Ln:  s.tcpMuxLn,
		}
	}

	str := "listener opened on " + s.Address + " (HTTP)"
	if s.udpMuxLn != nil {
		str += ", " + s.LocalUDPAddress + " (ICE/UDP)"
	}
	if s.tcpMuxLn != nil {
		str += ", " + s.LocalTCPAddress + " (ICE/TCP)"
	}
	s.Log(logger.Info, str)

	go s.run()

	// ========================================================================
	// MQTT SIGNALING (OPTIONAL)
	// ========================================================================
	if !s.MQTTEnabled {
		if getenv("MQTT_ENABLED", "1") == "1" {
			s.MQTTEnabled = true
			s.MQTTBrokerURL   = getenv("MQTT_BROKER_URL",  "tcp://127.0.0.1:1883")
			s.MQTTTopicPrefix = getenv("MQTT_TOPIC_PREFIX", "webrtc")
		}
	}

	if s.MQTTEnabled {
		api := &mqttPubAPI{s: s, sessKeyByPath: make(map[string]uuid.UUID)}
		s.mqttSignaler = NewMQTTSignaler(s.MQTTBrokerURL, s.MQTTTopicPrefix, api,
			func(format string, args ...any) { s.Log(logger.Info, format, args...) })

		if err := s.mqttSignaler.Start(s.ctx); err != nil {
			s.Log(logger.Error, "MQTT signaling start failed: %v", err)
		}
	}

	/*
	 FPT CAMERA ADAPTER INITIALIZATION
	*/
	if os.Getenv("FPT_CAMERA_ENABLED") == "1" {
		s.Log(logger.Info, "[WebRTC] [FPT] Initializing FPT Camera Adapter")
		
		// serial := os.Getenv("FPT_CAMERA_SERIAL")
		brokerURL := os.Getenv("FPT_MQTT_BROKER")
		username := os.Getenv("FPT_MQTT_USER")
		password := os.Getenv("FPT_MQTT_PASS")
		
		if brokerURL == "" {
			s.Log(logger.Warn, "[WebRTC] [FPT] FPT_MQTT_BROKER not set (adapter disabled)")
		} else {
			s.Log(logger.Info, "[WebRTC] [FPT] Configuration:")
			s.Log(logger.Info, "[WebRTC] [FPT]   - Broker: %s", brokerURL)
			s.Log(logger.Info, "[WebRTC] [FPT]   - Username: %s", username)
			
			// Step 1: Create adapter instance
			s.fptAdapter = NewFPTCameraAdapter(brokerURL, username, password, s)
			s.fptAdapter.server = s  // Set server reference for trickle ICE
			s.Log(logger.Info, "[WebRTC] [FPT] Adapter instance created")
			
			// Step 2: Register OnOffer callback BEFORE Start()
			s.Log(logger.Info, "[WebRTC] [FPT] Registering OnOffer callback...")
			
			s.fptAdapter.SetOnOffer(func(serial, clientID, cleanedSDP string) (string, error) {
				s.Log(logger.Info, "[FPT] OnOffer CALLBACK TRIGGERED!")
				s.Log(logger.Info, "[FPT] Parameters:")
				s.Log(logger.Info, "[FPT]   - ClientID: %s", clientID)
				s.Log(logger.Info, "[FPT]   - SDP length: %d bytes", len(cleanedSDP))
				
				// DATACHANNEL SUPPORT: Allow DataChannel in SDP for camera streaming
				if strings.Contains(cleanedSDP, "m=application") {
					s.Log(logger.Info, "[FPT] DataChannel present in SDP (camera streaming support)")
				} else {
					s.Log(logger.Warn, "[FPT] No DataChannel in SDP - camera may not support streaming")
				}
				
				// Log SDP structure
				hasVideo := strings.Contains(cleanedSDP, "m=video")
				hasAudio := strings.Contains(cleanedSDP, "m=audio")
				s.Log(logger.Info, "[FPT] SDP structure:")
				s.Log(logger.Info, "[FPT]   - Video: %v", hasVideo)
				s.Log(logger.Info, "[FPT]   - Audio: %v", hasAudio)
				
				// Create path name
				pathName := fmt.Sprintf("fpt/%s", serial)
				s.Log(logger.Info, "[FPT] Dynamic path: %s", pathName)
				
				// Verify SDP ends with \r\n
				if !strings.HasSuffix(cleanedSDP, "\r\n") {
					cleanedSDP += "\r\n"
				}
				
				// Create answer channel
				answerCh := make(chan *pwebrtc.SessionDescription, 1)
				defer close(answerCh)
				
				// Create session request (MQTT mode)
				req := webRTCNewSessionReq{
					pathName:    pathName,
					remoteAddr:  fmt.Sprintf("mqtt-camera-%s", serial),
					offer:       []byte(cleanedSDP),
					publish:     true, // Camera publishes TO server
					httpRequest: nil,  // No HTTP (MQTT mode)
					res:         nil,  // No HTTP response channel
					answerCh:    answerCh, // MQTT answer channel
				}
				
				s.Log(logger.Info, "[FPT] Creating session for camera %s...", serial)
				
				res := s.newSession(req)
				
				if res.err != nil {
					s.Log(logger.Error, "[FPT] newSession() failed: %v", res.err)
					return "", res.err
				}
				
				if res.sx == nil {
					s.Log(logger.Error, "[FPT] Session is nil!")
					return "", fmt.Errorf("nil session returned")
				}
				
				sessionID := res.sx.uuid.String()[:8]
				s.Log(logger.Info, "[FPT] Session created (UUID=%s)", sessionID)
				
				// Register session for trickle ICE
				s.fptAdapter.RegisterSession(serial, pathName, res.sx.secret, clientID)
				
				s.Log(logger.Info, "[FPT] Waiting for answer from session...")
				
				select {
				case ans := <-answerCh:
					if ans == nil || ans.SDP == ""{
						s.Log(logger.Error, "[FPT] Empty answer")
						return "", fmt.Errorf("nil answer")
					}
					
					s.Log(logger.Info, "[FPT] ANSWER RECEIVED FROM SESSION!")
					s.Log(logger.Info, "[FPT]   - Length: %d bytes", len(ans.SDP))
					
					// Verify answer structure
					s.Log(logger.Info, "[FPT] Answer structure:")
					s.Log(logger.Info, "[FPT]   - Video: %v", strings.Contains(ans.SDP, "m=video"))
					s.Log(logger.Info, "[FPT]   - Audio: %v", strings.Contains(ans.SDP, "m=audio"))
					
					s.Log(logger.Info, "[FPT] OnOffer CALLBACK COMPLETE")
					
					return ans.SDP, nil
					
				case <-time.After(30 * time.Second):
					s.Log(logger.Error, "[FPT] TIMEOUT waiting for answer (30s)")
					
					// Try to close the session
					if res.sx != nil {
						res.sx.Close()
					}
					
					return "", fmt.Errorf("answer timeout (30s)")
				}
			})
			
			s.Log(logger.Info, "[WebRTC] [FPT] OnOffer callback registered")
			
			// Step 3: Start adapter (connects to MQTT)
			s.Log(logger.Info, "[WebRTC] [FPT] Starting MQTT connection...")
			
			if err := s.fptAdapter.Start(); err != nil {
				s.Log(logger.Error, "[WebRTC] [FPT] Adapter start failed: %v", err)
			} else {
				s.Log(logger.Info, "[WebRTC] [FPT] Adapter started successfully")
			}
		}
	}

	// ========================================================================
	// RECORDER INITIALIZATION
	// ========================================================================
	if os.Getenv("RECORDING_ENABLED") == "1" {
		outputDir := os.Getenv("RECORDING_OUTPUT_DIR")
		if outputDir == "" {
			outputDir = "./recordings"
		}
		
		s.Log(logger.Info, "[Recorder] Initializing recorder (output: %s)", outputDir)
		s.recorder = NewRecorder(outputDir, s)
		s.Log(logger.Info, "[Recorder] READY!")
	}

	// ========================================================================
	// TURN/STUN CONFIGURATION (DISABLED BY USER)
	// ========================================================================
	/*
	turnURL := os.Getenv("TURN_SERVER_URL")
	if turnURL == "" {
		turnURL = "turn:turn-connect.fcam.vn:3478"  // Default FPT TURN
	}

	turnUser := os.Getenv("TURN_USERNAME")
	turnPass := os.Getenv("TURN_PASSWORD")

	if turnUser != "" && turnPass != "" {
		turnServer := conf.WebRTCICEServer{
			URL:      turnURL,
			Username: turnUser,
			Password: turnPass,
		}
		s.ICEServers = append(s.ICEServers, turnServer)
		s.Log(logger.Info, "FPT TURN server: %s (user: %s)", turnURL, turnUser)
	} else {
		s.Log(logger.Warn, "TURN credentials not set - relay may fail!")
	}

	// Add FPT STUN Servers (from camera ICE servers)
	fptStunServers := []string{
		"stun:stun-connect.fcam.vn:3478",
		"stun:stunp-connect.fcam.vn:3478",
	}

	for _, stunURL := range fptStunServers {
		s.ICEServers = append(s.ICEServers, conf.WebRTCICEServer{
			URL: stunURL,
		})
		s.Log(logger.Info, "FPT STUN server: %s", stunURL)
	}

	stunServers := os.Getenv("WEBRTC_STUN_SERVERS")
	if stunServers == "" {
		stunServers = "stun:stun.l.google.com:19302"
	}
	for _, stunURL := range strings.Split(stunServers, ",") {
		stunURL = strings.TrimSpace(stunURL)
		if stunURL != "" {
			s.ICEServers = append(s.ICEServers, conf.WebRTCICEServer{
				URL: stunURL,
			})
		}
	}

	s.Log(logger.Info, "=== ICE Servers Configuration ===")
	for i, srv := range s.ICEServers {
		if srv.Username != "" {
			s.Log(logger.Info, "  [%d] %s (auth: yes)", i+1, srv.URL)
		} else {
			s.Log(logger.Info, "  [%d] %s", i+1, srv.URL)
		}
	}
	*/

	// ========================================================================
	// PUBLIC IP CONFIGURATION
	// ========================================================================
	if os.Getenv("WEBRTC_AUTO_DETECT_IP") == "1" {
		publicIP := detectPublicIP(s)
		if publicIP != "" {
			s.AdditionalHosts = append(s.AdditionalHosts, publicIP)
			s.Log(logger.Info, "Auto-detected public IP: %s", publicIP)
		}
	}

	if manualIP := os.Getenv("WEBRTC_PUBLIC_IP"); manualIP != "" {
		s.AdditionalHosts = append(s.AdditionalHosts, manualIP)
		s.Log(logger.Info, "Using manual public IP: %s", manualIP)
	}

	// ========================================================================
	// METRICS
	// ========================================================================
	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetWebRTCServer(s)
	}

	return nil
}

func detectPublicIP(s *Server) string {
    endpoints := []string{
        "https://api.ipify.org",
        "https://ifconfig.me",
        "https://icanhazip.com",
    }

    client := &http.Client{Timeout: 5 * time.Second}

    for _, endpoint := range endpoints {
        resp, err := client.Get(endpoint)
        if err != nil {
            continue
        }
        defer resp.Body.Close()

        body, err := io.ReadAll(resp.Body)
        if err != nil {
            continue
        }

        ip := strings.TrimSpace(string(body))
        if net.ParseIP(ip) != nil {
            return ip
        }
    }

    s.Log(logger.Warn, "Could not auto-detect public IP")
    return ""
}

// handleFPTCameraOffer processes SDP offer from FPT camera
// Converts Bundle ICE (camera) → MediaMTX session → SDP answer
func (s *Server) handleFPTCameraOffer(serial, clientID, cleanedSDP string) (string, error) {
	s.Log(logger.Info, "[FPT] Processing camera offer (serial=%s, clientID=%s)", serial, clientID)
	
	pathName := fmt.Sprintf("fpt/%s", serial)
	// Create answer channel for async response
	answerCh := make(chan *pwebrtc.SessionDescription, 1)
	
	// Create session request
	req := webRTCNewSessionReq{
		pathName:    pathName,
		remoteAddr:  "0.0.0.0:0",
		offer:       []byte(cleanedSDP),
		publish:     true,
		httpRequest: nil,
		answerCh:    answerCh,
		clientID: clientID,
	}
	
	// Create session via existing newSession logic
	res := s.newSession(req)
	if res.err != nil {
		s.Log(logger.Error, "[FPT] newSession failed: %v", res.err)
		return "", fmt.Errorf("newSession failed: %w", res.err)
	}
	
	s.fptAdapter.RegisterSession(serial, pathName, res.sx.secret, clientID)

	s.Log(logger.Info, "[FPT] Session created (UUID=%s), waiting for answer...", res.sx.uuid)
	
	// Wait for answer from session goroutine
	select {
	case ans := <-answerCh:
		if ans == nil || ans.SDP == "" {
			s.Log(logger.Error, "[FPT] Received empty answer")
			return "", fmt.Errorf("empty answer received")
		}
		s.Log(logger.Info, "[FPT] Got answer (SDP len=%d)", len(ans.SDP))
		return ans.SDP, nil
		
	case <-time.After(30 * time.Second):
		s.Log(logger.Error, "[FPT] Timeout waiting for answer")
		return "", fmt.Errorf("timeout waiting for answer (30s)")
	}
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[WebRTC] "+format, args...)
}

// Close closes the server.
func (s *Server) Close() {
	s.Log(logger.Info, "listener is closing")

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetWebRTCServer(nil)
	}

	if s.recorder != nil {
		s.recorder.Close()
	}
	
	if s.fptAdapter != nil {
		s.fptAdapter.Close()
	}

	s.ctxCancel()
	<-s.done
}

func (s *Server) run() {
	defer close(s.done)

	var wg sync.WaitGroup

outer:
	for {
		select {
		case req := <-s.chNewSession:
			sx := &session{
				udpReadBufferSize:     s.UDPReadBufferSize,
				udpMaxPayloadSize:     s.UDPMaxPayloadSize,
				parentCtx:             s.ctx,
				ipsFromInterfaces:     s.IPsFromInterfaces,
				ipsFromInterfacesList: s.IPsFromInterfacesList,
				additionalHosts:       s.AdditionalHosts,
				iceUDPMux:             s.iceUDPMux,
				iceTCPMux:             s.iceTCPMux,
				handshakeTimeout:      s.HandshakeTimeout,
				trackGatherTimeout:    s.TrackGatherTimeout,
				stunGatherTimeout:     s.STUNGatherTimeout,
				req:                   req,
				wg:                    &wg,
				externalCmdPool:       s.ExternalCmdPool,
				pathManager:           s.PathManager,
				parent:                s,
				payloadMaxSize:    s.UDPMaxPayloadSize,
				fragmentChunkSize: s.UDPMaxPayloadSize - 14,
			}
			sx.initialize()
			s.sessions[sx] = struct{}{}
			s.sessionsBySecret[sx.secret] = sx
			s.sessionsMutex.Lock()
			s.sessionsByPath[req.pathName] = sx
			s.sessionsMutex.Unlock()
			req.res <- webRTCNewSessionRes{sx: sx}

		case sx := <-s.chCloseSession:
			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)
			s.sessionsMutex.Lock()
			if cur, ok := s.sessionsByPath[sx.req.pathName]; ok && cur == sx {
				delete(s.sessionsByPath, sx.req.pathName)
			}
			s.sessionsMutex.Unlock()

		case req := <-s.chAddSessionCandidates:
			sx, ok := s.sessionsBySecret[req.secret]
			if !ok || sx.req.pathName != req.pathName {
				req.res <- webRTCAddSessionCandidatesRes{err: ErrSessionNotFound}
				continue
			}

			req.res <- webRTCAddSessionCandidatesRes{sx: sx}

		case req := <-s.chDeleteSession:
			sx, ok := s.sessionsBySecret[req.secret]
			if !ok || sx.req.pathName != req.pathName {
				req.res <- webRTCDeleteSessionRes{err: ErrSessionNotFound}
				continue
			}

			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)
			if cur, ok := s.sessionsByPath[sx.req.pathName]; ok && cur == sx {
				delete(s.sessionsByPath, sx.req.pathName)
			}
			sx.Close()

			req.res <- webRTCDeleteSessionRes{}

		case req := <-s.chAPISessionsList:
			data := &defs.APIWebRTCSessionList{
				Items: []*defs.APIWebRTCSession{},
			}

			for sx := range s.sessions {
				data.Items = append(data.Items, sx.apiItem())
			}

			sort.Slice(data.Items, func(i, j int) bool {
				return data.Items[i].Created.Before(data.Items[j].Created)
			})

			req.res <- serverAPISessionsListRes{data: data}

		case req := <-s.chAPISessionsGet:
			sx := s.findSessionByUUID(req.uuid)
			if sx == nil {
				req.res <- serverAPISessionsGetRes{err: ErrSessionNotFound}
				continue
			}

			req.res <- serverAPISessionsGetRes{data: sx.apiItem()}

		case req := <-s.chAPIConnsKick:
			sx := s.findSessionByUUID(req.uuid)
			if sx == nil {
				req.res <- serverAPISessionsKickRes{err: ErrSessionNotFound}
				continue
			}

			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)
			sx.Close()

			req.res <- serverAPISessionsKickRes{}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	wg.Wait()

	s.httpServer.close()

	if s.udpMuxLn != nil {
		s.udpMuxLn.Close()
	}

	if s.tcpMuxLn != nil {
		s.tcpMuxLn.Close()
	}
}

func (s *Server) findSessionByUUID(uuid uuid.UUID) *session {
	for sx := range s.sessions {
		if sx.uuid == uuid {
			return sx
		}
	}
	return nil
}

func (s *Server) generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error) {
	ret := make([]pwebrtc.ICEServer, 0, len(s.ICEServers))

	for _, server := range s.ICEServers {
		if !server.ClientOnly || clientConfig {
			if server.Username == "AUTH_SECRET" {
				expireDate := time.Now().Add(webrtcTurnSecretExpiration).Unix()

				user, err := randomTurnUser()
				if err != nil {
					return nil, err
				}

				server.Username = strconv.FormatInt(expireDate, 10) + ":" + user

				h := hmac.New(sha1.New, []byte(server.Password))
				h.Write([]byte(server.Username))

				server.Password = base64.StdEncoding.EncodeToString(h.Sum(nil))
			}

			ret = append(ret, pwebrtc.ICEServer{
				URLs:       []string{server.URL},
				Username:   server.Username,
				Credential: server.Password,
			})
		}
	}

	return ret, nil
}

// newSession is called by webRTCHTTPServer and MQTT signaler
func (s *Server) newSession(req webRTCNewSessionReq) webRTCNewSessionRes {
	req.res = make(chan webRTCNewSessionRes, 1)

	select {
	case s.chNewSession <- req:
		res := <-req.res

		// For MQTT mode: don't call sx.new() - let CreatePublisherFromOffer handle the answer flow
		if req.answerCh != nil {
			s.Log(logger.Info, "[Server] MQTT mode: session running")
			return res
		}
		s.Log(logger.Debug, "[Server] HTTP mode: calling sx.new()")
		// For HTTP mode: call sx.new() to complete the handshake
		return res.sx.new(req)

	case <-s.ctx.Done():
		return webRTCNewSessionRes{
			errStatusCode: http.StatusInternalServerError,
			err:           fmt.Errorf("terminated"),
		}
	}
}

// getSessionByPath safely retrieves a session by path name.
func (s *Server) getSessionByPath(pathName string) (*session, error) {
	s.sessionsMutex.RLock()
	defer s.sessionsMutex.RUnlock()
	
	sx, ok := s.sessionsByPath[pathName]
	if !ok {
		return nil, fmt.Errorf("session not found for path: %s", pathName)
	}
	return sx, nil
}

// closeSession is called by session.
func (s *Server) closeSession(sx *session) {
	select {
	case s.chCloseSession <- sx:
	case <-s.ctx.Done():
	}
}

// addSessionCandidates is called by webRTCHTTPServer.
func (s *Server) addSessionCandidates(
	req webRTCAddSessionCandidatesReq,
) webRTCAddSessionCandidatesRes {
	req.res = make(chan webRTCAddSessionCandidatesRes)
	select {
	case s.chAddSessionCandidates <- req:
		res1 := <-req.res
		if res1.err != nil {
			return res1
		}

		return res1.sx.addCandidates(req)

	case <-s.ctx.Done():
		return webRTCAddSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// deleteSession is called by webRTCHTTPServer.
func (s *Server) deleteSession(req webRTCDeleteSessionReq) error {
	req.res = make(chan webRTCDeleteSessionRes)
	select {
	case s.chDeleteSession <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}

// APISessionsList is called by api.
func (s *Server) APISessionsList() (*defs.APIWebRTCSessionList, error) {
	req := serverAPISessionsListReq{
		res: make(chan serverAPISessionsListRes),
	}

	select {
	case s.chAPISessionsList <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APISessionsGet is called by api.
func (s *Server) APISessionsGet(uuid uuid.UUID) (*defs.APIWebRTCSession, error) {
	req := serverAPISessionsGetReq{
		uuid: uuid,
		res:  make(chan serverAPISessionsGetRes),
	}

	select {
	case s.chAPISessionsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APISessionsKick is called by api.
func (s *Server) APISessionsKick(uuid uuid.UUID) error {
	req := serverAPISessionsKickReq{
		uuid: uuid,
		res:  make(chan serverAPISessionsKickRes),
	}

	select {
	case s.chAPIConnsKick <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}

func getenv(k, fallback string) string {
    if v := os.Getenv(k); v != "" { return v }
    return fallback
}

func ternary[T any](cond bool, a, b T) T { if cond { return a }; return b }

// APIDataChannelSend sends command to camera via DataChannel
func (s *Server) APIDataChannelSend(w http.ResponseWriter, r *http.Request) {
	// Extract path from URL (e.g., /v3/webrtc/fpt/c02i24040003273/datachannel/send)
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v3/webrtc/"), "/")
	if len(pathParts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	
	// Reconstruct path (e.g., "fpt/c02i24040003273")
	pathName := strings.Join(pathParts[:len(pathParts)-2], "/")
	
	s.Log(logger.Info, "[DataChannel-API] Send command request for path: %s", pathName)
	
	// Parse JSON command
	var cmd map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	s.Log(logger.Info, "[DataChannel-API] Command: %+v", cmd)
	
	// Get active session
	sess, err := s.getSessionByPath(pathName)
	if err != nil {
		s.Log(logger.Warn, "[DataChannel-API] Session not found: %s", pathName)
		http.Error(w, fmt.Sprintf("Session not found: %v", err), http.StatusNotFound)
		return
	}
	
	// Get PeerConnection
	sess.mutex.RLock()
	pc := sess.pc
	sess.mutex.RUnlock()
	
	if pc == nil {
		s.Log(logger.Warn, "[DataChannel-API] PeerConnection not ready")
		http.Error(w, "PeerConnection not ready", http.StatusServiceUnavailable)
		return
	}
	
	// Wait for DataChannel (timeout 5s)
	s.Log(logger.Info, "[DataChannel-API] Waiting for DataChannel...")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	
	done := make(chan error, 1)
	go func() {
		done <- pc.WaitDataChannelReady()
	}()
	
	select {
	case err := <-done:
		if err != nil {
			s.Log(logger.Error, "[DataChannel-API] DataChannel not ready: %v", err)
			http.Error(w, fmt.Sprintf("DataChannel not ready: %v", err), http.StatusServiceUnavailable)
			return
		}
	case <-ctx.Done():
		s.Log(logger.Error, "[DataChannel-API] Timeout waiting for DataChannel")
		http.Error(w, "Timeout waiting for DataChannel", http.StatusGatewayTimeout)
		return
	}
	
	// Send command
	data, _ := json.Marshal(cmd)
	err = pc.SendDataChannelMessage(data, true)
	if err != nil {
		s.Log(logger.Error, "[DataChannel-API] Failed to send: %v", err)
		http.Error(w, fmt.Sprintf("Failed to send: %v", err), http.StatusInternalServerError)
		return
	}
	
	s.Log(logger.Info, "[DataChannel-API] Command sent successfully!")
	
	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "sent",
		"command": cmd,
		"path":    pathName,
	})
}

// onDataChannelSend handles /:path/datachannel/send
func (s *httpServer) onDataChannelSend(ctx *gin.Context) {
	path := ctx.Param("path")
	s.handleDataChannelSend(ctx, path)
}

// onDataChannelSendWithSubpath handles /:path/:subpath/datachannel/send
func (s *httpServer) onDataChannelSendWithSubpath(ctx *gin.Context) {
	path := ctx.Param("path")
	subpath := ctx.Param("subpath")
	fullPath := path + "/" + subpath
	s.handleDataChannelSend(ctx, fullPath)
}

// handleDataChannelSend is the actual handler logic (shared by both routes)
func (s *httpServer) handleDataChannelSend(ctx *gin.Context, pathName string) {
	s.parent.Log(logger.Info, "[DataChannel-API] Send request for path: %s", pathName)
	
	// Parse JSON command from body
	var cmd map[string]interface{}
	if err := ctx.BindJSON(&cmd); err != nil {
		s.parent.Log(logger.Warn, "[DataChannel-API] Invalid JSON: %v", err)
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid JSON",
			"details": err.Error(),
		})
		return
	}
	
	s.parent.Log(logger.Info, "[DataChannel-API] Command: %+v", cmd)
	
	// Get active session for this path
	sess := GetActiveSession(pathName)
	if sess == nil {
		s.parent.Log(logger.Warn, "[DataChannel-API] No active session for path: %s", pathName)
		ctx.JSON(http.StatusNotFound, gin.H{
			"error": "Session not found",
			"path": pathName,
			"hint": "Trigger camera first: POST /v3/webrtc/fpt/trigger?serial=...",
			"active_sessions": ListActiveSessions(),
		})
		return
	}
	
	s.parent.Log(logger.Info, "[DataChannel-API] Found session: %s", sess.GetSessionID())
	
	// Get PeerConnection from session
	sess.mutex.RLock()
	pc := sess.pc
	sess.mutex.RUnlock()
	
	if pc == nil {
		s.parent.Log(logger.Warn, "[DataChannel-API] PeerConnection not ready")
		ctx.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "PeerConnection not ready",
			"status": "Wait for connection to establish",
		})
		return
	}
	
	// Check if DataChannel is ready
	if !pc.HasDataChannel() {
		s.parent.Log(logger.Warn, "[DataChannel-API] DataChannel not available yet")
		ctx.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "DataChannel not available",
			"status": "Wait for DataChannel to open (usually 2-3 seconds after trigger)",
		})
		return
	}
	
	// Marshal command to JSON
	data, err := json.Marshal(cmd)
	if err != nil {
		s.parent.Log(logger.Error, "[DataChannel-API] JSON marshal failed: %v", err)
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to marshal JSON",
		})
		return
	}
	
	s.parent.Log(logger.Info, "[DataChannel-API] Sending to DataChannel: %s", string(data))
	
	// Send via DataChannel
	err = pc.SendDataChannelMessage(data, true) // true = text message
	if err != nil {
		s.parent.Log(logger.Error, "[DataChannel-API] Send failed: %v", err)
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to send message",
			"details": err.Error(),
		})
		return
	}
	
	s.parent.Log(logger.Info, "[DataChannel-API] Command sent successfully")
	
	// Return success
	ctx.JSON(http.StatusOK, gin.H{
		"status": "sent",
		"command": cmd,
		"path": pathName,
		"session_id": sess.GetSessionID().String(),
		"timestamp": time.Now().Unix(),
	})
}

// APIStreamStatus checks if stream is ready for WHEP
func (s *Server) APIStreamStatus(w http.ResponseWriter, r *http.Request) {
    pathName := strings.TrimPrefix(r.URL.Path, "/v3/webrtc/")
    pathName = strings.TrimSuffix(pathName, "/status")
    
    s.Log(logger.Info, "[Stream-Status] Checking: %s", pathName)
    
    sess := GetActiveSession(pathName)
    if sess == nil {
        http.Error(w, "Session not found", http.StatusNotFound)
        return
    }
    
    // Check if session is publishing and has active stream
    if !sess.IsPublishing() {
        json.NewEncoder(w).Encode(map[string]interface{}{
            "ready": false,
            "reason": "Session created but not publishing yet",
            "path": pathName,
        })
        return
    }
    
    // Check if PeerConnection exists and is connected
    sess.mutex.RLock()
    pc := sess.pc
    sess.mutex.RUnlock()
    
    if pc == nil {
        json.NewEncoder(w).Encode(map[string]interface{}{
            "ready": false,
            "reason": "PeerConnection not initialized",
            "path": pathName,
        })
        return
    }
    
    iceState := pc.WebRTCConnection().ICEConnectionState()
    connState := pc.WebRTCConnection().ConnectionState()
    
    ready := (iceState == pwebrtc.ICEConnectionStateConnected || 
              iceState == pwebrtc.ICEConnectionStateCompleted) &&
             connState == pwebrtc.PeerConnectionStateConnected
    
    json.NewEncoder(w).Encode(map[string]interface{}{
        "ready": ready,
        "ice_state": iceState.String(),
        "connection_state": connState.String(),
        "path": pathName,
        "session_id": sess.GetSessionID().String(),
    })
}