package webrtc

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpsender"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// OutgoingTrack is a WebRTC outgoing track
type OutgoingTrack struct {
	Caps webrtc.RTPCodecCapability

	track          *webrtc.TrackLocalStaticRTP
	ssrc           uint32
	negotiatedPT   webrtc.PayloadType
	rtcpSender     *rtpsender.Sender
	rtpPacketsSent *uint64

	mu    sync.RWMutex
	ready bool
	
	packetBuffer []*rtpPacketWithNTP
	bufferMu     sync.Mutex

	remapCount  int
    remapLogged bool
}

type rtpPacketWithNTP struct {
	pkt *rtp.Packet
	ntp time.Time
}

func (t *OutgoingTrack) isVideo() bool {
	return strings.Split(t.Caps.MimeType, "/")[0] == "video"
}

func (t *OutgoingTrack) setup(p *PeerConnection) error {
	var trackID string
	if t.isVideo() {
		trackID = "video"
	} else {
		trackID = "audio"
	}

	var err error
	t.track, err = webrtc.NewTrackLocalStaticRTP(
		t.Caps,
		trackID,
		webrtcStreamID,
	)
	if err != nil {
		return err
	}

	sender, err := p.wr.AddTrack(t.track)
	if err != nil {
		return err
	}

	params := sender.GetParameters()
	t.ssrc = uint32(params.Encodings[0].SSRC)

	// PT will be set later via SetNegotiatedPT() after SDP negotiation
	t.negotiatedPT = 0
	p.Log.Log(logger.Info, "[OutgoingTrack] Track %s created, PT will be set after negotiation", trackID)

	t.rtcpSender = &rtpsender.Sender{
		ClockRate: int(t.track.Codec().ClockRate),
		Period:    1 * time.Second,
		TimeNow:   time.Now,
		WritePacketRTCP: func(pkt rtcp.Packet) {
			p.wr.WriteRTCP([]rtcp.Packet{pkt}) //nolint:errcheck
		},
	}
	t.rtcpSender.Initialize()

	// t.rtpPacketsSent = p.rtpPacketsSent

	t.bufferMu.Lock()
	buffered := t.packetBuffer
	t.packetBuffer = nil
	t.bufferMu.Unlock()
	
	if len(buffered) > 0 {
		p.Log.Log(logger.Info, "[OutgoingTrack] Flushing %d buffered packets for %s", 
			len(buffered), trackID)
		for _, item := range buffered {
			t.writeRTPInternal(item.pkt, item.ntp)
		}
	}

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()

	// incoming RTCP packets must always be read to make interceptors work
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err2 := sender.Read(buf)
			if err2 != nil {
				return
			}

			_, err2 = rtcp.Unmarshal(buf[:n])
			if err2 != nil {
				panic(err2)
			}
		}
	}()

	return nil
}

func (t *OutgoingTrack) close() {
	if t.rtcpSender != nil {
		t.rtcpSender.Close()
	}
}

func (t *OutgoingTrack) SetNegotiatedPT(pc *PeerConnection, trackID string) {
	desc := pc.wr.LocalDescription()
	if desc == nil {
		pc.Log.Log(logger.Warn, "[OutgoingTrack] No LocalDescription available for %s", trackID)
		return
	}

	// Parse SDP manually to find PT
	lines := strings.Split(desc.SDP, "\n")
	inTargetMedia := false
	
	for _, line := range lines {
		line = strings.TrimSpace(line)
		
		// Find media section matching our track type
		if strings.HasPrefix(line, "m=") {
			if t.isVideo() && strings.HasPrefix(line, "m=video") {
				inTargetMedia = true
			} else if !t.isVideo() && strings.HasPrefix(line, "m=audio") {
				inTargetMedia = true
			} else {
				inTargetMedia = false
			}
		}
		
		// Extract PT from m= line
		if inTargetMedia && strings.HasPrefix(line, "m=") {
			// Format: m=video 9 UDP/TLS/RTP/SAVPF 103 104
			parts := strings.Fields(line)
			if len(parts) > 3 {
				// PT is 4th field (index 3)
				ptStr := parts[3]
				var pt uint8
				_, err := fmt.Sscanf(ptStr, "%d", &pt)
				if err == nil {
					t.negotiatedPT = webrtc.PayloadType(pt)
					pc.Log.Log(logger.Info, "[OutgoingTrack] Track %s negotiated PT = %d (from SDP)", 
						trackID, pt)
					return
				}
			}
		}
	}

	pc.Log.Log(logger.Error, "[OutgoingTrack] Could not find PT for %s in LocalDescription!", trackID)
}

// WriteRTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTP(pkt *rtp.Packet) error {
	return t.WriteRTPWithNTP(pkt, time.Now())
}

// WriteRTPWithNTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTPWithNTP(pkt *rtp.Packet, ntp time.Time) error {
	t.mu.RLock()
	ready := t.ready
	t.mu.RUnlock()
	
	if !ready {
		// FIX: Buffer packet instead of dropping it
		t.bufferMu.Lock()
		defer t.bufferMu.Unlock()
		
		// Limit buffer size to prevent memory leak
		if len(t.packetBuffer) < 100 {
			// Clone packet to avoid race condition
			cloned := &rtp.Packet{
				Header:  pkt.Header,
				Payload: make([]byte, len(pkt.Payload)),
			}
			copy(cloned.Payload, pkt.Payload)
			
			t.packetBuffer = append(t.packetBuffer, &rtpPacketWithNTP{
				pkt: cloned,
				ntp: ntp,
			})
		}
		// Silently drop if buffer full (very rare)
		return nil
	}
	
	return t.writeRTPInternal(pkt, ntp)
}

// func (t *OutgoingTrack) writeRTPInternal(pkt *rtp.Packet, ntp time.Time) error {
// 	// use right SSRC in packet to make rtcpSender work
// 	pkt.SSRC = t.ssrc

//     if pkt.PayloadType != uint8(t.negotiatedPT) {
//         pkt.PayloadType = uint8(t.negotiatedPT)
//     }

// 	t.rtcpSender.ProcessPacket(pkt, ntp, true)

// 	return t.track.WriteRTP(pkt)
// }

func (t *OutgoingTrack) writeRTPInternal(pkt *rtp.Packet, ntp time.Time) error {
	// use right SSRC in packet to make rtcpSender work
	pkt.SSRC = t.ssrc

	// REMAP PT if needed
	originalPT := pkt.PayloadType
	targetPT := uint8(t.negotiatedPT)
	
	if originalPT != targetPT && targetPT != 0 {
		// LOG first remap event with details
		if !t.remapLogged {
			fmt.Printf("[writeRTPInternal] PT REMAP ACTIVE: %d → %d (negotiated)\n", originalPT, targetPT)
			t.remapLogged = true
		}
		
		// Count total remaps (for debugging)
		t.remapCount++
		if t.remapCount <= 5 {
			fmt.Printf("[writeRTPInternal] Packet #%d: Remapping PT %d → %d\n", t.remapCount, originalPT, targetPT)
		}
		
		pkt.PayloadType = targetPT
	}
	
	// THÊM LOG NÀY - Xem PT thực tế TRƯỚC KHI gửi
	if t.remapCount <= 10 && t.remapCount > 0 {
		fmt.Printf("[WHEP-Send] Final PT=%d (original=%d), SSRC=%d, Seq=%d, TS=%d\n", 
			pkt.PayloadType, originalPT, pkt.SSRC, pkt.SequenceNumber, pkt.Timestamp)
	}

	t.rtcpSender.ProcessPacket(pkt, ntp, true)
	return t.track.WriteRTP(pkt)
}