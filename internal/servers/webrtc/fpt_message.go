package webrtc

import (
	"encoding/json"
 	"fmt"
)

type FPTICEData struct {
	Type      string `json:"Type"`
	ClientID  string `json:"ClientID,omitempty"`
	Candidate string `json:"candidate"`
	SdpMid    string `json:"sdpMid,omitempty"`
	SdpMLineIndex int    `json:"sdpMLineIndex,omitempty"`
}

func (m *FPTMessage) ParseICEData() (*FPTICEData, error) {
	var data FPTICEData
	if err := json.Unmarshal(m.Data, &data); err != nil {
        return nil, err
    }
    
    if data.Candidate == "" {
        return nil, fmt.Errorf("empty ICE candidate")
    }
    
    return &data, nil
}