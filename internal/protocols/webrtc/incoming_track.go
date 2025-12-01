package webrtc

import (
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpreceiver"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/counterdumper"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	keyFrameInterval  = 2 * time.Second
	mimeTypeMultiopus = "audio/multiopus"
	mimeTypeL16       = "audio/L16"
)

var incomingVideoCodecs = []webrtc.RTPCodecParameters{
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeAV1,
			ClockRate:   90000,
			SDPFmtpLine: "profile=1",
		},
		PayloadType: 96,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeAV1,
			ClockRate: 90000,
		},
		PayloadType: 97,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=3",
		},
		PayloadType: 98,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=2",
		},
		PayloadType: 99,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=1",
		},
		PayloadType: 100,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=0",
		},
		PayloadType: 101,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		PayloadType: 102,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH265,
			ClockRate:   90000,
			SDPFmtpLine: "level-id=93;profile-id=2;tier-flag=0;tx-mode=SRST",
		},
		PayloadType: 103,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH265,
			ClockRate:   90000,
			SDPFmtpLine: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST",
		},
		PayloadType: 104,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 105,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 106,
	},
}

var incomingAudioCodecs = []webrtc.RTPCodecParameters{
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    3,
			SDPFmtpLine: "channel_mapping=0,2,1;num_streams=2;coupled_streams=1",
		},
		PayloadType: 112,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    4,
			SDPFmtpLine: "channel_mapping=0,1,2,3;num_streams=2;coupled_streams=2",
		},
		PayloadType: 113,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    5,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3;num_streams=3;coupled_streams=2",
		},
		PayloadType: 114,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    6,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3,5;num_streams=4;coupled_streams=2",
		},
		PayloadType: 115,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    7,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3,5,6;num_streams=4;coupled_streams=4",
		},
		PayloadType: 116,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    8,
			SDPFmtpLine: "channel_mapping=0,6,1,4,5,2,3,7;num_streams=5;coupled_streams=4",
		},
		PayloadType: 117,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
		},
		PayloadType: 111,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeG722,
			ClockRate: 8000,
		},
		PayloadType: 9,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 118,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 119,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
		},
		PayloadType: 0,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
		},
		PayloadType: 8,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 120,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 16000,
			Channels:  2,
		},
		PayloadType: 121,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 122,
	},
}

// IncomingTrack is an incoming track.
type IncomingTrack struct {
	OnPacketRTP func(*rtp.Packet)

	track     *webrtc.TrackRemote
	receiver  *webrtc.RTPReceiver
	writeRTCP func([]rtcp.Packet) error
	log       logger.Writer

	packetsLost *counterdumper.CounterDumper
	rtpReceiver *rtpreceiver.Receiver

	seqRewriter struct {
		enabled       bool
		nextOutSeq    uint16
		initialized   bool
		packetsOut    uint64
		fragmentsOut  uint64
	}
}

func (t *IncomingTrack) initialize() {
	t.OnPacketRTP = func(*rtp.Packet) {}

	if t.track.Kind() == webrtc.RTPCodecTypeVideo {
		t.seqRewriter.enabled = true
		t.log.Log(logger.Info, "[SeqRewriter] Enabled for video track")
	}
}

// Codec returns the track codec.
func (t *IncomingTrack) Codec() webrtc.RTPCodecParameters {
	return t.track.Codec()
}

// ClockRate returns the clock rate. Needed by rtptime.GlobalDecoder
func (t *IncomingTrack) ClockRate() int {
	return int(t.track.Codec().ClockRate)
}

// PTSEqualsDTS returns whether PTS equals DTS. Needed by rtptime.GlobalDecoder
func (*IncomingTrack) PTSEqualsDTS(*rtp.Packet) bool {
	return true
}

func (t *IncomingTrack) fragmentSingleNAL(pkt *rtp.Packet) []*rtp.Packet {
	nalHeader := pkt.Payload[0]
	nalType := nalHeader & 0x1F
	nalNRI := nalHeader & 0x60  // Extract NRI bits (priority)
	
	payload := pkt.Payload[1:]  // Skip NAL header
	maxChunkSize := 1198        // 1200 - 2 bytes for FU headers
	
	numChunks := (len(payload) + maxChunkSize - 1) / maxChunkSize
	fragments := make([]*rtp.Packet, numChunks)
	
	t.log.Log(logger.Debug, "[SeqRewriter] Fragmenting single NAL type=%d, %d bytes → %d FU-A packets",
		nalType, len(pkt.Payload), numChunks)
	
	for i := 0; i < numChunks; i++ {
		start := i * maxChunkSize
		end := start + maxChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		
		chunkData := payload[start:end]
		
		// Create FU-A packet
		fragments[i] = &rtp.Packet{
			Header: rtp.Header{
				Version:        pkt.Version,
				Padding:        false,
				Extension:      pkt.Extension,
				ExtensionProfile: pkt.ExtensionProfile,
				Extensions:     pkt.Extensions,
				Marker:         (i == numChunks-1) && pkt.Marker,  // Marker only on last
				PayloadType:    pkt.PayloadType,
				SequenceNumber: 0,  // Will be rewritten later
				Timestamp:      pkt.Timestamp,
				SSRC:           pkt.SSRC,
				CSRC:           pkt.CSRC,
			},
			Payload: make([]byte, 2+len(chunkData)),
		}
		
		// FU Indicator: type=28 (FU-A) + NRI from original
		fragments[i].Payload[0] = 28 | nalNRI
		
		// FU Header: S|E|R|NAL_TYPE
		fuHeader := nalType
		if i == 0 {
			fuHeader |= 0x80  // Start bit
		}
		if i == numChunks-1 {
			fuHeader |= 0x40  // End bit
		}
		fragments[i].Payload[1] = fuHeader
		
		// Copy data
		copy(fragments[i].Payload[2:], chunkData)
	}
	
	return fragments
}

// refragmentFUA splits an oversized FU-A packet into smaller FU-A packets
func (t *IncomingTrack) refragmentFUA(pkt *rtp.Packet) []*rtp.Packet {
	if len(pkt.Payload) < 2 {
		return []*rtp.Packet{pkt}
	}
	
	fuIndicator := pkt.Payload[0]
	fuHeader := pkt.Payload[1]
	
	isOriginalStart := (fuHeader & 0x80) != 0
	isOriginalEnd := (fuHeader & 0x40) != 0
	nalType := fuHeader & 0x1F
	
	payload := pkt.Payload[2:]
	maxChunkSize := 1198
	
	numChunks := (len(payload) + maxChunkSize - 1) / maxChunkSize
	fragments := make([]*rtp.Packet, numChunks)
	
	t.log.Log(logger.Debug, "[SeqRewriter] Re-fragmenting FU-A NAL type=%d, %d bytes → %d packets",
		nalType, len(pkt.Payload), numChunks)
	
	for i := 0; i < numChunks; i++ {
		start := i * maxChunkSize
		end := start + maxChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		
		chunkData := payload[start:end]
		
		fragments[i] = &rtp.Packet{
			Header: rtp.Header{
				Version:        pkt.Version,
				Padding:        false,
				Extension:      pkt.Extension,
				ExtensionProfile: pkt.ExtensionProfile,
				Extensions:     pkt.Extensions,
				Marker:         false,
				PayloadType:    pkt.PayloadType,
				SequenceNumber: 0,
				Timestamp:      pkt.Timestamp,
				SSRC:           pkt.SSRC,
				CSRC:           pkt.CSRC,
			},
			Payload: make([]byte, 2+len(chunkData)),
		}
		
		// Copy FU Indicator
		fragments[i].Payload[0] = fuIndicator
		
		// Build FU Header
		newFUHeader := nalType
		if i == 0 && isOriginalStart {
			newFUHeader |= 0x80
		}
		if i == numChunks-1 && isOriginalEnd {
			newFUHeader |= 0x40
			fragments[i].Marker = true
		}
		fragments[i].Payload[1] = newFUHeader
		
		copy(fragments[i].Payload[2:], chunkData)
	}
	
	return fragments
}

// rewriteAndForward assigns new sequence numbers and forwards packets
func (t *IncomingTrack) rewriteAndForward(packets []*rtp.Packet) {
	for _, pkt := range packets {
		// Initialize on first packet
		if !t.seqRewriter.initialized {
			t.seqRewriter.nextOutSeq = pkt.SequenceNumber
			t.seqRewriter.initialized = true
			t.log.Log(logger.Info, "[SeqRewriter] Initialized with starting SeqNum=%d", t.seqRewriter.nextOutSeq)
		}
		
		// Assign new sequence number
		originalSeq := pkt.SequenceNumber
		pkt.SequenceNumber = t.seqRewriter.nextOutSeq
		t.seqRewriter.nextOutSeq++
		
		// Forward packet
		t.OnPacketRTP(pkt)
		t.seqRewriter.packetsOut++
		
		// Debug log for first few packets
		if t.seqRewriter.packetsOut <= 10 {
			t.log.Log(logger.Debug, "[SeqRewriter] Packet #%d: OrigSeq=%d → NewSeq=%d, TS=%d, Size=%d, Marker=%v",
				t.seqRewriter.packetsOut, originalSeq, pkt.SequenceNumber, 
				pkt.Timestamp, len(pkt.Payload), pkt.Marker)
		}
	}
}

func (t *IncomingTrack) start() {
	t.packetsLost = &counterdumper.CounterDumper{
		OnReport: func(val uint64) {
			t.log.Log(logger.Warn, "%d RTP %s lost",
				val,
				func() string {
					if val == 1 {
						return "packet"
					}
					return "packets"
				}())
		},
	}
	t.packetsLost.Start()

	t.rtpReceiver = &rtpreceiver.Receiver{
		ClockRate:            int(t.track.Codec().ClockRate),
		UnrealiableTransport: true,
		Period:               1 * time.Second,
		WritePacketRTCP: func(p rtcp.Packet) {
			t.writeRTCP([]rtcp.Packet{p}) //nolint:errcheck
		},
	}
	err := t.rtpReceiver.Initialize()
	if err != nil {
		panic(err)
	}

	// read incoming RTCP packets.
	// incoming RTCP packets must always be read to make interceptors work.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err2 := t.receiver.Read(buf)
			if err2 != nil {
				return
			}

			pkts, err2 := rtcp.Unmarshal(buf[:n])
			if err2 != nil {
				panic(err2)
			}

			for _, pkt := range pkts {
				if sr, ok := pkt.(*rtcp.SenderReport); ok {
					t.rtpReceiver.ProcessSenderReport(sr, time.Now())
				}
			}
		}
	}()

	// send period key frame requests
	if t.track.Kind() == webrtc.RTPCodecTypeVideo {
		go func() {
			keyframeTicker := time.NewTicker(keyFrameInterval)
			defer keyframeTicker.Stop()

			for range keyframeTicker.C {
				err2 := t.writeRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: uint32(t.track.SSRC()),
					},
				})
				if err2 != nil {
					return
				}
			}
		}()
	}

	// read incoming RTP packets.
	go func() {
		// ========================================
		// RTP PACKET VALIDATION FOR FPT CAMERA
		// ========================================
		var lastTimestamp uint32
		var lastSeqNum uint16
		var frameCount uint64
		var totalPackets uint64
		var oversizedPackets uint64
		var timestampJumps uint64
		var markerBitFrames uint64
		
		isVideo := t.track.Kind() == webrtc.RTPCodecTypeVideo
		codecName := t.track.Codec().MimeType
		
		if isVideo {
			t.log.Log(logger.Info, "[RTP-Validator] Starting validation for %s (SSRC=%d)", codecName, t.track.SSRC())
		}
		
		for {
			pkt, _, err2 := t.track.ReadRTP()
			if err2 != nil {
				return
			}
			
			totalPackets++
			
			// ========================================
			// VALIDATION 1: Packet Size (MTU check)
			// ========================================
			payloadSize := len(pkt.Payload)
			if payloadSize > 1200 {
				oversizedPackets++
				if isVideo && oversizedPackets <= 5 {
					t.log.Log(logger.Warn, "[RTP-Validator] OVERSIZED packet #%d: %d bytes (max 1200) - May cause fragmentation!", 
						totalPackets, payloadSize)
				}
			}
			
			// ========================================
			// VALIDATION 2: Timestamp Jump Detection
			// ========================================
			if isVideo && lastTimestamp != 0 {
				tsDiff := pkt.Timestamp - lastTimestamp
				
				// For 30fps H.264: normal diff = 90000/30 = 3000 ticks
				// For 25fps: 90000/25 = 3600 ticks
				// Anything > 10000 is suspicious (> 111ms gap)
				if tsDiff > 10000 && tsDiff < 0xFFFF0000 { // Ignore wraparound
					timestampJumps++
					if timestampJumps <= 5 {
						t.log.Log(logger.Warn, "[RTP-Validator] TIMESTAMP JUMP detected: diff=%d ticks (%.1fms) - Expected ~3000-3600 for 25-30fps",
							tsDiff, float64(tsDiff)/90.0)
					}
				}
			}
			
			// ========================================
			// VALIDATION 3: Marker Bit (Frame End)
			// ========================================
			if isVideo && pkt.Marker {
				markerBitFrames++
				frameCount++
				
				// Log first 3 frames with detailed info
				if frameCount <= 3 {
					t.log.Log(logger.Info, "[RTP-Validator] Frame #%d END (marker=1): TS=%d, SeqNum=%d, Size=%d bytes",
						frameCount, pkt.Timestamp, pkt.SequenceNumber, payloadSize)
				}
				
				// Periodic summary every 100 frames
				if frameCount%100 == 0 {
					t.log.Log(logger.Info, "[RTP-Validator] Stats after %d frames: Packets=%d, Oversized=%d, TS-Jumps=%d",
						frameCount, totalPackets, oversizedPackets, timestampJumps)
					
					// Sequence rewriter stats
					if t.seqRewriter.enabled {
						t.log.Log(logger.Info, "[SeqRewriter] Stats: PacketsOut=%d, FragmentsCreated=%d",
							t.seqRewriter.packetsOut, t.seqRewriter.fragmentsOut)
					}
				}
			}
			
			// ========================================
			// VALIDATION 4: H.264 NAL Unit Analysis
			// ========================================
			if isVideo && codecName == "video/H264" && payloadSize > 0 {
				nalType := pkt.Payload[0] & 0x1F
				
				// Check for fragmentation (FU-A)
				if nalType == 28 { // FU-A (Fragmentation Unit)
					if payloadSize > 1 {
						fuHeader := pkt.Payload[1]
						isStart := (fuHeader & 0x80) != 0
						isEnd := (fuHeader & 0x40) != 0
						actualNalType := fuHeader & 0x1F
						
						if isStart && frameCount <= 3 {
							t.log.Log(logger.Debug, "[RTP-Validator] FU-A START: NAL type=%d, SeqNum=%d", actualNalType, pkt.SequenceNumber)
						}
						if isEnd && frameCount <= 3 {
							t.log.Log(logger.Debug, "[RTP-Validator] FU-A END: NAL type=%d, SeqNum=%d", actualNalType, pkt.SequenceNumber)
						}
					}
				} else if nalType == 7 { // SPS
					if frameCount <= 3 {
						t.log.Log(logger.Info, "[RTP-Validator] SPS (Sequence Parameter Set) received - Frame config data")
					}
				} else if nalType == 8 { // PPS
					if frameCount <= 3 {
						t.log.Log(logger.Info, "[RTP-Validator] PPS (Picture Parameter Set) received - Frame config data")
					}
				} else if nalType == 5 { // IDR (Keyframe)
					if frameCount <= 3 {
						t.log.Log(logger.Info, "[RTP-Validator] DR Keyframe received - Full frame data")
					}
				}
			}
			
			// ========================================
			// VALIDATION 5: Sequence Number Gap
			// ========================================
			if lastSeqNum != 0 {
				expectedSeq := lastSeqNum + 1
				if pkt.SequenceNumber != expectedSeq && pkt.SequenceNumber != 0 {
					gap := int(pkt.SequenceNumber) - int(expectedSeq)
					if gap < 0 {
						gap += 65536 // Handle wraparound
					}
					if gap > 1 && gap < 100 { // Ignore large gaps (likely restart)
						t.log.Log(logger.Warn, "[RTP-Validator] SEQUENCE GAP: Expected %d, got %d (gap=%d packets)",
							expectedSeq, pkt.SequenceNumber, gap)
					}
				}
			}
			
			lastTimestamp = pkt.Timestamp
			lastSeqNum = pkt.SequenceNumber

			packets, lost := t.rtpReceiver.ProcessPacket2(pkt, time.Now(), true)

			if lost != 0 {
				t.packetsLost.Add(lost)
				// do not return
			}

			for _, pkt := range packets {
			// sometimes Chrome sends empty RTP packets. ignore them.
			if len(pkt.Payload) == 0 {
				continue
			}

			// ========================================
			// SEQUENCE REWRITER: Handle ALL packets
			// ========================================
			var packetsToForward []*rtp.Packet
			
			if isVideo && len(pkt.Payload) > 1200 {
				// Oversized packet - needs fragmentation
				oversizedPackets++
				
				if oversizedPackets <= 5 {
					nalType := pkt.Payload[0] & 0x1F
					t.log.Log(logger.Info, "[SeqRewriter] Oversized packet #%d: %d bytes, NAL type=%d",
						oversizedPackets, len(pkt.Payload), nalType)
				}
				
				// Check NAL type
				nalType := pkt.Payload[0] & 0x1F
				
				if nalType == 28 {
					// Already FU-A - re-fragment
					packetsToForward = t.refragmentFUA(pkt)
					t.seqRewriter.fragmentsOut += uint64(len(packetsToForward))
				} else {
					// Single NAL unit - create FU-A fragments
					packetsToForward = t.fragmentSingleNAL(pkt)
					t.seqRewriter.fragmentsOut += uint64(len(packetsToForward))
				}
				
				if oversizedPackets <= 5 {
					t.log.Log(logger.Info, "[SeqRewriter]   → Created %d fragments", len(packetsToForward))
				}
			} else {
				// Normal-sized packet - forward as single packet
				packetsToForward = []*rtp.Packet{pkt}
			}
			
			// Rewrite sequence numbers and forward
			t.rewriteAndForward(packetsToForward)
		}
		}
	}()
}

// PacketNTP returns the packet NTP.
func (t *IncomingTrack) PacketNTP(pkt *rtp.Packet) (time.Time, bool) {
	return t.rtpReceiver.PacketNTP(pkt.Timestamp)
}

func (t *IncomingTrack) close() {
	if t.packetsLost != nil {
		t.packetsLost.Stop()
	}
	if t.rtpReceiver != nil {
		t.rtpReceiver.Close()
	}
}
