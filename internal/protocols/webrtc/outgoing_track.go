package webrtc

import (
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
	rtcpSender     *rtpsender.Sender
	rtpPacketsSent *uint64

	mu    sync.RWMutex
	ready bool
	
	packetBuffer []*rtpPacketWithNTP
	bufferMu     sync.Mutex
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

	t.ssrc = uint32(sender.GetParameters().Encodings[0].SSRC)

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

func (t *OutgoingTrack) writeRTPInternal(pkt *rtp.Packet, ntp time.Time) error {
	// use right SSRC in packet to make rtcpSender work
	pkt.SSRC = t.ssrc

	t.rtcpSender.ProcessPacket(pkt, ntp, true)

	return t.track.WriteRTP(pkt)
}