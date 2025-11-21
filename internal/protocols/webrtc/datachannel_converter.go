package webrtc

import (
	"fmt"
	"sync"
	
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// DataChannelToRTPConverter converts H.264 chunks from DataChannel to RTP packets
type DataChannelToRTPConverter struct {
	videoTrack *webrtc.TrackLocalStaticRTP
	
	videoSeqNum    uint16
	videoTimestamp uint32
	videoSSRC      uint32
	
	mu  sync.Mutex
	log logger.Writer
}

// NewDataChannelToRTPConverter creates a new converter instance
func NewDataChannelToRTPConverter(log logger.Writer) *DataChannelToRTPConverter {
	return &DataChannelToRTPConverter{
		videoSSRC: 1234, // Match camera SSRC if possible
		log:       log,
	}
}

// ProcessH264Chunk converts H.264 NAL unit to RTP packets
func (c *DataChannelToRTPConverter) ProcessH264Chunk(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Minimum size check
	if len(data) < 5 {
		return fmt.Errorf("chunk too small: %d bytes", len(data))
	}
	
	// Check for Annex B start code (0x00 0x00 0x00 0x01 or 0x00 0x00 0x01)
	nalStart := 0
	if data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x00 && data[3] == 0x01 {
		nalStart = 4
	} else if data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x01 {
		nalStart = 3
	} else {
		return fmt.Errorf("no H.264 start code found")
	}
	
	nalUnit := data[nalStart:]
	nalType := nalUnit[0] & 0x1F
	
	c.log.Log(logger.Debug, "[Converter] NAL type %d, size %d bytes", nalType, len(nalUnit))
	
	// Fragment large NAL units (MTU = 1200 bytes)
	const maxPayloadSize = 1200
	
	if len(nalUnit) <= maxPayloadSize {
		// Single NAL unit packet
		return c.sendRTPPacket(nalUnit)
	}
	
	// FU-A fragmentation (RFC 6184 Section 5.8)
	return c.fragmentNALUnit(nalUnit, nalType)
}

// sendRTPPacket sends a single RTP packet
func (c *DataChannelToRTPConverter) sendRTPPacket(payload []byte) error {
	if c.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}
	
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         true, // Mark end of frame
			PayloadType:    102,  // H.264
			SequenceNumber: c.videoSeqNum,
			Timestamp:      c.videoTimestamp,
			SSRC:           c.videoSSRC,
		},
		Payload: payload,
	}
	
	c.videoSeqNum++
	c.videoTimestamp += 3000 // 90kHz / 30fps = 3000
	
	return c.videoTrack.WriteRTP(packet)
}

// fragmentNALUnit splits large NAL units into FU-A fragments
func (c *DataChannelToRTPConverter) fragmentNALUnit(nalUnit []byte, nalType byte) error {
	const maxFragmentSize = 1200
	
	nalHeader := nalUnit[0]
	nalPayload := nalUnit[1:]
	
	numFragments := (len(nalPayload) + maxFragmentSize - 1) / maxFragmentSize
	
	for i := 0; i < numFragments; i++ {
		start := i * maxFragmentSize
		end := start + maxFragmentSize
		if end > len(nalPayload) {
			end = len(nalPayload)
		}
		
		fragment := nalPayload[start:end]
		
		// FU-A indicator (same as NAL header but type = 28)
		fuIndicator := (nalHeader & 0xE0) | 28
		
		// FU-A header
		fuHeader := byte(0)
		if i == 0 {
			fuHeader |= 0x80 // Start bit
		}
		if i == numFragments-1 {
			fuHeader |= 0x40 // End bit
		}
		fuHeader |= nalType // Original NAL type
		
		payload := make([]byte, 2+len(fragment))
		payload[0] = fuIndicator
		payload[1] = fuHeader
		copy(payload[2:], fragment)
		
		marker := i == numFragments-1
		
		err := c.sendRTPFragment(payload, marker)
		if err != nil {
			return err
		}
	}
	
	return nil
}

// sendRTPFragment sends a fragmented RTP packet
func (c *DataChannelToRTPConverter) sendRTPFragment(payload []byte, marker bool) error {
	if c.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}
	
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    102,
			SequenceNumber: c.videoSeqNum,
			Timestamp:      c.videoTimestamp,
			SSRC:           c.videoSSRC,
		},
		Payload: payload,
	}
	
	c.videoSeqNum++
	if marker {
		c.videoTimestamp += 3000 // Update timestamp only for last fragment
	}
	
	return c.videoTrack.WriteRTP(packet)
}

// SetVideoTrack assigns the RTP track for output
func (c *DataChannelToRTPConverter) SetVideoTrack(track *webrtc.TrackLocalStaticRTP) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.videoTrack = track
	c.log.Log(logger.Info, "[Converter] Video track assigned")
}