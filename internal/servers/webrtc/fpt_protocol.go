package webrtc

import (
	"encoding/json"
	"time"
)

type FPTMessage struct {
	Method      string          `json:"Method"`              // "ACT"
	MessageType string          `json:"MessageType"`         // "Signaling"
	Serial      string          `json:"Serial"`              // Camera serial
	Data        json.RawMessage `json:"Data"`                // Nested data
	Result      *FPTResult      `json:"Result,omitempty"`    // Optional result
	Timestamp   int64           `json:"Timestamp,omitempty"` // Unix timestamp
}

type FPTResult struct {
	Ret     int    `json:"Ret"`     // 100 = success
	Message string `json:"Message"` // "Success"
}

// ============ REQUEST MESSAGES (MediaMTX → Camera) ============

type FPTRequestData struct {
	Type     string `json:"Type"`     // "request"
	ClientID string `json:"ClientId"` // Unique session ID
}

type FPTAnswerData struct {
	Type     string `json:"Type"`     // "answer"
	ClientID string `json:"ClientId"`
	Sdp      string `json:"Sdp"`
}

// ============ RESPONSE MESSAGES (Camera → MediaMTX) ============

type FPTOfferData struct {
	Type       string   `json:"Type"`       // "offer"
	ClientID   string   `json:"ClientId"`   // Camera thường đổi thành ipc-user-{serial}-xxx
	Sdp        string   `json:"Sdp"`
	IceServers []string `json:"IceServers,omitempty"`
	
	// Thêm lại các field bị thiếu
	CleanMax       int `json:"CleanMax,omitempty"`       // Max concurrent viewers
	CurrentClients *struct {
		Total int `json:"total"` // Current viewer count
	} `json:"CurrentClients,omitempty"`
}

type FPTDenyData struct {
	Type     string `json:"Type"`     // "deny"
	ClientID string `json:"ClientId"`
	Message  string `json:"Message,omitempty"`
	
	// Thêm lại field bị thiếu
	CurrentClients *struct {
		Total int `json:"total"`
	} `json:"CurrentClients,omitempty"`
}

type FPTSuccessData struct {
	Type     string `json:"Type"`     // "ccu" hoặc "success"
	ClientID string `json:"ClientId,omitempty"`
	
	// Thêm lại field bị thiếu
	CurrentClients *struct {
		Total int `json:"total"`
	} `json:"CurrentClients,omitempty"`
	
	Result *struct {
		Ref     int    `json:"Ref"`
		Message string `json:"Message"`
	} `json:"Result,omitempty"`
}

// ============ MESSAGE CONSTRUCTORS ============

func NewFPTRequestMessage(serial, clientID string) *FPTMessage {
	data := FPTRequestData{
		Type:     "request",
		ClientID: clientID,
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

func NewFPTAnswerMessage(serial, clientID, sdp string) *FPTMessage {
	data := FPTAnswerData{
		Type:     "answer",
		ClientID: clientID,
		Sdp:      sdp,
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

// ============ HELPER METHODS ============

func (m *FPTMessage) ParseOfferData() (*FPTOfferData, error) {
	var data FPTOfferData
	err := json.Unmarshal(m.Data, &data)
	return &data, err
}

func (m *FPTMessage) ParseDenyData() (*FPTDenyData, error) {
	var data FPTDenyData
	err := json.Unmarshal(m.Data, &data)
	return &data, err
}

func (m *FPTMessage) ParseSuccessData() (*FPTSuccessData, error) {
	var data FPTSuccessData
	err := json.Unmarshal(m.Data, &data)
	return &data, err
}