package webrtc

import (
	"fmt"
	"time"
)

// TimingMetrics để track thời gian từng bước
type PublishTimings struct {
	StartTime      time.Time     // Bắt đầu publish request
	PublishCall    time.Time     // Gọi cli.Publish()
	WaitStart      time.Time     // Bắt đầu WaitTimeout()
	WaitComplete   time.Time     // WaitTimeout() hoàn thành
	TotalDuration  time.Duration
	
	PublishDuration time.Duration // Từ call đến hoàn thành
	WaitDuration    time.Duration // Thời gian chờ WaitTimeout
}

func NewPublishTimings() *PublishTimings {
	return &PublishTimings{
		StartTime: time.Now(),
	}
}

func (pt *PublishTimings) MarkPublishCall() {
	pt.PublishCall = time.Now()
}

func (pt *PublishTimings) MarkWaitStart() {
	pt.WaitStart = time.Now()
}

func (pt *PublishTimings) MarkWaitComplete() {
	pt.WaitComplete = time.Now()
	pt.PublishDuration = pt.WaitComplete.Sub(pt.PublishCall)
	pt.WaitDuration = pt.WaitComplete.Sub(pt.WaitStart)
	pt.TotalDuration = pt.WaitComplete.Sub(pt.StartTime)
}

func (pt *PublishTimings) String() string {
	return fmt.Sprintf(
		"[TIMING] Total: %v | PublishCall->Complete: %v | WaitTimeout: %v",
		pt.TotalDuration, pt.PublishDuration, pt.WaitDuration,
	)
}

// Ví dụ sử dụng trong trigger stream:
// 
// timing := NewPublishTimings()
// s.Log(logger.Info, "[FPT] [API] Starting publish... %v", timing.StartTime.Format("15:04:05.000"))
// 
// timing.MarkPublishCall()
// token := s.fptAdapter.cli.Publish(requestTopic, 1, false, payload)
// 
// timing.MarkWaitStart()
// waitSuccess := token.WaitTimeout(5 * time.Second)
// timing.MarkWaitComplete()
// 
// s.Log(logger.Info, "[FPT] [API] Publish completed: %s", timing)
// 
// if !waitSuccess {
//     s.Log(logger.Error, "[FPT] [API] Publish timeout! Details: %s", timing)
// }
