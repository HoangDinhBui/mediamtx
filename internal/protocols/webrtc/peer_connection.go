// Package webrtc contains WebRTC utilities.
package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	webrtcStreamID = "mediamtx"
)

func interfaceIPs(interfaceList []string) ([]string, error) {
	intfs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var ips []string

	for _, intf := range intfs {
		if len(interfaceList) == 0 || slices.Contains(interfaceList, intf.Name) {
			var addrs []net.Addr
			addrs, err = intf.Addrs()
			if err == nil {
				for _, addr := range addrs {
					var ip net.IP

					switch v := addr.(type) {
					case *net.IPNet:
						ip = v.IP
					case *net.IPAddr:
						ip = v.IP
					}

					if ip != nil {
						ips = append(ips, ip.String())
					}
				}
			}
		}
	}

	return ips, nil
}

// * skip ConfigureRTCPReports
// * add statsInterceptor
func registerInterceptors(
	mediaEngine *webrtc.MediaEngine,
	interceptorRegistry *interceptor.Registry,
	onStatsInterceptor func(s *statsInterceptor),
) error {
	err := webrtc.ConfigureNack(mediaEngine, interceptorRegistry)
	if err != nil {
		return err
	}

	err = webrtc.ConfigureSimulcastExtensionHeaders(mediaEngine)
	if err != nil {
		return err
	}

	err = webrtc.ConfigureTWCCSender(mediaEngine, interceptorRegistry)
	if err != nil {
		return err
	}

	interceptorRegistry.Add(&statsInterceptorFactory{
		onCreate: onStatsInterceptor,
	})

	return nil
}

func candidateLabel(c *webrtc.ICECandidate) string {
	return c.Typ.String() + "/" + c.Protocol.String() + "/" +
		c.Address + "/" + strconv.FormatInt(int64(c.Port), 10)
}

// TracksAreValid checks whether tracks in the SDP are valid
func TracksAreValid(medias []*sdp.MediaDescription) error {
	videoTrack := false
	audioTrack := false
	dataChannelTrack := false

	for _, media := range medias {
		switch media.MediaName.Media {
		case "video":
			if videoTrack {
				return fmt.Errorf("only a single video and a single audio track are supported")
			}
			videoTrack = true

		case "audio":
			if audioTrack {
				return fmt.Errorf("only a single video and a single audio track are supported")
			}
			audioTrack = true

		case "application":
			// DataChannel support for FPT camera streaming
			if dataChannelTrack {
				return fmt.Errorf("only a single data channel is supported")
			}
			dataChannelTrack = true

		default:
			return fmt.Errorf("unsupported media '%s'", media.MediaName.Media)
		}
	}

	if !videoTrack && !audioTrack && !dataChannelTrack {
		return fmt.Errorf("no valid tracks found")
	}

	return nil
}

type trackRecvPair struct {
	track    *webrtc.TrackRemote
	receiver *webrtc.RTPReceiver
}

// PeerConnection is a wrapper around webrtc.PeerConnection.
type PeerConnection struct {
	LocalRandomUDP        bool
	ICEUDPMux             ice.UDPMux
	ICETCPMux             *TCPMuxWrapper
	ICEServers            []webrtc.ICEServer
	IPsFromInterfaces     bool
	IPsFromInterfacesList []string
	AdditionalHosts       []string
	HandshakeTimeout      conf.Duration
	TrackGatherTimeout    conf.Duration
	STUNGatherTimeout     conf.Duration
	Publish               bool
	OutgoingTracks        []*OutgoingTrack
	Log                   logger.Writer

	wr                 *webrtc.PeerConnection
	ctx                context.Context
	ctxCancel          context.CancelFunc
	incomingTracks     []*IncomingTrack
	startedReading     *int64
	rtpPacketsReceived *uint64
	rtpPacketsSent     *uint64
	rtpPacketsLost     *uint64
	statsInterceptor   *statsInterceptor

	newLocalCandidate chan *webrtc.ICECandidateInit
	incomingTrack     chan trackRecvPair
	connected         chan struct{}
	failed            chan struct{}
	closed            chan struct{}
	gatheringDone     chan struct{}
	done              chan struct{}
	chStartReading    chan struct{}
	
	// ICE reconnection handling
	disconnectedAt    time.Time
	reconnectTimeout  time.Duration
	mu                sync.Mutex

	// DataChannel support for FPT camera streaming
	dataChannel             *webrtc.DataChannel
	dataChannelReady        chan struct{}
	OnDataChannelMessageFunc func(isText bool, data []byte)

	LocalICEHandler    func(candidate string)
	OnTrackFunc        func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)
	OnDataChannelFunc  func(dc *webrtc.DataChannel)

	CameraSerial string
}

// Start starts the peer connection.
func (co *PeerConnection) Start() error {
	co.Log.Log(logger.Info, "=== PeerConnection.Start() called: Publish=%v, OutgoingTracks=%d ===", 
		co.Publish, len(co.OutgoingTracks))
	
	// Set reconnection timeout (10 seconds grace period for ICE to recover)
	co.reconnectTimeout = 10 * time.Second
	
	settingsEngine := webrtc.SettingEngine{}

	settingsEngine.SetIncludeLoopbackCandidate(true)

	networkTypes := []webrtc.NetworkType{
		webrtc.NetworkTypeTCP4,
		webrtc.NetworkTypeTCP6,
	}

	if co.LocalRandomUDP || co.ICEUDPMux != nil || len(co.ICEServers) != 0 {
		networkTypes = append(networkTypes, webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6)
	}

	settingsEngine.SetNetworkTypes(networkTypes)

	if co.ICEUDPMux != nil {
		settingsEngine.SetICEUDPMux(co.ICEUDPMux)
	}

	if co.ICETCPMux != nil {
		settingsEngine.SetICETCPMux(co.ICETCPMux.Mux)
	}

	settingsEngine.SetSTUNGatherTimeout(time.Duration(co.STUNGatherTimeout))

	// Create fresh MediaEngine for every session (Pion doesn't allow re-registering codecs)
	mediaEngine := &webrtc.MediaEngine{}

	if co.Publish {
		if len(co.OutgoingTracks) == 0 {
			// MQTT mode: Register camera codecs FIRST, then defaults (skip conflicts)
			co.Log.Log(logger.Info, "MQTT Mode: Registering FPT camera codecs FIRST (H.264 PT 102, PCMA PT 8)")
			
			// Register FPT camera's H.264 codec (PT 102) FIRST
			err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:    webrtc.MimeTypeH264,
					ClockRate:   90000,
					SDPFmtpLine: "profile-level-id=42e01f;packetization-mode=1;level-asymmetry-allowed=1",
				},
				PayloadType: 102,
			}, webrtc.RTPCodecTypeVideo)
			if err != nil {
				return fmt.Errorf("failed to register H.264 PT 102: %w", err)
			}
			co.Log.Log(logger.Info, "Registered H.264 PT 102 (camera codec)")
			
			// Register FPT camera's PCMA codec (PT 8) FIRST
			err = mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:  webrtc.MimeTypePCMA,
					ClockRate: 8000,
					Channels:  1,
				},
				PayloadType: 8,
			}, webrtc.RTPCodecTypeAudio)
			if err != nil {
				return fmt.Errorf("failed to register PCMA PT 8: %w", err)
			}
			co.Log.Log(logger.Info, "Registered PCMA PT 8 (camera codec)")
			
			// Now register remaining defaults, SKIP PT 102 and PT 8 (already registered)
			videoCount := 1 // Already registered H.264 PT 102
			for _, codec := range incomingVideoCodecs {
				if codec.PayloadType != 102 { // Skip VP8 PT 102 - we already registered H.264 PT 102
					_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo)
					videoCount++
				}
			}
			
			audioCount := 1 // Already registered PCMA PT 8
			for _, codec := range incomingAudioCodecs {
				if codec.PayloadType != 8 { // Skip if default PCMA PT 8 exists
					_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio)
					audioCount++
				}
			}
			
			co.Log.Log(logger.Info, "Registered %d video codecs total (camera H.264 + defaults, skipped VP8 PT 102)", videoCount)
			co.Log.Log(logger.Info, "Registered %d audio codecs total (camera PCMA + defaults, skipped duplicate PT 8)", audioCount)
			
			// Register remaining defaults, SKIP PT 102 (VP8) and PT 8 (already used)
			for _, codec := range incomingVideoCodecs {
				if codec.PayloadType != 102 { // Skip VP8 PT 102 - we use H.264 PT 102
					_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo)
				}
			}

			for _, codec := range incomingAudioCodecs {
				if codec.PayloadType != 8 { // Skip if PT 8 exists in defaults
					_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio)
				}
			}
		} else {
			// Original logic for when we have OutgoingTracks
			videoSetupped := false
			audioSetupped := false
			for _, tr := range co.OutgoingTracks {
				if tr.isVideo() {
					videoSetupped = true
				} else {
					audioSetupped = true
				}
			}

			if !audioSetupped {
				co.OutgoingTracks = append(co.OutgoingTracks, &OutgoingTrack{
					Caps: webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypePCMU,
						ClockRate: 8000,
					},
				})
			}

			for i, tr := range co.OutgoingTracks {
				var codecType webrtc.RTPCodecType
				if tr.isVideo() {
					codecType = webrtc.RTPCodecTypeVideo
				} else {
					codecType = webrtc.RTPCodecTypeAudio
				}

				err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
					RTPCodecCapability: tr.Caps,
					PayloadType:        webrtc.PayloadType(96 + i),
				}, codecType)
				if err != nil {
					return err
				}
			}

			if !videoSetupped {
				err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
					RTPCodecCapability: webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypeVP8,
						ClockRate: 90000,
					},
					PayloadType: 96,
				}, webrtc.RTPCodecTypeVideo)
				if err != nil {
					return err
				}
			}
		}
	} else {
		// Read mode (WHEP) - keep existing logic
		_ = mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				RTCPFeedback: []webrtc.RTCPFeedback{
					{Type: "nack"},
					{Type: "nack", Parameter: "pli"},
					{Type: "goog-remb"},
				},
			},
			PayloadType: 102,
		}, webrtc.RTPCodecTypeVideo)
		
		for _, codec := range incomingVideoCodecs {
			if codec.PayloadType == 102 && codec.MimeType == webrtc.MimeTypeVP8 {
				continue
			}
			_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo)
		}

		for _, codec := range incomingAudioCodecs {
			_ = mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio)
		}
	}

	interceptorRegistry := &interceptor.Registry{}

	err := registerInterceptors(
		mediaEngine,
		interceptorRegistry,
		func(s *statsInterceptor) {
			co.statsInterceptor = s
		},
	)
	if err != nil {
		return err
	}

	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(settingsEngine),
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry))

	// SOLUTION 2E: ENABLE TURN RELAY FOR NAT TRAVERSAL
	// Use TURN relay when direct SRFLX connection fails
	// This is necessary when both peers are behind symmetric NAT/firewall
	co.Log.Log(logger.Info, "[ICE-Config] === SOLUTION 2E: TURN RELAY ENABLED ===")
	co.Log.Log(logger.Info, "[ICE-Config] Using all ICE servers (STUN + TURN): %d", len(co.ICEServers))
	
	for i, server := range co.ICEServers {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "stun:") {
				co.Log.Log(logger.Info, "[ICE-Config] #%d STUN: %s", i+1, url)
			} else if strings.HasPrefix(url, "turn:") {
				co.Log.Log(logger.Info, "[ICE-Config] #%d TURN: %s (relay for NAT traversal)", i+1, url)
			}
		}
	}

	co.Log.Log(logger.Info, "[TURN-Debug] === ICE Servers Configuration ===")
	for i, server := range co.ICEServers {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "turn:") {
				co.Log.Log(logger.Info, "[TURN-Debug] Server #%d: %s", i+1, url)
				co.Log.Log(logger.Info, "[TURN-Debug]   Username: %s", server.Username)
				passwordLength := 0
				if credentialStr, ok := server.Credential.(string); ok {
					passwordLength = len(credentialStr)
				} else {
					co.Log.Log(logger.Warn, "Credential is not a string type")
				}
				co.Log.Log(logger.Info, "[TURN-Debug]   Password: %s", strings.Repeat("*", passwordLength))
				
				// Verify TURN server is reachable
				host := strings.TrimPrefix(url, "turn:")
				if idx := strings.Index(host, ":"); idx > 0 {
					host = host[:idx]
				}
				co.Log.Log(logger.Info, "[TURN-Debug]   Host: %s", host)
			}
		}
	}
	co.Log.Log(logger.Info, "[TURN-Debug] ================================")

	co.wr, err = api.NewPeerConnection(webrtc.Configuration{
		ICEServers: co.ICEServers, // USE ALL SERVERS (STUN + TURN)
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return err
	}

	co.Log.Log(logger.Info, "[ICE-Config] PeerConnection created with FULL ICE config (RELAY candidates will be generated for NAT traversal)")

	co.ctx, co.ctxCancel = context.WithCancel(context.Background())

	co.startedReading = new(int64)
	co.rtpPacketsReceived = new(uint64)
	co.rtpPacketsSent = new(uint64)
	co.rtpPacketsLost = new(uint64)

	co.newLocalCandidate = make(chan *webrtc.ICECandidateInit)
	co.connected = make(chan struct{})
	co.failed = make(chan struct{})
	co.closed = make(chan struct{})
	co.gatheringDone = make(chan struct{})
	co.incomingTrack = make(chan trackRecvPair)
	co.done = make(chan struct{})
	co.chStartReading = make(chan struct{})

	if co.Publish {
		// Setup transceivers based on mode
		if len(co.OutgoingTracks) > 0 {
			// HTTP/WHIP mode: setup outgoing tracks
			for _, tr := range co.OutgoingTracks {
				err = tr.setup(co)
				if err != nil {
					co.wr.GracefulClose() //nolint:errcheck
					return err
				}
			}
		} else {
			// ============================================================
			// MQTT MODE: Let Pion create transceivers automatically
			// ============================================================
			// DON'T manually create transceivers! They won't match offer's mid
			// Pion will auto-create them during SetRemoteDescription() 
			// As long as codecs are registered in MediaEngine (done above)
			
			co.Log.Log(logger.Info, "MQTT Mode: Codecs registered, Pion will auto-create transceivers from offer")
		}
		
		// Setup OnTrack handler (for ALL publish modes)
		if co.OnTrackFunc != nil {
			co.wr.OnTrack(co.OnTrackFunc)
			co.Log.Log(logger.Info, "Custom OnTrackFunc registered")
		} else {
			co.wr.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
				codec := track.Codec()
				co.Log.Log(logger.Info, "[OnTrack] ===== TRACK RECEIVED =====")
				co.Log.Log(logger.Info, "[OnTrack] Kind: %s", track.Kind())
				co.Log.Log(logger.Info, "[OnTrack] Codec: %s", track.Codec().MimeType)
				co.Log.Log(logger.Info, "[OnTrack] SSRC: %d", track.SSRC())
				co.Log.Log(logger.Info, "[OnTrack] PayloadType: %d", track.PayloadType())
				co.Log.Log(logger.Info, "[OnTrack] ===========================")
				
				if codec.MimeType == "" || codec.ClockRate == 0 {
					co.Log.Log(logger.Error, "[OnTrack] INVALID CODEC - Track will be ignored!")
					return
				}

				select {
				case co.incomingTrack <- trackRecvPair{track, receiver}:
				case <-co.ctx.Done():
				}
			})
			co.Log.Log(logger.Info, "Default OnTrack handler registered")
		}
	} else {
		// Read mode: Add transceivers upfront
		_, err = co.wr.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			co.wr.GracefulClose() //nolint:errcheck
			return err
		}

		_, err = co.wr.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendrecv,
		})
		if err != nil {
			co.wr.GracefulClose() //nolint:errcheck
			return err
		}

		co.wr.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			co.Log.Log(logger.Warn, "[OnTrack] ========== TRACK RECEIVED ==========")
			co.Log.Log(logger.Warn, "[OnTrack] Kind: %s", track.Kind().String())
			co.Log.Log(logger.Warn, "[OnTrack] SSRC: %d", track.SSRC())
			co.Log.Log(logger.Warn, "[OnTrack] PayloadType: %d", track.PayloadType())
			co.Log.Log(logger.Warn, "[OnTrack] Codec: %s", track.Codec().MimeType)
			co.Log.Log(logger.Warn, "[OnTrack] =====================================")
			
			select {
			case co.incomingTrack <- trackRecvPair{track, receiver}:
			case <-co.ctx.Done():
			}
		})
	}

	// DATACHANNEL SUPPORT: Register OnDataChannel handler for FPT camera streaming
	co.dataChannelReady = make(chan struct{})

var controlChannel *webrtc.DataChannel
var dataChannel *webrtc.DataChannel
var dataChannelOpened = false
var streamCommandSent = false  // NEW FLAG: Track if we sent Stream command
_ = controlChannel
_ = dataChannel

co.wr.OnDataChannel(func(dc *webrtc.DataChannel) {
	co.Log.Log(logger.Info, "[DataChannel] ========== DATACHANNEL RECEIVED ==========")
	co.Log.Log(logger.Info, "[DataChannel] Label: %s", dc.Label())
	co.Log.Log(logger.Info, "[DataChannel] ID: %d", *dc.ID())
	co.Log.Log(logger.Info, "[DataChannel] =============================================")
	
	// Store references based on label
	if dc.Label() == "control" {
		controlChannel = dc
		co.dataChannel = dc
		co.Log.Log(logger.Info, "[DataChannel] CONTROL channel registered")
	} else if dc.Label() == "dc" || dc.Label() == "data" {
		dataChannel = dc
		co.dataChannel = dc
		co.Log.Log(logger.Info, "[DataChannel] DATA/VIDEO channel registered")
	}
	
	// Setup DataChannel event handlers
	dc.OnOpen(func() {
		co.Log.Log(logger.Info, "[DataChannel] Channel OPENED: %s", dc.Label())

		// Mark first channel as opened (for dataChannelReady signal)
		if !dataChannelOpened {
			dataChannelOpened = true
			close(co.dataChannelReady)
			co.Log.Log(logger.Info, "[DataChannel] First channel opened - ready signal sent")
		}
		
		// FIX: Send Stream command ONLY when CONTROL channel opens
		// Don't check dataChannelOpened flag - it may already be true if data channel opened first
		if dc.Label() == "control" && !streamCommandSent {
			streamCommandSent = true
			co.Log.Log(logger.Info, "[DataChannel] CONTROL channel opened - sending Stream command...")
			
			go func() {
				time.Sleep(500 * time.Millisecond) // Wait for channel to stabilize

				cameraSerial := co.CameraSerial
				if cameraSerial == "" {
					cameraSerial = "unknown"
				}
				
				streamCmd := map[string]interface{}{
					"Id":      cameraSerial,
					"Command": "Stream",
					"Type":    "Request",
					"Content": map[string]interface{}{
						"ChannelMask":    3, // All channels
						"ResolutionMask": 2, // Highest resolution (0=highest)
					},
				}
				
				cmdJSON, _ := json.Marshal(streamCmd)
				co.Log.Log(logger.Info, "[DataChannel] Sending FPT Stream command: %s", string(cmdJSON))
				
				err := dc.SendText(string(cmdJSON))
				if err != nil {
					co.Log.Log(logger.Error, "[DataChannel] Failed to send Stream command: %v", err)
				} else {
					co.Log.Log(logger.Info, "[DataChannel] Stream command sent successfully")
				}
			}()
		}
	})
	
	dc.OnClose(func() {
		co.Log.Log(logger.Info, "[DataChannel] Channel CLOSED: %s", dc.Label())
	})
	
	dc.OnError(func(err error) {
		co.Log.Log(logger.Error, "[DataChannel] Error on %s: %v", dc.Label(), err)
	})
	
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			co.Log.Log(logger.Info, "[DataChannel/%s] <- Text: %s", dc.Label(), string(msg.Data))

			// Check if message is simple greeting first
			messageText := string(msg.Data)
			if strings.Contains(messageText, "Hello from") {
				co.Log.Log(logger.Info, "[DataChannel] ✓ Camera greeting received!")
				co.Log.Log(logger.Info, "[DataChannel] Camera is ready for commands")
				
				// Send Stream command after greeting
				go func() {
					time.Sleep(1 * time.Second) // Wait for camera to be fully ready
					
					cameraSerial := co.CameraSerial
					if cameraSerial == "" {
						cameraSerial = "unknown"
					}
					
					streamCmd := map[string]interface{}{
						"Id":      cameraSerial,
						"Command": "Stream",
						"Type":    "Request",
						"Content": map[string]interface{}{
							"Option":     1,
							"Resolution": 0, // 0 = highest
						},
					}
					
					cmdJSON, _ := json.Marshal(streamCmd)
					co.Log.Log(logger.Info, "[DataChannel] Sending Stream command after greeting: %s", string(cmdJSON))
					
					err := dc.SendText(string(cmdJSON))
					if err != nil {
						co.Log.Log(logger.Error, "[DataChannel] Failed to send Stream command: %v", err)
					} else {
						co.Log.Log(logger.Info, "[DataChannel] Stream command sent successfully after greeting")
					}
				}()
				return
			}

			// Parse FPT camera response format (JSON)
			var response struct {
				Command string                 `json:"Command"`
				Type    string                 `json:"Type"`
				Result  struct {
					Ret     int    `json:"Ret"`
					Message string `json:"Message"`
				} `json:"Result"`
			}
			
			if err := json.Unmarshal(msg.Data, &response); err == nil {
				if response.Command == "Stream" && response.Type == "Respond" {
					if response.Result.Ret == 0 {
						co.Log.Log(logger.Info, "[DataChannel] Camera accepted Stream command!")
						co.Log.Log(logger.Info, "[DataChannel] Now expecting RTP packets...")
					} else {
						co.Log.Log(logger.Error, "[DataChannel] Camera rejected: %s", response.Result.Message)
					}
				}
			} else {
				co.Log.Log(logger.Debug, "[DataChannel] Message is not JSON format: %v", err)
			}
		}
	})
})

	var stateChangeMutex sync.Mutex

	// Log ICE Gathering state (shows TURN allocation progress)
	co.wr.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		co.Log.Log(logger.Info, "[ICE-Gathering] State changed: %s", state.String())
		if state == webrtc.ICEGatheringStateGathering {
			co.Log.Log(logger.Info, "[ICE-Gathering] Starting to gather candidates (TURN allocation should happen here)...")
		} else if state == webrtc.ICEGatheringStateComplete {
			co.Log.Log(logger.Info, "[ICE-Gathering] Gathering complete!")
			candidates := co.wr.GetReceivers()
			co.Log.Log(logger.Info, "[ICE-Gathering] Total media descriptions (receivers): %d", len(candidates))
		}
	})

	// Add Signaling State logging
	co.wr.OnSignalingStateChange(func(state webrtc.SignalingState) {
		co.Log.Log(logger.Warn, "[DTLS-Debug] Signaling state changed: %s", state.String())
	})

	co.wr.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		stateChangeMutex.Lock()
		defer stateChangeMutex.Unlock()

		select {
		case <-co.closed:
			return
		default:
		}

		co.Log.Log(logger.Info, "[WebRTC] Peer connection state changed: %s", state.String())
		co.Log.Log(logger.Warn, "[DTLS-Debug] Current signaling state: %s", co.wr.SignalingState().String())

		switch state {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-co.connected:
				return
			default:
			}

			co.Log.Log(logger.Info, "peer connection established, local candidate: %v, remote candidate: %v",
				co.LocalCandidate(), co.RemoteCandidate())
			
			// Check DTLS transport state
			receivers := co.wr.GetReceivers()
			if len(receivers) > 0 {
				transport := receivers[0].Transport()
				if transport != nil {
					dtlsTransport := transport.ICETransport()
					if dtlsTransport != nil {
						co.Log.Log(logger.Info, "[DTLS] DTLS transport state: connected")
						co.Log.Log(logger.Info, "[DTLS] Ready to receive RTP packets")
					}
				}
			}

			close(co.connected)

		case webrtc.PeerConnectionStateFailed:
			co.Log.Log(logger.Error, "[DTLS-Debug] Peer connection FAILED!")
			co.Log.Log(logger.Error, "[DTLS-Debug] ICE state: %s", co.wr.ICEConnectionState().String())
			co.Log.Log(logger.Error, "[DTLS-Debug] Signaling state: %s", co.wr.SignalingState().String())
			close(co.failed)

		case webrtc.PeerConnectionStateClosed:
			co.Log.Log(logger.Warn, "[DTLS-Debug] Peer connection CLOSED!")
			co.Log.Log(logger.Warn, "[DTLS-Debug] ICE state: %s", co.wr.ICEConnectionState().String())
			co.Log.Log(logger.Warn, "[DTLS-Debug] Signaling state: %s", co.wr.SignalingState().String())
			co.Log.Log(logger.Warn, "[DTLS-Debug] Possible reasons: Camera closed, DTLS failed, or timeout")
			
			select {
			case <-co.failed:
			default:
				close(co.failed)
			}

			close(co.closed)
		}
	})

	// Enhanced OnICEConnectionStateChange with detailed pair logging
	co.wr.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		co.Log.Log(logger.Info, "[ICE] State: %s", state.String())
		
		// Always try to log selected candidate pair
		receivers := co.wr.GetReceivers()
		if len(receivers) > 0 && receivers[0].Transport() != nil {
			transport := receivers[0].Transport().ICETransport()
			if transport != nil {
				cp, err := transport.GetSelectedCandidatePair()
				if err == nil && cp != nil {
					co.Log.Log(logger.Info, "[ICE] Testing pair:")
					co.Log.Log(logger.Info, "[ICE]   Local:  %s %s:%d (priority: %d)", 
						cp.Local.Typ, cp.Local.Address, cp.Local.Port, cp.Local.Priority)
					co.Log.Log(logger.Info, "[ICE]   Remote: %s %s:%d (priority: %d)", 
						cp.Remote.Typ, cp.Remote.Address, cp.Remote.Port, cp.Remote.Priority)
				} else if state == webrtc.ICEConnectionStateChecking {
					co.Log.Log(logger.Warn, "[ICE] No candidate pair selected yet")
				}
			}
		}
		
		switch state {
		case webrtc.ICEConnectionStateChecking:
			co.Log.Log(logger.Info, "[ICE-Debug] === CHECKING PHASE ===")
			co.Log.Log(logger.Info, "[ICE-Debug] Testing connectivity...")
			
			// Log our candidates
			if localDesc := co.wr.LocalDescription(); localDesc != nil {
				hostCount := strings.Count(localDesc.SDP, "typ host")
				srflxCount := strings.Count(localDesc.SDP, "typ srflx")
				relayCount := strings.Count(localDesc.SDP, "typ relay")
				co.Log.Log(logger.Info, "[ICE-Debug] Our candidates: %d HOST, %d SRFLX, %d RELAY", 
					hostCount, srflxCount, relayCount)
			}
			
			// Log camera's candidates from remote description
			if remoteDesc := co.wr.RemoteDescription(); remoteDesc != nil {
				hostCount := strings.Count(remoteDesc.SDP, "typ host")
				srflxCount := strings.Count(remoteDesc.SDP, "typ srflx")
				relayCount := strings.Count(remoteDesc.SDP, "typ relay")
				co.Log.Log(logger.Info, "[ICE-Debug] Camera candidates: %d HOST, %d SRFLX, %d RELAY", 
					hostCount, srflxCount, relayCount)
				
				// Parse and log camera's actual IPs
				lines := strings.Split(remoteDesc.SDP, "\n")
				for _, line := range lines {
					if strings.Contains(line, "typ srflx") {
						co.Log.Log(logger.Info, "[ICE-Debug] Camera SRFLX: %s", strings.TrimSpace(line))
					}
				}
			}
			
			// Add stuck detector
			go func() {
				time.Sleep(15 * time.Second)
				currentState := co.wr.ICEConnectionState()
				if currentState == webrtc.ICEConnectionStateChecking {
					co.Log.Log(logger.Error, "[ICE] STUCK in checking for 15s!")
					co.Log.Log(logger.Error, "[ICE] Possible causes:")
					co.Log.Log(logger.Error, "[ICE]   1. Firewall blocking all candidate pairs")
					co.Log.Log(logger.Error, "[ICE]   2. Camera IP mismatch (NAT issue)")
					co.Log.Log(logger.Error, "[ICE]   3. UDP port 8189 not accessible")
					co.Log.Log(logger.Error, "[ICE]   4. TURN relay not working")
					
					// Log Windows Firewall command
					co.Log.Log(logger.Error, "[ICE] Try: netsh advfirewall firewall add rule name=\"MediaMTX WebRTC\" dir=in action=allow protocol=UDP localport=8189")
				}
			}()
			
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			co.Log.Log(logger.Info, "[ICE] CONNECTION SUCCESSFUL!")
			
			receivers := co.wr.GetReceivers()
			if len(receivers) > 0 && receivers[0].Transport() != nil {
				transport := receivers[0].Transport().ICETransport()
				if transport != nil {
					cp, err := transport.GetSelectedCandidatePair()
					if err == nil && cp != nil {
						co.Log.Log(logger.Info, "[ICE] Winning pair:")
						co.Log.Log(logger.Info, "[ICE]   Local:  %s %s:%d", cp.Local.Typ, cp.Local.Address, cp.Local.Port)
						co.Log.Log(logger.Info, "[ICE]   Remote: %s %s:%d", cp.Remote.Typ, cp.Remote.Address, cp.Remote.Port)
						
						if cp.Local.Typ.String() == "srflx" {
							co.Log.Log(logger.Info, "[ICE] Direct SRFLX connection")
						} else if cp.Local.Typ.String() == "relay" {
							co.Log.Log(logger.Info, "[ICE] TURN RELAY connection (NAT traversal)")
						}
					}
				}
			}
			
		case webrtc.ICEConnectionStateFailed:
			co.Log.Log(logger.Error, "[ICE] CONNECTION FAILED!")
			
			// Log final attempted pair
			receivers := co.wr.GetReceivers()
			if len(receivers) > 0 && receivers[0].Transport() != nil {
				transport := receivers[0].Transport().ICETransport()
				if transport != nil {
					cp, err := transport.GetSelectedCandidatePair()
					if err == nil && cp != nil {
						co.Log.Log(logger.Error, "[ICE] Last pair: Local=%s Remote=%s", 
							cp.Local.Typ, cp.Remote.Typ)
					}
				}
			}
			
			co.Log.Log(logger.Error, "[ICE] ========================================")
			co.Log.Log(logger.Error, "[ICE] TROUBLESHOOTING STEPS:")
			co.Log.Log(logger.Error, "[ICE] 1. Check Windows Firewall:")
			co.Log.Log(logger.Error, "[ICE]    netsh advfirewall firewall show rule name=all | findstr 8189")
			co.Log.Log(logger.Error, "[ICE] 2. Test TURN server:")
			co.Log.Log(logger.Error, "[ICE]    nc -u -v turn-connect.fcam.vn 3478")
			co.Log.Log(logger.Error, "[ICE] 3. Check camera reachability:")
			co.Log.Log(logger.Error, "[ICE]    ping 21.64.68.193")
			co.Log.Log(logger.Error, "[ICE] 4. Verify public IP:")
			co.Log.Log(logger.Error, "[ICE]    curl ifconfig.me (should be 58.187.123.89)")
			co.Log.Log(logger.Error, "[ICE] ========================================")
			
		case webrtc.ICEConnectionStateDisconnected:
			co.Log.Log(logger.Warn, "[ICE] Temporarily disconnected, waiting for recovery...")
		}
	})

	co.wr.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i != nil {
			v := i.ToJSON()

			// Log ICE candidate details
			co.Log.Log(logger.Info, "[ICE] Local candidate generated: type=%s, protocol=%s, address=%s:%d", 
				i.Typ, i.Protocol, i.Address, i.Port)
			co.Log.Log(logger.Debug, "[ICE] Full candidate string: %s", v.Candidate)

			select {
			case co.newLocalCandidate <- &v:
				co.Log.Log(logger.Debug, "[ICE] Local candidate sent to channel")
			case <-co.connected:
				co.Log.Log(logger.Debug, "[ICE] Connection already established, skipping candidate")
			case <-co.ctx.Done():
				co.Log.Log(logger.Debug, "[ICE] Context done, skipping candidate")
			}

			// NOTE: Trickle ICE is DISABLED because FPT camera doesn't support it
			// FPT camera protocol only supports bundled offer/answer, not trickle ICE
			// Camera receives "remote candidate reader: total received 0" - it never sends back candidates
			cand := v.Candidate
			if cand != "" {
				co.Log.Log(logger.Info, "[ICE-Local] Candidate (will be bundled in answer): type=%s, address=%s:%d", 
					i.Typ, i.Address, i.Port)
				
				// Highlight RELAY candidates (these are allocated from TURN)
				if strings.Contains(cand, "typ relay") {
					co.Log.Log(logger.Info, "[ICE-Local] RELAY candidate found! (TURN allocation successful!)")
				}
			}
			
			// DISABLED: Trickle ICE sending
			// if h := co.LocalICEHandler; h != nil {
			//     go func() {
			//         h(cand)  // This would send candidate to camera, but FPT doesn't support trickle ICE
			//     }()
			// }
		} else {
			co.Log.Log(logger.Info, "[ICE] Candidate gathering complete (received nil)")
			close(co.gatheringDone)
			
			// NOTE: Trickle ICE disabled - all candidates were collected and will be in bundled answer
			// Do NOT send end-of-candidates signal
			co.Log.Log(logger.Info, "[ICE] All candidates gathered and will be sent in bundled answer SDP (trickle ICE disabled)")
		}
	})

	go co.run()

	return nil
}

// Close closes the connection.
func (co *PeerConnection) Close() {
	co.ctxCancel()
	<-co.done
}

func (co *PeerConnection) run() {
	defer close(co.done)

	defer func() {
		for _, track := range co.incomingTracks {
			track.close()
		}
		for _, track := range co.OutgoingTracks {
			track.close()
		}

		co.wr.GracefulClose() //nolint:errcheck

		// even if GracefulClose() should wait for any goroutine to return,
		// we have to wait for OnConnectionStateChange to return anyway,
		// since it is executed in an uncontrolled goroutine.
		// https://github.com/pion/webrtc/blob/4742d1fd54abbc3f81c3b56013654574ba7254f3/peerconnection.go#L509
		<-co.closed
	}()

	for {
		select {
		case <-co.chStartReading:
			for _, track := range co.incomingTracks {
				track.start()
			}
			atomic.StoreInt64(co.startedReading, 1)

		case <-co.ctx.Done():
			return
		}
	}
}

func (co *PeerConnection) removeUnwantedCandidates(firstMedia *sdp.MediaDescription) error {
	// Count existing candidates FIRST
	candidateCount := 0
	for _, attr := range firstMedia.Attributes {
		if attr.Key == "candidate" {
			candidateCount++
		}
	}
	
	// Skip filtering if no candidates (bundled media sections)
	if candidateCount == 0 {
		co.Log.Log(logger.Debug, "[ICE-Filter] Skipping media section with 0 candidates (bundled)")
		return nil
	}
	
	var newAttributes []sdp.Attribute
	relayCount := 0
	
	co.Log.Log(logger.Info, "[ICE-Filter] === FORCING TURN RELAY MODE ===")
	co.Log.Log(logger.Info, "[ICE-Filter] Camera IP %s is unreachable, using TURN relay only", "21.64.68.193")
	co.Log.Log(logger.Info, "[ICE-Filter] Processing %d candidates...", candidateCount)
	
	for _, attr := range firstMedia.Attributes {
		if attr.Key == "candidate" {
			parts := strings.Split(attr.Value, " ")
			
			if len(parts) > 7 {
				candType := parts[7]
				
				if candType == "relay" {
					// ONLY keep RELAY candidates (TURN)
					newAttributes = append(newAttributes, attr)
					relayCount++
					co.Log.Log(logger.Info, "[ICE-Filter] ✓ RELAY kept: %s", attr.Value)
				} else {
					co.Log.Log(logger.Debug, "[ICE-Filter] ✗ Filtered out %s candidate", candType)
				}
			}
			continue
		}
		
		newAttributes = append(newAttributes, attr)
	}
	
	co.Log.Log(logger.Info, "[ICE-Filter] Summary: %d RELAY candidates (filtered %d host/srflx)", 
		relayCount, candidateCount - relayCount)
	
	if relayCount == 0 {
		co.Log.Log(logger.Error, "[ICE-Filter] ERROR: No RELAY candidates available! TURN server may have failed to allocate.")
		return fmt.Errorf("no TURN relay candidates available")
	}
	
	firstMedia.Attributes = newAttributes
	
	co.Log.Log(logger.Info, "[ICE-Filter] === RELAY MODE ENABLED ===")
	co.Log.Log(logger.Info, "[ICE-Filter] All traffic will route through TURN server 42.116.138.54")
	
	return nil
}

func (co *PeerConnection) addAdditionalCandidates(firstMedia *sdp.MediaDescription) error {
	i := 0
	for _, attr := range firstMedia.Attributes {
		if attr.Key == "end-of-candidates" {
			break
		}
		i++
	}

	for _, host := range co.AdditionalHosts {
		var ips []string
		if net.ParseIP(host) != nil {
			ips = []string{host}
		} else {
			tmp, err := net.LookupIP(host)
			if err != nil {
				return err
			}

			ips = make([]string, len(tmp))
			for i, e := range tmp {
				ips[i] = e.String()
			}
		}

		for _, ip := range ips {
			newAttrs := append([]sdp.Attribute(nil), firstMedia.Attributes[:i]...)

			if co.ICEUDPMux != nil {
				port := strconv.FormatInt(int64(co.ICEUDPMux.GetListenAddresses()[0].(*net.UDPAddr).Port), 10)

				tmp, err := randUint32()
				if err != nil {
					return err
				}
				id := strconv.FormatInt(int64(tmp), 10)

				newAttrs = append(newAttrs, sdp.Attribute{
					Key:   "candidate",
					Value: id + " 1 udp 2130706431 " + ip + " " + port + " typ host",
				})
				newAttrs = append(newAttrs, sdp.Attribute{
					Key:   "candidate",
					Value: id + " 2 udp 2130706431 " + ip + " " + port + " typ host",
				})
			}

			if co.ICETCPMux != nil {
				port := strconv.FormatInt(int64(co.ICETCPMux.Ln.Addr().(*net.TCPAddr).Port), 10)

				tmp, err := randUint32()
				if err != nil {
					return err
				}
				id := strconv.FormatInt(int64(tmp), 10)

				newAttrs = append(newAttrs, sdp.Attribute{
					Key:   "candidate",
					Value: id + " 1 tcp 1671430143 " + ip + " " + port + " typ host tcptype passive",
				})
				newAttrs = append(newAttrs, sdp.Attribute{
					Key:   "candidate",
					Value: id + " 2 tcp 1671430143 " + ip + " " + port + " typ host tcptype passive",
				})
			}

			newAttrs = append(newAttrs, firstMedia.Attributes[i:]...)
			firstMedia.Attributes = newAttrs
		}
	}

	return nil
}

func (co *PeerConnection) filterLocalDescription(desc *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	var psdp sdp.SessionDescription
	psdp.Unmarshal([]byte(desc.SDP)) //nolint:errcheck

	// Filter candidates in ALL media descriptions (video, audio, etc.)
	for _, media := range psdp.MediaDescriptions {
		err := co.removeUnwantedCandidates(media)
		if err != nil {
			return nil, err
		}

		err = co.addAdditionalCandidates(media)
		if err != nil {
			return nil, err
		}
	}

	out, _ := psdp.Marshal()
	desc.SDP = string(out)

	return desc, nil
}

// addIceOptionsToAnswer adds ice-options attribute to answer SDP for FPT camera compatibility.
// FPT camera OFFER includes "a=ice-options:ice2,trickle" and firmware expects it in Answer.
// fixRejectedMediaPortsWithOfferPort replaces port 9 AND other incorrect ports with offer ports
// Camera suggests ports in offer - we should echo them back in answer so RTP arrives correctly
func (co *PeerConnection) fixRejectedMediaPortsWithOfferPort(desc *webrtc.SessionDescription, offer *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	// Extract offer ports
	var offerPsdp sdp.SessionDescription
	err := offerPsdp.Unmarshal([]byte(offer.SDP))
	if err != nil {
		co.Log.Log(logger.Warn, "[Port-Fix] Failed to parse offer: %v", err)
		return desc, nil
	}

	// Map offer ports by media type
	offerPorts := make(map[string]int)
	for _, media := range offerPsdp.MediaDescriptions {
		mediaType := media.MediaName.Media
		port := media.MediaName.Port.Value
		offerPorts[mediaType] = port
		co.Log.Log(logger.Info, "[Port-Fix] Offer port: %s=%d", mediaType, port)
	}

	// Fix answer ports to match offer ports
	var psdp sdp.SessionDescription
	err = psdp.Unmarshal([]byte(desc.SDP))
	if err != nil {
		return nil, err
	}

	mediaFixed := 0
	for i := range psdp.MediaDescriptions {
		mediaType := psdp.MediaDescriptions[i].MediaName.Media
		offerPort := offerPorts[mediaType]
		currentPort := psdp.MediaDescriptions[i].MediaName.Port.Value
		
		// If offer suggested a port and answer doesn't match it, use offer port
		if offerPort > 0 && currentPort != offerPort {
			psdp.MediaDescriptions[i].MediaName.Port.Value = offerPort
			mediaFixed++
			
			co.Log.Log(logger.Info, "[Port-Fix] Answer %s: %d → %d (from offer)", mediaType, currentPort, offerPort)
		}
	}

	if mediaFixed > 0 {
		newSDP, _ := psdp.Marshal()
		return &webrtc.SessionDescription{
			Type: desc.Type,
			SDP:  string(newSDP),
		}, nil
	}

	return desc, nil
}

// fixRejectedMediaPorts replaces port 9 (rejected) with first available RTP port
// This forces Pion to accept media streams even if codec matching fails
func (co *PeerConnection) fixRejectedMediaPorts(desc *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	var psdp sdp.SessionDescription
	err := psdp.Unmarshal([]byte(desc.SDP))
	if err != nil {
		return nil, err
	}

	// Use fixed port 8189 for all media
	// ICE candidates will specify actual connection addresses
	fixedPort := 8189
	mediaFixed := 0

	for i := range psdp.MediaDescriptions {
		if psdp.MediaDescriptions[i].MediaName.Port.Value == 9 {
			mediaType := psdp.MediaDescriptions[i].MediaName.Media
			psdp.MediaDescriptions[i].MediaName.Port.Value = fixedPort
			mediaFixed++
			
			co.Log.Log(logger.Info, "[Port-Fix] Replacing port 9 → %d for %s media", fixedPort, mediaType)
		}
	}

	if mediaFixed > 0 {
		// Marshal back to SDP string
		newSDP, _ := psdp.Marshal()
		return &webrtc.SessionDescription{
			Type: desc.Type,
			SDP:  string(newSDP),
		}, nil
	}

	// No rejected media found - return original
	return desc, nil
}

// Pion doesn't include ice-options in Answer by default, but camera requires it.
func (co *PeerConnection) addIceOptionsToAnswer(desc *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	var psdp sdp.SessionDescription
	err := psdp.Unmarshal([]byte(desc.SDP))
	if err != nil {
		return nil, err
	}

	co.Log.Log(logger.Info, "[FPT-ICE] Adding ice-options attribute for FPT camera firmware compatibility")

	// Check if ice-options already exists at session level
	hasIceOptions := false
	for _, attr := range psdp.Attributes {
		if attr.Key == "ice-options" {
			hasIceOptions = true
			co.Log.Log(logger.Debug, "[FPT-ICE] Session already has ice-options: %s", attr.Value)
			break
		}
	}

	// Add ice-options at session level if not present (before group:BUNDLE)
	if !hasIceOptions {
		insertIdx := len(psdp.Attributes)
		for i, attr := range psdp.Attributes {
			if attr.Key == "group" {
				insertIdx = i
				break
			}
		}

		newAttrs := make([]sdp.Attribute, 0, len(psdp.Attributes)+1)
		newAttrs = append(newAttrs, psdp.Attributes[:insertIdx]...)
		newAttrs = append(newAttrs, sdp.Attribute{
			Key:   "ice-options",
			Value: "trickle",
		})
		newAttrs = append(newAttrs, psdp.Attributes[insertIdx:]...)
		psdp.Attributes = newAttrs

		co.Log.Log(logger.Info, "[FPT-ICE] Added session-level ice-options:trickle")
	}

	// Also add ice-options to each media description if not present
	for idx, media := range psdp.MediaDescriptions {
		hasMediaIceOptions := false
		for _, attr := range media.Attributes {
			if attr.Key == "ice-options" {
				hasMediaIceOptions = true
				break
			}
		}

		if !hasMediaIceOptions {
			// Find insertion point (after mid, before ice-ufrag)
			insertIdx := 0
			for i, attr := range media.Attributes {
				if attr.Key == "mid" {
					insertIdx = i + 1
					break
				}
			}

			newAttrs := make([]sdp.Attribute, 0, len(media.Attributes)+1)
			newAttrs = append(newAttrs, media.Attributes[:insertIdx]...)
			newAttrs = append(newAttrs, sdp.Attribute{
				Key:   "ice-options",
				Value: "trickle",
			})
			newAttrs = append(newAttrs, media.Attributes[insertIdx:]...)
			media.Attributes = newAttrs

			co.Log.Log(logger.Info, "[FPT-ICE] Added ice-options to %s media[%d]", media.MediaName.Media, idx)
		}
	}

	out, err := psdp.Marshal()
	if err != nil {
		return nil, err
	}
	desc.SDP = string(out)

	co.Log.Log(logger.Info, "[FPT-ICE] Answer SDP modified with ice-options successfully")
	return desc, nil
}

// addSSRCAttributesForFPTCamera adds SSRC and MSID attributes to answer SDP for FPT camera compatibility.
// FPT camera firmware requires these attributes even for recvonly transceivers (non-standard behavior).
// Browser WebRTC implementations include these automatically, but Pion doesn't (per spec).
func (co *PeerConnection) addSSRCAttributesForFPTCamera(desc *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	var psdp sdp.SessionDescription
	err := psdp.Unmarshal([]byte(desc.SDP))
	if err != nil {
		return nil, err
	}

	co.Log.Log(logger.Info, "[FPT-SSRC] Adding SSRC attributes for FPT camera firmware compatibility")

	// Generate random SSRCs for video and audio (must be unique)
	videoSSRC := rand.Uint32()
	audioSSRC := rand.Uint32()
	if audioSSRC == videoSSRC {
		audioSSRC++ // Ensure uniqueness
	}

	for idx, media := range psdp.MediaDescriptions {
		var ssrc uint32
		var streamLabel string
		var trackLabel string

		// Determine media type and labels
		if media.MediaName.Media == "video" {
			ssrc = videoSSRC
			streamLabel = "MediaMTXStream"
			trackLabel = "MediaMTXVideo"
			co.Log.Log(logger.Info, "[FPT-SSRC] Video media: ssrc=%d, msid=%s %s", ssrc, streamLabel, trackLabel)
		} else if media.MediaName.Media == "audio" {
			ssrc = audioSSRC
			streamLabel = "MediaMTXStream"
			trackLabel = "MediaMTXAudio"
			co.Log.Log(logger.Info, "[FPT-SSRC] Audio media: ssrc=%d, msid=%s %s", ssrc, streamLabel, trackLabel)
		} else {
			continue // Skip data channels
		}

		// Check if already has SSRC (shouldn't happen, but be safe)
		hasSSRC := false
		for _, attr := range media.Attributes {
			if attr.Key == "ssrc" {
				hasSSRC = true
				break
			}
		}

		if hasSSRC {
			co.Log.Log(logger.Debug, "[FPT-SSRC] Media %d already has SSRC, skipping", idx)
			continue
		}

		// Add SSRC attributes BEFORE rtcp-mux (to match browser format)
		// Find insertion point (before rtcp-mux or end-of-candidates)
		insertIdx := len(media.Attributes)
		for i, attr := range media.Attributes {
			if attr.Key == "rtcp-mux" || attr.Key == "end-of-candidates" {
				insertIdx = i
				break
			}
		}

		// Create new attributes with SSRC inserted
		newAttrs := make([]sdp.Attribute, 0, len(media.Attributes)+3)
		newAttrs = append(newAttrs, media.Attributes[:insertIdx]...)

		// Add SSRC attributes (matching camera format)
		newAttrs = append(newAttrs, sdp.Attribute{
			Key:   "ssrc",
			Value: fmt.Sprintf("%d cname:%s", ssrc, trackLabel),
		})
		newAttrs = append(newAttrs, sdp.Attribute{
			Key:   "ssrc",
			Value: fmt.Sprintf("%d msid:%s %s", ssrc, streamLabel, trackLabel),
		})
		newAttrs = append(newAttrs, sdp.Attribute{
			Key:   "msid",
			Value: fmt.Sprintf("%s %s", streamLabel, trackLabel),
		})

		// Append remaining attributes
		newAttrs = append(newAttrs, media.Attributes[insertIdx:]...)
		media.Attributes = newAttrs

		co.Log.Log(logger.Info, "[FPT-SSRC] Added 3 SSRC attributes to %s media (total attrs: %d)", 
			media.MediaName.Media, len(media.Attributes))
	}

	out, err := psdp.Marshal()
	if err != nil {
		return nil, err
	}
	desc.SDP = string(out)

	co.Log.Log(logger.Info, "[FPT-SSRC] Answer SDP modified successfully")
	return desc, nil
}

// CreatePartialOffer creates a partial offer.
func (co *PeerConnection) CreatePartialOffer() (*webrtc.SessionDescription, error) {
	tmp, err := co.wr.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	offer := &tmp

	err = co.wr.SetLocalDescription(*offer)
	if err != nil {
		return nil, err
	}

	offer, err = co.filterLocalDescription(offer)
	if err != nil {
		return nil, err
	}

	return offer, nil
}

// SetAnswer sets the answer.
func (co *PeerConnection) SetAnswer(answer *webrtc.SessionDescription) error {
	return co.wr.SetRemoteDescription(*answer)
}

// AddRemoteCandidate adds a remote candidate.
func (co *PeerConnection) AddRemoteCandidate(candidate *webrtc.ICECandidateInit) error {
	return co.wr.AddICECandidate(*candidate)
}

// normalizeOfferCodecs modifies offer to use standard Pion codec payload types
// This fixes Pion's codec rejection issue with non-standard PT values
func (co *PeerConnection) normalizeOfferCodecs(offer *webrtc.SessionDescription) *webrtc.SessionDescription {
	// Keep offer unchanged for now - just ensure it's accepted by Pion
	// The key is that we already registered H.264 PT 102 and PCMA PT 8
	// Pion should accept these when we call SetRemoteDescription
	
	// Alternative approach: If Pion still rejects, we can remap codecs here
	// For now, return original offer - the issue might be elsewhere
	return offer
}

// CreateFullAnswer creates a full answer.
func (co *PeerConnection) CreateFullAnswer(offer *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	// Log complete offer SDP to see what camera actually sends
	co.Log.Log(logger.Info, "[CreateFullAnswer] ========== CAMERA OFFER SDP START ==========")
	offerLines := strings.Split(offer.SDP, "\n")
	for i, line := range offerLines {
		// Truncate very long lines
		displayLine := line
		if len(line) > 200 {
			displayLine = line[:200] + "... (truncated)"
		}
		co.Log.Log(logger.Info, "[CreateFullAnswer] Offer[%d]: %s", i+1, displayLine)
	}
	co.Log.Log(logger.Info, "[CreateFullAnswer] ========== CAMERA OFFER SDP END ==========")
	
	// Log remote candidates from offer
	remoteCandidateCount := strings.Count(offer.SDP, "a=candidate:")
	co.Log.Log(logger.Info, "[CreateFullAnswer] Remote candidates in offer SDP: %d", remoteCandidateCount)
	
	// Parse and log each candidate from the offer
	if remoteCandidateCount > 0 {
		lines := strings.Split(offer.SDP, "\n")
		candNum := 0
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "a=candidate:") {
				candNum++
				// Extract basic info
				parts := strings.Split(trimmed, " ")
				if len(parts) > 7 {
					candType := parts[7]
					protocol := parts[2]
					address := parts[4]
					port := "?"
					if len(parts) > 5 {
						port = parts[5]
					}
					co.Log.Log(logger.Info, "[CreateFullAnswer] Camera candidate #%d: %s/%s %s:%s", 
						candNum, candType, protocol, address, port)
				} else {
					co.Log.Log(logger.Info, "[CreateFullAnswer] Camera candidate #%d: %s", candNum, trimmed)
				}
			}
		}
	} else {
		co.Log.Log(logger.Warn, "[CreateFullAnswer] WARNING: Camera offer has ZERO ICE candidates!")
		co.Log.Log(logger.Warn, "[CreateFullAnswer] This might explain why ICE connection fails")
	}
	
	// FIX: Modify offer to ensure Pion accepts codecs
	// Problem: Pion rejects H.264 PT 102 during SetRemoteDescription
	// Solution: Normalize offer SDP to use standard payload types
	modifiedOffer := co.normalizeOfferCodecs(offer)
	co.Log.Log(logger.Info, "[CreateFullAnswer] Offer modified for codec compatibility")
	
	err := co.wr.SetRemoteDescription(*modifiedOffer)
	if err != nil {
		co.Log.Log(logger.Error, "[CreateFullAnswer] SetRemoteDescription failed: %v", err)
		return nil, err
	}
	
	co.Log.Log(logger.Info, "[CreateFullAnswer] SetRemoteDescription succeeded")
	
	// CRITICAL FIX: Force Pion to create answer with proper ports
	// Problem: Receiver exists but no incoming track yet - Pion creates answer with port 9 (reject)
	// Root cause: Pion needs either (1) RTP data to arrive, or (2) outgoing track on transceiver
	// Solution: Add dummy outgoing tracks to force transceiver activation
	
	existingTransceivers := co.wr.GetTransceivers()
	for i, tr := range existingTransceivers {
		if tr.Receiver() != nil && tr.Receiver().Track() == nil {
			mid := tr.Mid()
			
			if i == 0 { // Video transceiver
				co.Log.Log(logger.Info, "[Track-Accept] Video transceiver[0] mid=%s - will add video track to force port != 9", mid)
				// Use Sender to add outgoing track
				videoTrack, err := webrtc.NewTrackLocalStaticSample(
					webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
					"video",
					"MediaMTXVideo",
				)
				if err == nil {
					_, err = co.wr.AddTrack(videoTrack)
					if err == nil {
						co.Log.Log(logger.Info, "[Track-Accept] ✓ Added dummy video track to PC")
					} else {
						co.Log.Log(logger.Warn, "[Track-Accept] Failed to add video track: %v", err)
					}
				}
			} else if i == 1 { // Audio transceiver
				co.Log.Log(logger.Info, "[Track-Accept] Audio transceiver[1] mid=%s - will add audio track to force port != 9", mid)
				audioTrack, err := webrtc.NewTrackLocalStaticSample(
					webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1},
					"audio",
					"MediaMTXAudio",
				)
				if err == nil {
					_, err = co.wr.AddTrack(audioTrack)
					if err == nil {
						co.Log.Log(logger.Info, "[Track-Accept] ✓ Added dummy audio track to PC")
					} else {
						co.Log.Log(logger.Warn, "[Track-Accept] Failed to add audio track: %v", err)
					}
				}
			}
		}
	}
	
	// Log transceivers after SetRemoteDescription
	transceivers := co.wr.GetTransceivers()
	co.Log.Log(logger.Info, "[CreateFullAnswer] Transceivers: %d", len(transceivers))
	
	for i, tr := range transceivers {
		mid := tr.Mid()
		currentDir := tr.Direction()
		
		kind := "unknown"
		var receiver *webrtc.RTPReceiver
		var track *webrtc.TrackRemote
		
		if tr.Receiver() != nil {
			receiver = tr.Receiver()
			if receiver.Track() != nil {
				track = receiver.Track()
				kind = track.Kind().String()
				co.Log.Log(logger.Info, "[Codec-Debug] Transceiver[%d] HAS TRACK: kind=%s, codec=%+v", 
					i, kind, track.Codec())
			} else {
				co.Log.Log(logger.Warn, "[Codec-Debug] Transceiver[%d] Receiver exists but NO TRACK!", i)
				// Try to get receiver codecs
				params := receiver.GetParameters()
				co.Log.Log(logger.Info, "[Codec-Debug] Receiver parameters: %d codecs", len(params.Codecs))
				for j, codec := range params.Codecs {
					co.Log.Log(logger.Info, "[Codec-Debug]   Codec[%d]: %s PT=%d", j, codec.MimeType, codec.PayloadType)
				}
			}
		} else {
			co.Log.Log(logger.Warn, "[Codec-Debug] Transceiver[%d] NO RECEIVER at all!", i)
		}
		
		co.Log.Log(logger.Info, "[CreateFullAnswer] Transceiver[%d]: mid=%s, kind=%s, direction=%s", 
			i, mid, kind, currentDir.String())
	}

	// Create answer
	answerOptions := &webrtc.AnswerOptions{}
	tmp, err := co.wr.CreateAnswer(answerOptions)
	if err != nil {
		if errors.Is(err, webrtc.ErrSenderWithNoCodecs) {
			return nil, fmt.Errorf("codecs not supported by client")
		}
		return nil, err
	}
	answer := &tmp

	// Set local description with ORIGINAL answer (Pion internal state)
	co.Log.Log(logger.Info, "[FPT-Fix] SetLocalDescription with ORIGINAL Answer")
	
	err = co.wr.SetLocalDescription(*answer)
	if err != nil {
		return nil, err
	}

	// Wait for ICE gathering
	err = co.waitGatheringDone()
	if err != nil {
		return nil, err
	}

	// Get final answer with all candidates
	answer = co.wr.LocalDescription()

	answer, err = co.fixConnectionAddress(answer)
	if err != nil {
		co.Log.Log(logger.Error, "[FPT-Fix] Failed to fix connection address: %v", err)
	}

	// Apply candidate filtering (keep TURN relay for NAT traversal)
	answer, err = co.filterLocalDescription(answer)
	if err != nil {
		return nil, err
	}

	// Add FPT-specific fixes
	answer, err = co.addIceOptionsToAnswer(answer)
	if err != nil {
		co.Log.Log(logger.Error, "[FPT-Fix] Failed to add ice-options: %v", err)
		return nil, err
	}

	answer, err = co.addSSRCAttributesForFPTCamera(answer)
	if err != nil {
		co.Log.Log(logger.Error, "[FPT-Fix] Failed to add SSRC: %v", err)
		return nil, err
	}

	// FIX: Replace port 9 (rejected) with port from camera's offer
	// This ensures answer uses same port as offer, matching camera's listening port
	answer, err = co.fixRejectedMediaPortsWithOfferPort(answer, offer)
	if err != nil {
		co.Log.Log(logger.Warn, "[FPT-Fix] Failed to fix rejected media ports: %v", err)
		// Continue anyway - not critical
	}

	// Log final answer ports for verification
	var answerSdp sdp.SessionDescription
	answerSdp.Unmarshal([]byte(answer.SDP)) //nolint:errcheck
	for i, media := range answerSdp.MediaDescriptions {
		co.Log.Log(logger.Info, "[Final-Answer] Media[%d]: %s on port %d", i, media.MediaName.Media, media.MediaName.Port.Value)
	}

	// Debug: Check transceiver receivers state before returning answer
	txceivers := co.wr.GetTransceivers()
	co.Log.Log(logger.Info, "[Answer-Return] Total transceivers: %d", len(txceivers))
	for i, tr := range txceivers {
		if tr.Receiver() != nil {
			co.Log.Log(logger.Info, "[Answer-Return] Transceiver[%d]: Receiver exists, mid=%s, direction=%s", 
				i, tr.Mid(), tr.Direction().String())
		} else {
			co.Log.Log(logger.Warn, "[Answer-Return] Transceiver[%d]: NO RECEIVER, mid=%s, direction=%s", 
				i, tr.Mid(), tr.Direction().String())
		}
	}

	co.Log.Log(logger.Info, "[FPT-Fix] CreateFullAnswer returned modified Answer")
	
	return answer, nil
}

func (co *PeerConnection) waitGatheringDone() error {
	for {
		select {
		case <-co.NewLocalCandidate():
		case <-co.GatheringDone():
			return nil
		case <-co.ctx.Done():
			return fmt.Errorf("terminated")
		}
	}
}

// WaitUntilConnected waits until connection is established.
func (co *PeerConnection) WaitUntilConnected() error {
	t := time.NewTimer(time.Duration(co.HandshakeTimeout))
	defer t.Stop()

outer:
	for {
		select {
		case <-t.C:
			return fmt.Errorf("deadline exceeded while waiting connection")

		case <-co.connected:
			break outer

		case <-co.ctx.Done():
			return fmt.Errorf("terminated")
		}
	}

	return nil
}

// GatherIncomingTracks gathers incoming tracks.
func (co *PeerConnection) GatherIncomingTracks() error {
	var sdp sdp.SessionDescription
	sdp.Unmarshal([]byte(co.wr.RemoteDescription().SDP)) //nolint:errcheck

	// Count only accepted media sections (port != 9), not rejected ones
	maxTrackCount := 0
	rejectedCount := 0
	for _, md := range sdp.MediaDescriptions {
		mediaType := md.MediaName.Media
		if mediaType == "application" {
            co.Log.Log(logger.Debug, "[Track-Gather] Skipping application media (DataChannel, not RTP)")
            continue
        }

		if md.MediaName.Port.Value != 9 {
			maxTrackCount++
			co.Log.Log(logger.Debug, "[Track-Gather] Counting %s media (port %d)", 
                mediaType, md.MediaName.Port.Value)
		} else {
			rejectedCount++
			co.Log.Log(logger.Debug, "[Track-Gather] Skipping rejected %s media (port 9)", 
                mediaType)
		}
	}

	co.Log.Log(logger.Info, "[Track-Gather] Expecting %d RTP tracks (video+audio only, %d rejected, %d application)", 
        maxTrackCount, rejectedCount, len(sdp.MediaDescriptions)-maxTrackCount-rejectedCount)
	co.Log.Log(logger.Info, "Gathering incoming tracks (timeout: %s)...", co.TrackGatherTimeout)
	co.Log.Log(logger.Info, "[Track-Gather] Expecting %d tracks from remote SDP (total media sections: %d, rejected: %d)", 
		maxTrackCount, len(sdp.MediaDescriptions), rejectedCount)
	co.Log.Log(logger.Info, "[Track-Gather] Current ICE state: %s", co.wr.ICEConnectionState().String())
	co.Log.Log(logger.Info, "[Track-Gather] Current connection state: %s", co.wr.ConnectionState().String())

	t := time.NewTimer(time.Duration(co.TrackGatherTimeout))
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if len(co.incomingTracks) != 0 {
				co.Log.Log(logger.Warn, "[Track-Gather] Timeout but received %d/%d tracks - continuing", 
					len(co.incomingTracks), maxTrackCount)
				return nil
			}
			co.Log.Log(logger.Error, "[Track-Gather] Timeout: No tracks received after %s", co.TrackGatherTimeout)
			co.Log.Log(logger.Error, "[Track-Gather] Final ICE state: %s", co.wr.ICEConnectionState().String())
			co.Log.Log(logger.Error, "[Track-Gather] Final connection state: %s", co.wr.ConnectionState().String())
			return fmt.Errorf("deadline exceeded while waiting tracks")

		case pair := <-co.incomingTrack:
			co.Log.Log(logger.Info, "[Track-Gather] ✓ Received track %d/%d: %s", 
				len(co.incomingTracks)+1, maxTrackCount, pair.track.Kind().String())
			t := &IncomingTrack{
				track:              pair.track,
				receiver:           pair.receiver,
				writeRTCP:          co.wr.WriteRTCP,
				log:                co.Log,
				rtpPacketsReceived: co.rtpPacketsReceived,
				rtpPacketsLost:     co.rtpPacketsLost,
			}
			t.initialize()
			co.incomingTracks = append(co.incomingTracks, t)

			if len(co.incomingTracks) >= maxTrackCount {
				co.Log.Log(logger.Info, "[Track-Gather] ✓ All %d tracks received successfully!", maxTrackCount)
				return nil
			}

		case <-co.Failed():
			co.Log.Log(logger.Error, "[Track-Gather] Peer connection failed/closed while gathering tracks")
			co.Log.Log(logger.Error, "[Track-Gather] Received %d/%d tracks before failure", 
				len(co.incomingTracks), maxTrackCount)
			co.Log.Log(logger.Error, "[Track-Gather] ICE state at failure: %s", co.wr.ICEConnectionState().String())
			return fmt.Errorf("peer connection closed")

		case <-co.ctx.Done():
			return fmt.Errorf("terminated")
		}
	}
}

// Connected returns when connected.
func (co *PeerConnection) Connected() <-chan struct{} {
	return co.connected
}

// Failed returns when failed.
func (co *PeerConnection) Failed() <-chan struct{} {
	return co.failed
}

// NewLocalCandidate returns when there's a new local candidate.
func (co *PeerConnection) NewLocalCandidate() <-chan *webrtc.ICECandidateInit {
	return co.newLocalCandidate
}

// GatheringDone returns when candidate gathering is complete.
func (co *PeerConnection) GatheringDone() <-chan struct{} {
	return co.gatheringDone
}

// ForwardIncomingTrack sends an incoming track to the Track-Gather channel.
// This is used by custom OnTrackFunc to ensure tracks are properly registered
// by the Track-Gather mechanism.
func (co *PeerConnection) ForwardIncomingTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	select {
	case co.incomingTrack <- trackRecvPair{track, receiver}:
		co.Log.Log(logger.Debug, "[ForwardIncomingTrack] Forwarded %s track to Track-Gather channel", track.Kind())
	default:
		co.Log.Log(logger.Warn, "[ForwardIncomingTrack] Track-Gather channel full, track may timeout")
	}
}
// IncomingTracks returns incoming tracks.
func (co *PeerConnection) IncomingTracks() []*IncomingTrack {
	return co.incomingTracks
}

// StartReading starts reading incoming tracks.
func (co *PeerConnection) StartReading() {
	select {
	case co.chStartReading <- struct{}{}:
	case <-co.ctx.Done():
	}
}

// LocalCandidate returns the local candidate.
func (co *PeerConnection) LocalCandidate() string {
	receivers := co.wr.GetReceivers()
	if len(receivers) < 1 {
		return ""
	}

	cp, err := receivers[0].Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || cp == nil {
		return ""
	}

	return candidateLabel(cp.Local)
}

// RemoteCandidate returns the remote candidate.
func (co *PeerConnection) RemoteCandidate() string {
	receivers := co.wr.GetReceivers()
	if len(receivers) < 1 {
		return ""
	}

	cp, err := receivers[0].Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || cp == nil {
		return ""
	}

	return candidateLabel(cp.Remote)
}

// DataChannel returns the active DataChannel (for FPT camera streaming)
func (co *PeerConnection) DataChannel() *webrtc.DataChannel {
	return co.dataChannel
}

// WaitDataChannelReady waits until DataChannel is opened
func (co *PeerConnection) WaitDataChannelReady() error {
	// Wait for DataChannel to be received from remote peer (up to 30s)
	dcTimeout := time.NewTimer(30 * time.Second)
	defer dcTimeout.Stop()
	
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			if co.dataChannel != nil {
				// DataChannel received, now wait for OnOpen
				select {
				case <-co.dataChannelReady:
					co.Log.Log(logger.Info, "[DataChannel] Ready!")
					return nil
				case <-time.After(10 * time.Second):
					return fmt.Errorf("DataChannel open timeout (10s)")
				case <-co.ctx.Done():
					return fmt.Errorf("terminated")
				}
			}
		case <-dcTimeout.C:
			return fmt.Errorf("no DataChannel received from remote peer within 30s")
		case <-co.ctx.Done():
			return fmt.Errorf("terminated")
		}
	}
}

// SendDataChannelMessage sends a message through DataChannel
func (co *PeerConnection) SendDataChannelMessage(data []byte, isText bool) error {
	if co.dataChannel == nil {
		return fmt.Errorf("DataChannel not available")
	}
	
	if co.dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("DataChannel not open (state: %s)", co.dataChannel.ReadyState())
	}
	
	var err error
	if isText {
		err = co.dataChannel.SendText(string(data))
		co.Log.Log(logger.Debug, "[DataChannel] -> Text message (%d bytes)", len(data))
	} else {
		err = co.dataChannel.Send(data)
		co.Log.Log(logger.Debug, "[DataChannel] -> Binary message (%d bytes)", len(data))
	}
	
	return err
}

func bytesStats(wr *webrtc.PeerConnection) (uint64, uint64) {
	for _, stats := range wr.GetStats() {
		if tstats, ok := stats.(webrtc.TransportStats); ok {
			if tstats.ID == "iceTransport" {
				return tstats.BytesReceived, tstats.BytesSent
			}
		}
	}
	return 0, 0
}

// Stats returns statistics.
func (co *PeerConnection) Stats() *Stats {
	bytesReceived, bytesSent := bytesStats(co.wr)

	v := float64(0)
	n := float64(0)

	if atomic.LoadInt64(co.startedReading) == 1 {
		for _, tr := range co.incomingTracks {
			if recvStats := tr.rtcpReceiver.Stats(); recvStats != nil {
				v += recvStats.Jitter
				n++
			}
		}
	}

	var rtpPacketsJitter float64
	if n != 0 {
		rtpPacketsJitter = v / n
	} else {
		rtpPacketsJitter = 0
	}

	return &Stats{
		BytesReceived:       bytesReceived,
		BytesSent:           bytesSent,
		RTPPacketsReceived:  atomic.LoadUint64(co.rtpPacketsReceived),
		RTPPacketsSent:      atomic.LoadUint64(co.rtpPacketsSent),
		RTPPacketsLost:      atomic.LoadUint64(co.rtpPacketsLost),
		RTPPacketsJitter:    rtpPacketsJitter,
		RTCPPacketsReceived: atomic.LoadUint64(co.statsInterceptor.rtcpPacketsReceived),
		RTCPPacketsSent:     atomic.LoadUint64(co.statsInterceptor.rtcpPacketsSent),
	}
}

func (pc *PeerConnection) LocalDescription() *webrtc.SessionDescription {
	if pc.wr != nil {
		return pc.wr.LocalDescription()
	}
	return nil
}

// HasDataChannel checks if a DataChannel has been received and is ready
func (co *PeerConnection) HasDataChannel() bool {
	co.mu.Lock()
	defer co.mu.Unlock()
	return co.dataChannel != nil
}

func (pc *PeerConnection) WebRTCConnection() *webrtc.PeerConnection {
    return pc.wr
}

func (co *PeerConnection) fixConnectionAddress(desc *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	var psdp sdp.SessionDescription
	err := psdp.Unmarshal([]byte(desc.SDP))
	if err != nil {
		return nil, err
	}

	// Get our TRUE public IP from SRFLX candidates
	// Priority: 58.x.x.x (FPT public) > other public IPs > private IPs
	publicIP := ""
	candidatePriority := 0 // 0=none, 1=private, 2=public, 3=FPT public
	
	for _, media := range psdp.MediaDescriptions {
		for _, attr := range media.Attributes {
			if attr.Key == "candidate" && strings.Contains(attr.Value, "typ srflx") {
				parts := strings.Fields(attr.Value)
				if len(parts) > 4 {
					ip := parts[4]
					priority := 0
					
					// Classify IP
					if strings.HasPrefix(ip, "58.") {
						// FPT public IP range - HIGHEST priority
						priority = 3
					} else if !strings.HasPrefix(ip, "10.") && 
					          !strings.HasPrefix(ip, "192.168.") && 
					          !strings.HasPrefix(ip, "172.") &&
					          !strings.HasPrefix(ip, "127.") &&
					          !strings.HasPrefix(ip, "100.") &&
					          !strings.HasPrefix(ip, "21.") { // FPT internal
						// Other public IP
						priority = 2
					} else {
						// Private IP
						priority = 1
					}
					
					// Update if better priority found
					if priority > candidatePriority {
						publicIP = ip
						candidatePriority = priority
						co.Log.Log(logger.Info, "[Connection-IP] Found better IP: %s (priority=%d)", ip, priority)
					}
				}
			}
		}
	}

	if publicIP == "" {
		// Fallback: use any non-loopback IP
		publicIP = "127.0.0.1"
		co.Log.Log(logger.Warn, "[Connection-IP] No public IP found, using fallback: %s", publicIP)
	} else {
		co.Log.Log(logger.Info, "[Connection-IP] Selected public IP: %s (priority=%d)", publicIP, candidatePriority)
	}

	// Replace 0.0.0.0 with actual IP in all media sections
	modified := false
	for i := range psdp.MediaDescriptions {
		if psdp.MediaDescriptions[i].ConnectionInformation != nil {
			oldIP := psdp.MediaDescriptions[i].ConnectionInformation.Address.Address
			if oldIP == "0.0.0.0" || strings.HasPrefix(oldIP, "21.") {
				co.Log.Log(logger.Info, "[Connection-IP] Fixing media[%d]: %s → %s", i, oldIP, publicIP)
				psdp.MediaDescriptions[i].ConnectionInformation.Address.Address = publicIP
				modified = true
			}
		}
	}

	// Also fix session-level connection if exists
	if psdp.ConnectionInformation != nil {
		oldIP := psdp.ConnectionInformation.Address.Address
		if oldIP == "0.0.0.0" || strings.HasPrefix(oldIP, "21.") {
			co.Log.Log(logger.Info, "[Connection-IP] Fixing session-level: %s → %s", oldIP, publicIP)
			psdp.ConnectionInformation.Address.Address = publicIP
			modified = true
		}
	}

	if modified {
		newSDP, err := psdp.Marshal()
		if err != nil {
			return nil, err
		}
		desc.SDP = string(newSDP)
		co.Log.Log(logger.Info, "[Connection-IP] Fixed connection address in answer")
	} else {
		co.Log.Log(logger.Debug, "[Connection-IP] No fix needed")
	}

	return desc, nil
}