package webrtc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	sdpv3 "github.com/pion/sdp/v3"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// Global registry of active publishing sessions
var (
	activeSessionsMutex sync.RWMutex
	activeSessions      = make(map[string]*session)
)

// RegisterSession adds a session to the global registry
func RegisterSession(pathName string, sess *session) {
	activeSessionsMutex.Lock()
	defer activeSessionsMutex.Unlock()
	activeSessions[pathName] = sess
}

// UnregisterSession removes a session from the global registry
func UnregisterSession(pathName string) {
	activeSessionsMutex.Lock()
	defer activeSessionsMutex.Unlock()
	delete(activeSessions, pathName)
}

// GetActiveSession retrieves a session by path name
func GetActiveSession(pathName string) *session {
	activeSessionsMutex.RLock()
	defer activeSessionsMutex.RUnlock()
	return activeSessions[pathName]
}

// ListActiveSessions returns all active session path names
func ListActiveSessions() []string {
	activeSessionsMutex.RLock()
	defer activeSessionsMutex.RUnlock()
	
	paths := make([]string, 0, len(activeSessions))
	for path := range activeSessions {
		paths = append(paths, path)
	}
	return paths
}

// GetSessionPeerConnection returns the PeerConnection for a session
func GetSessionPeerConnection(pathName string) *webrtc.PeerConnection {
	sess := GetActiveSession(pathName)
	if sess == nil {
		return nil
	}
	
	sess.mutex.RLock()
	defer sess.mutex.RUnlock()
	return sess.pc
}

func whipOffer(body []byte) *pwebrtc.SessionDescription {
	return &pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeOffer,
		SDP:  string(body),
	}
}

type sessionParent interface {
	closeSession(sx *session)
	generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error)
	logger.Writer
}

type session struct {
	udpReadBufferSize     uint
	parentCtx             context.Context
	ipsFromInterfaces     bool
	ipsFromInterfacesList []string
	additionalHosts       []string
	iceUDPMux             ice.UDPMux
	iceTCPMux             *webrtc.TCPMuxWrapper
	handshakeTimeout      conf.Duration
	trackGatherTimeout    conf.Duration
	stunGatherTimeout     conf.Duration
	req                   webRTCNewSessionReq
	wg                    *sync.WaitGroup
	externalCmdPool       *externalcmd.Pool
	pathManager           serverPathManager
	parent                sessionParent

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	secret    uuid.UUID
	mutex     sync.RWMutex
	pc        *webrtc.PeerConnection

	chNew           chan webRTCNewSessionReq
	chAddCandidates chan webRTCAddSessionCandidatesReq

	answerMu sync.Mutex
    answer   []byte

	// NEW: Store incoming tracks for recording
	incomingVideoTrack *pwebrtc.TrackRemote
	trackMutex         sync.RWMutex

	// NEW: WebSocket relay for DataChannel messages
	wsRelay *WebSocketDataChannelRelay

	// NEW: DataChannel-to-RTP converter
    dcConverter *webrtc.DataChannelToRTPConverter

	chunkQueue chan []byte
    queueWg    sync.WaitGroup
	queueRunning bool
}



func (s *session) initialize() {
	ctx, ctxCancel := context.WithCancel(s.parentCtx)

	s.ctx = ctx
	s.ctxCancel = ctxCancel
	s.created = time.Now()
	s.uuid = uuid.New()
	s.secret = uuid.New()
	s.chNew = make(chan webRTCNewSessionReq)
	s.chAddCandidates = make(chan webRTCAddSessionCandidatesReq)

	s.Log(logger.Info, "created by %s", s.req.remoteAddr)

	s.wg.Add(1)

	go s.run()
}

// Log implements logger.Writer.
func (s *session) Log(level logger.Level, format string, args ...any) {
	id := hex.EncodeToString(s.uuid[:4])
	s.parent.Log(level, "[session %v] "+format, append([]any{id}, args...)...)
}

func (s *session) Close() {
	UnregisterSession(s.req.pathName)
	s.ctxCancel()
}

func (s *session) run() {
	defer s.wg.Done()

	err := s.runInner()

	s.ctxCancel()

	s.parent.closeSession(s)

	s.Log(logger.Info, "closed: %v", err)
}

func (s *session) runInner() error {
    // ========================================================================
    // WAIT FOR SESSION INITIALIZATION
    // ========================================================================
    // MQTT mode: already initialized, skip wait for chNew
    // HTTP mode: wait for HTTP handler to call session.new()
    
    if s.req.answerCh != nil {
        // MQTT mode: skip chNew wait (already initialized in newSession)
        s.Log(logger.Info, "[MQTT] Session pre-initialized, proceeding directly to runInner2()")
    } else if s.req.httpRequest != nil {
        // HTTP/WHIP mode: wait for HTTP request
        s.Log(logger.Debug, "[HTTP] Waiting for session initialization...")
        select {
        case <-s.chNew:
            s.Log(logger.Debug, "[HTTP] Session initialized")
        case <-s.ctx.Done():
            return fmt.Errorf("terminated")
        }
    }

    // ========================================================================
    // RUN MAIN SESSION LOGIC
    // ========================================================================
    errStatusCode, err := s.runInner2()

    // ========================================================================
    // ERROR HANDLING
    // ========================================================================
    if errStatusCode != 0 && s.req.res != nil {
        select {
        case s.req.res <- webRTCNewSessionRes{
            errStatusCode: errStatusCode,
            err:           err,
        }:
            s.Log(logger.Debug, "[HTTP] Error response sent")
        case <-time.After(100 * time.Millisecond):
            s.Log(logger.Warn, "[HTTP] Failed to send error response (channel blocked)")
        }
    }

    return err
}

func (s *session) runInner2() (int, error) {
	s.Log(logger.Info, "=== runInner2: publish=%v, httpRequest=%v, answerCh=%v ===", 
		s.req.publish, s.req.httpRequest != nil, s.req.answerCh != nil)
	if s.req.publish {
		s.Log(logger.Info, "Going to runPublish()")
		return s.runPublish()
	}
	s.Log(logger.Info, "Going to runRead()")
	return s.runRead()
}

func (s *session) runPublish() (int, error) {
	// SAFE: Extract IP from remoteAddr
	var ip net.IP
	if s.req.remoteAddr != "" {
		host, _, err := net.SplitHostPort(s.req.remoteAddr)
		if err != nil {
			ip = net.ParseIP(s.req.remoteAddr)
		} else {
			ip = net.ParseIP(host)
		}
	}
	if ip == nil {
		ip = net.ParseIP("127.0.0.1")
	}

	// SAFE: Extract query and credentials
	q := ""
	if s.req.httpRequest != nil && s.req.httpRequest.URL != nil {
		q = s.req.httpRequest.URL.RawQuery
	}

	var creds *auth.Credentials
	if s.req.httpRequest != nil {
		creds = httpp.Credentials(s.req.httpRequest)
	}

	// 1. FIND PATH CONFIGURATION
	pathConf, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:        s.req.pathName,
			Query:       q,
			Publish:     true,
			Proto:       auth.ProtocolWebRTC,
			ID:          &s.uuid,
			Credentials: creds,
			IP:          ip,
		},
	})
	if err != nil {
		return http.StatusBadRequest, err
	}

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	// 2. CREATE PEER CONNECTION
	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               true,
		Log:                   s,
		CameraSerial: extractSerialFromPath(s.req.pathName),
	}
	
	// REMOVED: Trickle ICE disabled - all candidates bundled in answer SDP
	// pc.LocalICEHandler = func(cand string) { ... }

	// Store incoming video track for recording
	pc.OnTrackFunc = func(track *pwebrtc.TrackRemote, receiver *pwebrtc.RTPReceiver) {
		s.Log(logger.Info, "[OnTrack] Received track: kind=%s, codec=%s, id=%s",
			track.Kind(), track.Codec().MimeType, track.ID())
		
		// CRITICAL FIX: Forward track to Track-Gather channel so it doesn't timeout!
		pc.ForwardIncomingTrack(track, receiver)
		
		if track.Kind() == pwebrtc.RTPCodecTypeVideo {
			s.trackMutex.Lock()
			s.incomingVideoTrack = track
			s.trackMutex.Unlock()

			s.Log(logger.Info, "[Recording] Video track captured for path: %s", s.req.pathName)
			
			s.Log(logger.Info, "[RTP-Relay] Starting RTP to WebSocket relay...")

			// Read RTP packets and relay to WebSocket
			go func() {
				packetCount := 0
				for {
					rtpPacket, _, err := track.ReadRTP()
					if err != nil {
						s.Log(logger.Warn, "[RTP-Relay] ReadRTP error: %v", err)
						return
					}
					
					packetCount++

					// Get WebSocket relay
					s.mutex.RLock()
					relay := s.wsRelay
					s.mutex.RUnlock()

					// Relay payload to WebSocket
					if relay != nil {
						relay.BroadcastDataChannelMessage(false, rtpPacket.Payload)
						
						if packetCount == 1 {
							s.Log(logger.Info, "[RTP-Relay] First RTP packet relayed to WebSocket!")
						} else if packetCount%60 == 0 {
							s.Log(logger.Debug, "[RTP-Relay] %d packets relayed", packetCount)
						}
					}
				}
			}()
		}
		
		if track.Kind() == pwebrtc.RTPCodecTypeAudio {
			s.Log(logger.Info, "[RTP-Relay] Audio track available")
		}
	}

	// NEW: Relay DataChannel messages to WebSocket clients
	// Setup converter callback
	pc.OnDataChannelMessageFunc = func(isText bool, data []byte) {
		if !isText && s.dcConverter != nil {
			err := s.dcConverter.ProcessH264Chunk(data)
			if err != nil {
				s.Log(logger.Warn, "[Converter] Failed to process chunk: %v", err)
			}
		}
		
		// Also relay to WebSocket
		if s.wsRelay != nil {
			s.wsRelay.BroadcastDataChannelMessage(isText, data)
		}
	}

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	// Cleanup goroutine
	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	// 4. PROCESS SDP OFFER
	offer := whipOffer(s.req.offer)

	// DEBUG: Log SDP before unmarshaling
	s.Log(logger.Debug, "[DEBUG] About to unmarshal offer SDP:")
	s.Log(logger.Debug, "[DEBUG] - Length: %d bytes", len(offer.SDP))
	s.Log(logger.Debug, "[DEBUG] - Has video: %v", strings.Contains(offer.SDP, "m=video"))
	s.Log(logger.Debug, "[DEBUG] - Has audio: %v", strings.Contains(offer.SDP, "m=audio"))
	s.Log(logger.Debug, "[DEBUG] - Has application: %v", strings.Contains(offer.SDP, "m=application"))
	s.Log(logger.Debug, "[DEBUG] - Ends with \\r\\n: %v", strings.HasSuffix(offer.SDP, "\r\n"))
	
	// Print last 100 chars to see if it ends properly
	if len(offer.SDP) > 100 {
		s.Log(logger.Debug, "[DEBUG] - Last 100 chars: ...%s", offer.SDP[len(offer.SDP)-100:])
	}

	var sdp sdpv3.SessionDescription
	err = sdp.Unmarshal([]byte(offer.SDP))
	if err != nil {
		s.Log(logger.Error, "Failed to unmarshal offer SDP: %v", err)
		return http.StatusBadRequest, err
	}

	s.Log(logger.Debug, "Offer has %d media descriptions", len(sdp.MediaDescriptions))
	for i, md := range sdp.MediaDescriptions {
		s.Log(logger.Debug, "  Media %d: %s", i, md.MediaName.Media)
	}

	err = webrtc.TracksAreValid(sdp.MediaDescriptions)
	if err != nil {
		s.Log(logger.Error, "Tracks validation failed: %v", err)
		return http.StatusNotAcceptable, err
	}

	// 5. CREATE ANSWER
	s.Log(logger.Info, "Creating full answer from offer...")
	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		s.Log(logger.Error, "CreateFullAnswer failed: %v", err)
		return http.StatusBadRequest, err
	}

	// FIX: CreateFullAnswer() ALREADY waits for gathering and returns modified Answer
	// No need to wait again or call LocalDescription() - that would retrieve ORIGINAL unmodified Answer
	s.Log(logger.Info, "[FPT-Fix] CreateFullAnswer returned modified Answer with ice-options + SSRC + candidates")

	candidateCount := strings.Count(answer.SDP, "a=candidate:")
	s.Log(logger.Info, "Final answer contains %d ICE candidates", candidateCount)

	s.Log(logger.Info, "=== ANSWER SDP DEBUG ===")
	lines := strings.Split(answer.SDP, "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "a=candidate:") {
			s.Log(logger.Info, "  %s", line)
		}
		// CRITICAL: Check m=video/m=audio lines to see if track is accepted or rejected
		if strings.HasPrefix(line, "m=video") {
			s.Log(logger.Warn, "[VIDEO] %s", line)
			if strings.Contains(line, "m=video 9 ") || strings.Contains(line, "m=video 0 ") {
				s.Log(logger.Error, "[VIDEO] REJECTED! Port 9 or 0 means track was rejected by Pion")
			} else {
				s.Log(logger.Info, "[VIDEO] ACCEPTED! Port is not 9/0")
			}
		}
		if strings.HasPrefix(line, "m=audio") {
			s.Log(logger.Warn, "[AUDIO] %s", line)
			if strings.Contains(line, "m=audio 9 ") || strings.Contains(line, "m=audio 0 ") {
				s.Log(logger.Error, "[AUDIO] REJECTED! Port 9 or 0 means track was rejected by Pion")
			} else {
				s.Log(logger.Info, "[AUDIO] ACCEPTED! Port is not 9/0")
			}
		}
	}
	s.Log(logger.Info, "=== END ANSWER ===")
	
	if strings.Contains(answer.SDP, "m=video") {
		s.Log(logger.Debug, "Video track present")
	}

	if strings.Contains(answer.SDP, "m=audio") {
		s.Log(logger.Debug, "Audio track present")
	}

	// 6. SEND ANSWER BASED ON MODE
	if s.req.httpRequest != nil {
		// HTTP/WHIP MODE
		s.writeAnswer(answer)
	} else if s.req.answerCh != nil {
		// MQTT MODE: Send with panic protection
		s.Log(logger.Info, "[MQTT] Sending answer to channel...")
		
		answerSent := make(chan bool, 1)
		answerErr := make(chan error, 1)
		
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.Log(logger.Error, "[MQTT] PANIC recovered: channel closed, %v", r)
					answerErr <- fmt.Errorf("channel closed: %v", r)
				}
			}()
			
			select {
			case s.req.answerCh <- answer:
				answerSent <- true
			case <-time.After(3 * time.Second):
				answerSent <- false
			}
		}()
		
		select {
		case err := <-answerErr:
			s.Log(logger.Error, "[MQTT] Failed to send answer: %v", err)
			return http.StatusInternalServerError, err
		case success := <-answerSent:
			if !success {
				s.Log(logger.Error, "[MQTT] Timeout sending answer (3s)")
				return http.StatusInternalServerError, fmt.Errorf("answer channel timeout")
			}
		}
		
		s.Log(logger.Info, "[MQTT] Answer sent successfully")
	} else {
		return http.StatusBadRequest, fmt.Errorf("No answer sink available")
	}

	// 7. START ICE CANDIDATE READER
	s.Log(logger.Info, "Starting remote candidate reader...")
	go s.readRemoteCandidates(pc)

	// 7.5 REGISTER SESSION (for ICE handling)
	RegisterSession(s.req.pathName, s)
	s.Log(logger.Info, "Session registered early for ICE: %s", s.req.pathName)

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	// 8. WAIT FOR CONNECTION WITH MONITORING
	s.Log(logger.Info, "Waiting for peer connection (timeout: %s)...", s.handshakeTimeout)
	
	// Create done channel for WaitUntilConnected
	connDone := make(chan error, 1)
	go func() {
		connDone <- pc.WaitUntilConnected()
	}()
	
	// Status ticker to show ICE progress
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()
	
	// Timeout timer
	timeoutTimer := time.NewTimer(time.Duration(s.handshakeTimeout))
	defer timeoutTimer.Stop()
	
	for {
		select {
		case err := <-connDone:
			if err != nil {
				s.Log(logger.Error, "WaitUntilConnected failed: %v", err)
				return 0, err
			}
			s.Log(logger.Info, "Peer connection established successfully")
			goto connectionEstablished
			
		case <-statusTicker.C:
			// Log ICE state every 2 seconds
			iceState := pc.WebRTCConnection().ICEConnectionState()
			peerState := pc.WebRTCConnection().ConnectionState()
			
			s.Log(logger.Info, "Waiting... ICE: %s, Peer: %s", iceState, peerState)
			
			// Check if stuck in checking for too long
			if iceState == pwebrtc.ICEConnectionStateChecking {
				s.Log(logger.Warn, "ICE still checking - connectivity may be blocked")
			}
			
		case <-timeoutTimer.C:
			iceState := pc.WebRTCConnection().ICEConnectionState()
			s.Log(logger.Error, "CONNECTION TIMEOUT after %s", s.handshakeTimeout)
			s.Log(logger.Error, "Final ICE state: %s", iceState)
			
			// Diagnose why ICE failed
			if iceState == pwebrtc.ICEConnectionStateChecking {
				s.Log(logger.Error, "ICE STUCK in checking state - possible causes:")
				s.Log(logger.Error, "  1. Firewall blocking UDP port 8189")
				s.Log(logger.Error, "  2. NAT type incompatibility (symmetric NAT)")
				s.Log(logger.Error, "  3. TURN server not reachable")
				s.Log(logger.Error, "  4. Camera behind NAT without proper port forwarding")
			}
			
			return 0, fmt.Errorf("ICE connection timeout - stuck in %s state", iceState)
			
		case <-pc.Failed():
			s.Log(logger.Error, "Peer connection failed during handshake")
			return 0, fmt.Errorf("peer connection failed")
		}
	}
	
connectionEstablished:
	// Connection established, now gathering tracks...
	s.Log(logger.Info, "Connection established, now gathering tracks...")

	var psdp sdpv3.SessionDescription
	psdp.Unmarshal([]byte(offer.SDP)) //nolint:errcheck

	// REMOVED: DataChannel-only mode detection
	// Camera ALWAYS streams via RTP, not DataChannel!

	// Initialize converter (for future use if needed)
	// if pc.DataChannel() != nil {
	// 	s.Log(logger.Info, "[DataChannel] Available for commands")
	// 	s.dcConverter = webrtc.NewDataChannelToRTPConverter(s)
	// }

	// Wait for DataChannel to be ready first
	s.Log(logger.Info, "[DataChannel] Waiting for camera DataChannel...")
	if err := pc.WaitDataChannelReady(); err != nil {
		s.Log(logger.Error, "[DataChannel] Failed: %v", err)
		return 0, fmt.Errorf("DataChannel not ready: %w", err)
	}

	s.Log(logger.Info, "[DataChannel] Ready! Now starting converter...")

	// ========================================================================
	// ASYNC DATACHANNEL PROCESSOR
	// ========================================================================
	s.Log(logger.Info, "[DataChannel] Starting async chunk processor...")

	// Create buffered channel for chunks (buffer 100 chunks = ~1-2 seconds)
	s.chunkQueue = make(chan []byte, 500)
	s.queueRunning = true

	// Start async processor goroutine
	s.queueWg.Add(1)
	go func() {
		defer s.queueWg.Done()
		
		chunkCount := 0
		droppedCount := 0
		
		for chunk := range s.chunkQueue {
			chunkCount++
			
			// 1. Convert H.264 chunk → RTP packets
			if s.dcConverter != nil {
				if err := s.dcConverter.ProcessH264Chunk(chunk); err != nil {
					s.Log(logger.Warn, "[Converter] Failed chunk #%d: %v", chunkCount, err)
				}
			}
			
			// 2. Broadcast to WebSocket clients
			if s.wsRelay != nil {
				s.wsRelay.BroadcastDataChannelMessage(false, chunk)
			}
			
			// Log progress every 60 chunks
			if chunkCount%60 == 0 {
				s.Log(logger.Debug, "[Async] Processed %d chunks (dropped: %d)", chunkCount, droppedCount)
			}
		}
		
		s.Log(logger.Info, "[Async] Processor stopped. Total processed: %d chunks", chunkCount)
	}()

	s.Log(logger.Info, "[DataChannel] Async processor started (buffer: 500 chunks)")

	// Initialize AND START converter
	s.dcConverter = webrtc.NewDataChannelToRTPConverter(s)

	// Setup converter to handle incoming binary chunks
	// Setup NON-BLOCKING DataChannel handler
	pc.OnDataChannelMessageFunc = func(isText bool, data []byte) {
		if isText {
			// Handle text messages immediately (commands/responses)
			s.Log(logger.Debug, "[DataChannel] Text: %s", string(data))
			
			if s.wsRelay != nil {
				s.wsRelay.BroadcastDataChannelMessage(true, data)
			}
			return
		}
		
		// Binary data = H.264 chunk from camera
		// Queue for async processing (NON-BLOCKING!)
		if s.queueRunning {
			select {
			case s.chunkQueue <- data:
				// Successfully queued - handler returns immediately
				
			default:
				// Queue full - drop chunk and log warning
				s.Log(logger.Warn, "[DataChannel] Queue FULL! Dropping chunk (%d bytes) - increase buffer or optimize processing", len(data))
			}
		} else {
			s.Log(logger.Warn, "[DataChannel] Processor not running, chunk dropped")
		}
	}

	s.Log(logger.Info, "[Converter] Started! H.264 chunks will be converted to RTP")

	// Send Stream command to camera
	cameraSerial := extractSerialFromPath(s.req.pathName)
	streamCmd := map[string]interface{}{
		"Id":      cameraSerial,
		"Command": "Stream",
		"Type":    "Request",
		"Content": map[string]interface{}{
			"Resolution": 0, // Highest resolution
			"Option":     1,
		},
	}

	cmdJSON, _ := json.Marshal(streamCmd)
	s.Log(logger.Info, "[DataChannel] Sending Stream command: %s", string(cmdJSON))

	if err := pc.SendDataChannelMessage(cmdJSON, true); err != nil {
		s.Log(logger.Error, "[DataChannel] Failed to send: %v", err)
		return 0, fmt.Errorf("failed to send Stream command: %w", err)
	}

	s.Log(logger.Info, "[DataChannel] Stream command sent! Waiting for H.264 chunks...")


	// ========================================================================
	// GATHER INCOMING RTP TRACKS (ALWAYS!)
	// ========================================================================
	s.Log(logger.Info, "[Track-Gather] Waiting for RTP tracks from camera...")
	err = pc.GatherIncomingTracks()
	if err != nil {
		return 0, err
	}

	s.Log(logger.Info, "[Track-Gather] Received %d tracks", len(pc.IncomingTracks()))

	// ========================================================================
	// CREATE STREAM FROM RTP TRACKS
	// ========================================================================
	incomingTracks := pc.IncomingTracks()
	var stream *stream.Stream
	var medias []*description.Media

	if len(incomingTracks) == 0 {
		return 0, fmt.Errorf("no tracks received from camera")
	}

	medias, err = webrtc.ToStream(pc, pathConf, &stream, s)
	if err != nil {
		return 0, err
	}

	s.Log(logger.Info, "[Stream] Created from %d RTP tracks", len(incomingTracks))

	// ========================================================================
	// ADD PUBLISHER TO PATH
	// ========================================================================
	var path defs.Path

	path, stream, err = s.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:             s,
		Desc:               &description.Session{Medias: medias},
		GenerateRTPPackets: true,
		FillNTP:            !pathConf.UseAbsoluteTimestamp,
		ConfToCompare:      pathConf,
		AccessRequest: defs.PathAccessRequest{
			Name:     s.req.pathName,
			Query:    q,
			Publish:  true,
			SkipAuth: true,
		},
	})
	if err != nil {
		return 0, err
	}
	defer path.RemovePublisher(defs.PathRemovePublisherReq{Author: s})

	// Start reading RTP packets
	pc.StartReading()
	s.Log(logger.Info, "Publisher registered and STREAM READY on path '%s'", s.req.pathName)
	s.Log(logger.Info, "WHEP clients can now connect to: http://localhost:8889/%s/whep", s.req.pathName)

	// Store PeerConnection
	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	// Register session for ICE handling
	RegisterSession(s.req.pathName, s)
	defer UnregisterSession(s.req.pathName)

	// Cleanup async processor on exit
	defer func() {
		if s.queueRunning {
			s.Log(logger.Info, "[DataChannel] Stopping async processor...")
			s.queueRunning = false
			
			close(s.chunkQueue)
			s.queueWg.Wait()
			
			s.Log(logger.Info, "[DataChannel] Async processor stopped cleanly")
		}
	}()

	// Create WebSocket relay if MQTT mode
	isMQTTSession := s.req.httpRequest == nil && s.req.answerCh != nil
	if isMQTTSession {
		s.mutex.Lock()
		s.wsRelay = NewWebSocketDataChannelRelay(s, s.parent)
		s.mutex.Unlock()
		s.Log(logger.Info, "WebSocket relay ready for DataChannel messages")
	}

	// ========================================================================
	// WAIT UNTIL SESSION ENDS
	// ========================================================================
	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")
		
	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

// ADD HELPER METHOD: Get stored video track
func (s *session) GetVideoTrack() *pwebrtc.TrackRemote {
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()
	return s.incomingVideoTrack
}

func extractSerialFromPath(pathName string) string {
	if !strings.HasPrefix(pathName, "fpt/") {
		return "unknown"
	}
	
	serial := strings.TrimPrefix(pathName, "fpt/")
	
	serial = strings.TrimSuffix(serial, "/")
	
	if serial == "" {
		return "unknown"
	}
	
	return serial
}


func (s *session) runRead() (int, error) {
	// SAFE: Extract IP from remoteAddr
	var ip net.IP
	if s.req.remoteAddr != "" {
		host, _, err := net.SplitHostPort(s.req.remoteAddr)
		if err != nil {
			// remoteAddr might not have port, use as-is
			ip = net.ParseIP(s.req.remoteAddr)
		} else {
			ip = net.ParseIP(host)
		}
	}
	if ip == nil {
		ip = net.ParseIP("127.0.0.1") // Fallback
	}

	// SAFE: Extract query only if httpRequest exists
	q := ""
	if s.req.httpRequest != nil && s.req.httpRequest.URL != nil {
		q = s.req.httpRequest.URL.RawQuery
	}

	// SAFE: Extract credentials only if httpRequest exists
	var creds *auth.Credentials
	if s.req.httpRequest != nil {
		creds = httpp.Credentials(s.req.httpRequest)
	}

	req := defs.PathAccessRequest{
		Name:        s.req.pathName,
		Query:       q,
		Proto:       auth.ProtocolWebRTC,
		ID:          &s.uuid,
		Credentials: creds,
		IP:          ip,
	}

	path, strm, err := s.pathManager.AddReader(defs.PathAddReaderReq{
		Author:        s,
		AccessRequest: req,
	})
	if err != nil {
		var terr2 defs.PathNoStreamAvailableError
		if errors.As(err, &terr2) {
			return http.StatusNotFound, err
		}

		return http.StatusBadRequest, err
	}

	defer path.RemoveReader(defs.PathRemoveReaderReq{Author: s})

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               false,
		OutgoingTracks:        nil,
		Log:                   s,
	}

	r := &stream.Reader{Parent: s}

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	err = webrtc.FromStream(strm.Desc, r, pc)
	if err != nil {
		return http.StatusBadRequest, err
	}
	// REMOVED: Trickle ICE disabled - all candidates bundled in answer SDP

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	s.Log(logger.Info, "is reading from path '%s', %s",
		path.Name(), defs.FormatsInfo(r.Formats()))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          s,
		ExternalCmdPool: s.externalCmdPool,
		Conf:            path.SafeConf(),
		ExternalCmdEnv:  path.ExternalCmdEnv(),
		Reader:          s.APIReaderDescribe(),	
		Query:           q,
	})
	defer onUnreadHook()

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case err = <-r.Error():
		return 0, err

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) writeAnswer(answer *pwebrtc.SessionDescription) {
	// ========================================================================
	// STORE ANSWER (for API access)
	// ========================================================================
	s.answerMu.Lock()
	s.answer = []byte(answer.SDP)
	s.answerMu.Unlock()

	// ========================================================================
	// ROUTE ANSWER BASED ON MODE
	// ========================================================================
	
	// MQTT Mode: answerCh already handled in runPublish()
	if s.req.answerCh != nil {
		s.Log(logger.Debug, "[MQTT] Answer stored (already sent via answerCh)")
		return
	}

	// HTTP/WHIP Mode: send to res channel
	if s.req.res != nil {
		s.Log(logger.Debug, "[HTTP] Sending answer to res channel")
		select {
		case s.req.res <- webRTCNewSessionRes{
			sx:     s,
			answer: []byte(answer.SDP),
		}:
			s.Log(logger.Debug, "[HTTP] Answer sent successfully")
		case <-time.After(2 * time.Second):
			s.Log(logger.Error, "[HTTP] Timeout sending answer to res channel")
		}
	} else {
		s.Log(logger.Warn, "[UNKNOWN] No answer destination (neither answerCh nor res)")
	}
}

func (s *session) readRemoteCandidates(pc *webrtc.PeerConnection) {
	s.Log(logger.Info, "[ICE] Starting remote candidate reader for path: %s", len(s.req.pathName))
	candidateCount := 0

	for {
		select {
		case req := <-s.chAddCandidates:
			s.Log(logger.Info, "[ICE] Received %d remote candidates", len(req.candidates))

			for i, candidate := range req.candidates {
				candidateCount++
				s.Log(logger.Info, "[ICE] Adding remote candidate #%d (total=%d): %s",
					i+1, candidateCount, candidate.Candidate)

				err := pc.AddRemoteCandidate(candidate)
				if err != nil {
					s.Log(logger.Error, "[ICE] Failed to add remote candidate #%d: %v", i+1, err)
					req.res <- webRTCAddSessionCandidatesRes{err: err}
					continue
				}

				s.Log(logger.Debug, "[ICE] Remote candidate #%d added successfully", i+1)
			}
			req.res <- webRTCAddSessionCandidatesRes{}

		case <-s.ctx.Done():
			s.Log(logger.Info, "[ICE] Remote candidate reader stopped (total received: %d)", candidateCount)
			return
		}
	}
}

// new is called by webRTCHTTPServer through Server.
func (s *session) new(req webRTCNewSessionReq) webRTCNewSessionRes {
	select {
	case s.chNew <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCNewSessionRes{err: fmt.Errorf("terminated"), errStatusCode: http.StatusInternalServerError}
	}
}

// addCandidates is called by webRTCHTTPServer through Server.
func (s *session) addCandidates(
	req webRTCAddSessionCandidatesReq,
) webRTCAddSessionCandidatesRes {
	select {
	case s.chAddCandidates <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCAddSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// APIReaderDescribe implements reader.
func (s *session) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "webRTCSession",
		ID:   s.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (s *session) APISourceDescribe() defs.APIPathSourceOrReader {
	return s.APIReaderDescribe()
}

func (s *session) apiItem() *defs.APIWebRTCSession {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	peerConnectionEstablished := false
	localCandidate := ""
	remoteCandidate := ""
	bytesReceived := uint64(0)
	bytesSent := uint64(0)
	rtpPacketsReceived := uint64(0)
	rtpPacketsSent := uint64(0)
	rtpPacketsLost := uint64(0)
	rtpPacketsJitter := float64(0)
	rtcpPacketsReceived := uint64(0)
	rtcpPacketsSent := uint64(0)

	if s.pc != nil {
		peerConnectionEstablished = true
		localCandidate = s.pc.LocalCandidate()
		remoteCandidate = s.pc.RemoteCandidate()
		stats := s.pc.Stats()
		bytesReceived = stats.BytesReceived
		bytesSent = stats.BytesSent
		rtpPacketsReceived = stats.RTPPacketsReceived
		rtpPacketsSent = stats.RTPPacketsSent
		rtpPacketsLost = stats.RTPPacketsLost
		rtpPacketsJitter = stats.RTPPacketsJitter
		rtcpPacketsReceived = stats.RTCPPacketsReceived
		rtcpPacketsSent = stats.RTCPPacketsSent
	}

	// Safe query extraction
	query := ""
	if s.req.httpRequest != nil && s.req.httpRequest.URL != nil {
		query = s.req.httpRequest.URL.RawQuery
	}

	return &defs.APIWebRTCSession{
		ID:                        s.uuid,
		Created:                   s.created,
		RemoteAddr:                s.req.remoteAddr,
		PeerConnectionEstablished: peerConnectionEstablished,
		LocalCandidate:            localCandidate,
		RemoteCandidate:           remoteCandidate,
		State: func() defs.APIWebRTCSessionState {
			if s.req.publish {
				return defs.APIWebRTCSessionStatePublish
			}
			return defs.APIWebRTCSessionStateRead
		}(),
		Path:                s.req.pathName,
		Query:               query, // Safe variable instead of direct access
		BytesReceived:       bytesReceived,
		BytesSent:           bytesSent,
		RTPPacketsReceived:  rtpPacketsReceived,
		RTPPacketsSent:      rtpPacketsSent,
		RTPPacketsLost:      rtpPacketsLost,
		RTPPacketsJitter:    rtpPacketsJitter,
		RTCPPacketsReceived: rtcpPacketsReceived,
		RTCPPacketsSent:     rtcpPacketsSent,
	}
}

func (s *session) IsPublishing() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.pc != nil && s.req.publish
}

func (s *session) GetPathName() string {
	return s.req.pathName
}

func (s *session) GetSessionID() uuid.UUID {
	return s.uuid
}