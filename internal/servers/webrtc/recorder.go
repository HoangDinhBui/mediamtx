package webrtc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/pion/webrtc/v4"
)

// Recorder handles recording WebRTC tracks to disk
type Recorder struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	logger    logger.Writer

	mu        sync.RWMutex
	sessions  map[string]*RecordSession // pathName → session
	outputDir string
	
	// Recording settings
	segmentDuration time.Duration // HLS segment length
	retentionPeriod time.Duration // Auto-delete old files after
}

// RecordSession represents an active recording session
type RecordSession struct {
	pathName    string
	startTime   time.Time
	outputPath  string
	
	// FFmpeg process
	ffmpegCmd   *exec.Cmd
	ffmpegStdin *os.File
	
	// Track info
	videoTrack *webrtc.TrackRemote
	
	mu       sync.Mutex
	isClosed bool
}

// NewRecorder creates a new recorder instance
func NewRecorder(outputDir string, log logger.Writer) *Recorder {
	ctx, cancel := context.WithCancel(context.Background())
	
	// Create output directory if not exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Log(logger.Error, "[Recorder] Failed to create output dir: %v", err)
	}
	
	r := &Recorder{
		ctx:             ctx,
		ctxCancel:       cancel,
		logger:          log,
		sessions:        make(map[string]*RecordSession),
		outputDir:       outputDir,
		segmentDuration: 10 * time.Second,  // 10s HLS segments
		retentionPeriod: 24 * time.Hour,    // Keep 24h of recordings
	}
	
	// Start cleanup goroutine
	go r.cleanupLoop()
	
	return r
}

// StartRecording begins recording a WebRTC track
func (r *Recorder) StartRecording(pathName string, videoTrack *webrtc.TrackRemote) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	// Check if already recording
	if _, exists := r.sessions[pathName]; exists {
		return fmt.Errorf("already recording path: %s", pathName)
	}
	
	timestamp := time.Now().Format("20060102_150405")
	outputPath := filepath.Join(r.outputDir, fmt.Sprintf("%s_%s.mp4", pathName, timestamp))
	
	r.Log(logger.Info, "[Recorder] Starting recording: %s → %s", pathName, outputPath)
	
	session := &RecordSession{
		pathName:   pathName,
		startTime:  time.Now(),
		outputPath: outputPath,
		videoTrack: videoTrack,
	}
	
	// Start FFmpeg process
	if err := session.startFFmpeg(r.ctx, r.logger); err != nil {
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}
	
	r.sessions[pathName] = session
	
	// Start reading RTP packets
	go session.readLoop(r.logger)
	
	return nil
}

// StopRecording stops recording for a path
func (r *Recorder) StopRecording(pathName string) error {
	r.mu.Lock()
	session, exists := r.sessions[pathName]
	delete(r.sessions, pathName)
	r.mu.Unlock()
	
	if !exists {
		return fmt.Errorf("no active recording for path: %s", pathName)
	}
	
	return session.Close()
}

// IsRecording checks if a path is being recorded
func (r *Recorder) IsRecording(pathName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.sessions[pathName]
	return exists
}

// GetActiveSessions returns list of active recording paths
func (r *Recorder) GetActiveSessions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	paths := make([]string, 0, len(r.sessions))
	for path := range r.sessions {
		paths = append(paths, path)
	}
	return paths
}

// Close stops all recordings and shuts down recorder
func (r *Recorder) Close() {
	r.mu.Lock()
	sessions := make([]*RecordSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.sessions = make(map[string]*RecordSession)
	r.mu.Unlock()
	
	// Stop all sessions
	for _, s := range sessions {
		s.Close()
	}
	
	r.ctxCancel()
	r.Log(logger.Info, "[Recorder] Shut down complete")
}

// startFFmpeg launches FFmpeg to receive RTP and encode to MP4
func (s *RecordSession) startFFmpeg(ctx context.Context, log logger.Writer) error {
	// FFmpeg command to read raw H264 from stdin and mux to MP4
	// -f h264: Input format is raw H264
	// -i pipe:0: Read from stdin
	// -c:v copy: Copy video without re-encoding
	// -movflags +faststart: Optimize for streaming playback
	args := []string{
		"-f", "h264",           // Input is raw H264
		"-i", "pipe:0",         // Read from stdin
		"-c:v", "copy",         // Copy codec (no re-encode)
		"-movflags", "+faststart", // MP4 optimization
		"-y",                   // Overwrite output file
		s.outputPath,
	}
	
	s.ffmpegCmd = exec.CommandContext(ctx, "ffmpeg", args...)
	
	// Create stdin pipe
	stdin, err := s.ffmpegCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe failed: %w", err)
	}
	s.ffmpegStdin = stdin.(*os.File)
	
	// Capture stderr for debugging
	s.ffmpegCmd.Stderr = os.Stderr
	
	if err := s.ffmpegCmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start failed: %w", err)
	}
	
	log.Log(logger.Info, "[Recorder] FFmpeg started (PID=%d)", s.ffmpegCmd.Process.Pid)
	
	// Monitor process
	go func() {
		err := s.ffmpegCmd.Wait()
		if err != nil && ctx.Err() == nil {
			log.Log(logger.Warn, "[Recorder] FFmpeg exited: %v", err)
		}
	}()
	
	return nil
}

// readLoop reads RTP packets from WebRTC track and writes to FFmpeg
func (s *RecordSession) readLoop(log logger.Writer) {
	log.Log(logger.Info, "[Recorder] Read loop started for %s", s.pathName)
	// r.Log(logger.Info, "[Recorder] Read loop started for %s", s.pathName)
	
	buffer := make([]byte, 1500) // MTU size
	
	for {
		s.mu.Lock()
		if s.isClosed {
			s.mu.Unlock()
			break
		}
		stdin := s.ffmpegStdin
		s.mu.Unlock()
		
		// Read RTP packet
		n, _, err := s.videoTrack.Read(buffer)
		if err != nil {
			log.Log(logger.Debug, "[Recorder] Read error: %v", err)
			break
		}
		
		// Write to FFmpeg stdin (raw H264 payload)
		if _, err := stdin.Write(buffer[:n]); err != nil {
			log.Log(logger.Error, "[Recorder] Write to FFmpeg failed: %v", err)
			break
		}
	}
	
	log.Log(logger.Info, "[Recorder] Read loop stopped for %s", s.pathName)
}

// Close stops the recording session
func (s *RecordSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if s.isClosed {
		return nil
	}
	s.isClosed = true
	
	// Close FFmpeg stdin to signal EOF
	if s.ffmpegStdin != nil {
		s.ffmpegStdin.Close()
	}
	
	// Wait for FFmpeg to finish (with timeout)
	if s.ffmpegCmd != nil && s.ffmpegCmd.Process != nil {
		done := make(chan error, 1)
		go func() {
			done <- s.ffmpegCmd.Wait()
		}()
		
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.ffmpegCmd.Process.Kill()
		}
	}
	
	duration := time.Since(s.startTime)
	fmt.Printf("[Recorder] Stopped recording %s (duration: %s)\n", s.pathName, duration)
	
	return nil
}

// cleanupLoop periodically deletes old recordings
func (r *Recorder) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.cleanupOldFiles()
		}
	}
}

// cleanupOldFiles removes recordings older than retention period
func (r *Recorder) cleanupOldFiles() {
	r.Log(logger.Debug, "[Recorder] Running cleanup (retention: %v)", r.retentionPeriod)
	
	files, err := os.ReadDir(r.outputDir)
	if err != nil {
		r.Log(logger.Error, "[Recorder] Cleanup readdir failed: %v", err)
		return
	}
	
	now := time.Now()
	deletedCount := 0
	
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		
		info, err := file.Info()
		if err != nil {
			continue
		}
		
		age := now.Sub(info.ModTime())
		if age > r.retentionPeriod {
			path := filepath.Join(r.outputDir, file.Name())
			if err := os.Remove(path); err == nil {
				deletedCount++
				r.Log(logger.Info, "[Recorder] Deleted old file: %s (age: %v)", file.Name(), age)
			}
		}
	}
	
	if deletedCount > 0 {
		r.Log(logger.Info, "[Recorder] Cleanup complete: deleted %d files", deletedCount)
	}
}

// Log helper
func (r *Recorder) Log(level logger.Level, format string, args ...interface{}) {
	r.logger.Log(level, format, args...)
}