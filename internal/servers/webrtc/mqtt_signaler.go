package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type PubAPI interface {
	CreatePublisherFromOffer(path string, offerSDP string) (answerSDP string, err error)
	AddRemoteCandidate(path string, cand string) error
	OnLocalICE(path string, fn func(cand string))
}

type MQTTSignaler struct {
	cli       mqtt.Client
	brokerURL string
	topicPref string
	api       PubAPI
	logger    func(format string, args ...any)

	onceStart sync.Once
}

type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "ice"
	Path string `json:"path"`
	SDP  string `json:"sdp,omitempty"`
	Cand string `json:"candidate,omitempty"`
}

func NewMQTTSignaler(brokerURL, topicPref string, api PubAPI, logger func(string, ...any)) *MQTTSignaler {
	if logger == nil {
		logger = func(format string, args ...any) {}
	}
	return &MQTTSignaler{
		brokerURL: brokerURL,
		topicPref: topicPref,
		api:       api,
		logger:    logger,
	}
}

func (m *MQTTSignaler) Start(ctx context.Context) error {
	var retErr error
	m.onceStart.Do(func() {
		opts := mqtt.NewClientOptions().
			AddBroker(m.brokerURL).
			SetClientID(fmt.Sprintf("mediamtx-webrtc-%d", time.Now().UnixNano())).
			SetAutoReconnect(true).
			SetConnectRetry(true).
			SetResumeSubs(true).
			SetCleanSession(false).
			SetOrderMatters(false).
			SetKeepAlive(30 * time.Second).
			SetPingTimeout(10 * time.Second).
			SetConnectTimeout(10 * time.Second)  // ← CRITICAL: Add timeout

		opts.OnConnect = func(c mqtt.Client) {
			m.logger("[WebRTC] MQTT connected, subscribing...")
			
			offerTopic := m.topicPref + "/offer/+"
			iceUpTopic := m.topicPref + "/ice-up/+"

			if tok := c.Subscribe(offerTopic, 1, m.onOffer); tok.Wait() && tok.Error() != nil {
				m.logger("[WebRTC] MQTT subscribe error on %s: %v", offerTopic, tok.Error())
				return
			}
			m.logger("[WebRTC] MQTT subscribed %s", offerTopic)

			if tok := c.Subscribe(iceUpTopic, 1, m.onRemoteICE); tok.Wait() && tok.Error() != nil {
				m.logger("[WebRTC] MQTT subscribe error on %s: %v", iceUpTopic, tok.Error())
				return
			}
			m.logger("[WebRTC] MQTT subscribed %s", iceUpTopic)
		}

		opts.OnConnectionLost = func(_ mqtt.Client, err error) {
			m.logger("[WebRTC] MQTT connection lost: %v", err)
		}

		m.cli = mqtt.NewClient(opts)

		m.logger("[WebRTC] MQTTSignaler connecting to %s (prefix=%s)...", m.brokerURL, m.topicPref)
		
		tok := m.cli.Connect()
		
		// FIX: Use WaitTimeout instead of Wait
		if !tok.WaitTimeout(10 * time.Second) {
			retErr = fmt.Errorf("MQTT connect timeout (10s) - broker unreachable at %s", m.brokerURL)
			m.logger("[WebRTC] MQTT connect timeout - broker not available")
			return
		}
		
		if tok.Error() != nil {
			retErr = fmt.Errorf("MQTT connect error: %w", tok.Error())
			m.logger("[WebRTC] MQTT connect error: %v", tok.Error())
			return
		}

		m.logger("[WebRTC] MQTT client connected successfully")

		go func() {
			<-ctx.Done()
			m.logger("[WebRTC] MQTTSignaler shutting down")
			m.cli.Disconnect(250)
		}()
	})

	return retErr
}

func (m *MQTTSignaler) onOffer(_ mqtt.Client, msg mqtt.Message) {
	m.logger("[WebRTC] onOffer got payload on %s", msg.Topic())
	var in sdpMsg
	if err := json.Unmarshal(msg.Payload(), &in); err != nil {
		m.logger("[WebRTC] ERROR: Failed to unmarshal offer: %v", err)
		return
	}
	
	if in.Path == "" || in.Type != "offer" || in.SDP == "" {
		m.logger("[WebRTC] ERROR: Invalid offer message - path=%s type=%s sdp_len=%d", in.Path, in.Type, len(in.SDP))
		return
	}
	
	m.logger("[WebRTC] Calling CreatePublisherFromOffer for path=%s", in.Path)
	
	go func() {
		answer, err := m.api.CreatePublisherFromOffer(in.Path, in.SDP)
		if err != nil {
			m.logger("[WebRTC] CreatePublisherFromOffer failed path=%s err=%v", in.Path, err)
			return
		}
		
		if answer == "" {
			m.logger("[WebRTC] ERROR: empty answer received from CreatePublisherFromOffer")
			return
		}
		
		m.logger("[WebRTC] Got answer from API (length=%d), publishing to MQTT", len(answer))
		
		out := sdpMsg{Type: "answer", Path: in.Path, SDP: answer}
		b, err := json.Marshal(out)
		if err != nil {
			m.logger("[WebRTC] ERROR: Failed to marshal answer: %v", err)
			return
		}
		
		topic := m.topicPref + "/answer/" + in.Path
		m.logger("[WebRTC] Publishing answer to MQTT topic: %s", topic)
		
		tok := m.cli.Publish(topic, 1, false, b)
		if !tok.WaitTimeout(5 * time.Second) {
			m.logger("[WebRTC] ERROR: Timeout publishing answer")
			return
		}
		
		if tok.Error() != nil {
			m.logger("[WebRTC] ERROR: Failed to publish answer: %v", tok.Error())
			return
		}
		
		m.logger("[WebRTC] Answer published successfully to MQTT")
		
		// Add post-answer monitoring
		m.logger("[WebRTC] ========== POST-ANSWER DEBUG ==========")
		m.logger("[WebRTC] Camera should start sending RTP now...")
		m.logger("[WebRTC] Monitoring connection for next 10 seconds...")
		
		// Register for ICE local candidates (when camera processes our answer)
		m.api.OnLocalICE(in.Path, func(cand string) {
			m.logger("[WebRTC] [POST-ANSWER] Local ICE candidate: %s", cand)
			m.PublishLocalICE(in.Path, cand)
		})
		
		m.logger("[WebRTC] ==========================================")
	}()
}

func (m *MQTTSignaler) onRemoteICE(_ mqtt.Client, msg mqtt.Message) {
	var in sdpMsg
	_ = json.Unmarshal(msg.Payload(), &in)
	if in.Path == "" || in.Type != "ice" || in.Cand == "" {
		return
	}
	_ = m.api.AddRemoteCandidate(in.Path, in.Cand)
}

func (m *MQTTSignaler) PublishLocalICE(path string, cand string) {
	out := sdpMsg{Type: "ice", Path: path, Cand: cand}
	b, _ := json.Marshal(out)
	m.cli.Publish(m.topicPref+"/ice-down/"+path, 1, false, b)
}

func (m *MQTTSignaler) PublishAnswer(path, sdp string) {
	out := sdpMsg{Type: "answer", Path: path, SDP: sdp}
	b, _ := json.Marshal(out)
	topic := m.topicPref + "/answer/" + path
	m.logger("[WebRTC] PublishAnswer to %s", topic)
	m.cli.Publish(topic, 1, false, b)
}