package webrtc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	pwebrtc "github.com/pion/webrtc/v4"
)

// ============ HTTP API ENDPOINTS ============

// APIStartRecording triggers recording for a path
// POST /v3/webrtc/record/start
// Body: {"path": "e042f5010000020"}
func (s *Server) APIStartRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Path == "" {
		http.Error(w, "Missing 'path' field", http.StatusBadRequest)
		return
	}

	// Check if recorder is initialized
	if s.recorder == nil {
		http.Error(w, "Recorder not initialized", http.StatusInternalServerError)
		return
	}

	// Check if already recording
	if s.recorder.IsRecording(req.Path) {
		http.Error(w, fmt.Sprintf("Already recording path: %s", req.Path), http.StatusConflict)
		return
	}

	// ========================================================================
	// Get active session via global registry
	// ========================================================================
	targetSession := GetActiveSession(req.Path)
	if targetSession == nil {
		http.Error(w, fmt.Sprintf("No active stream for path: %s", req.Path), http.StatusNotFound)
		return
	}

	// ========================================================================
	// Get video track from session
	// ========================================================================
	videoTrack := getVideoTrackFromSession(targetSession)
	if videoTrack == nil {
		// Try to wait a bit for track to arrive
		s.Log(logger.Warn, "[API] No video track yet, waiting 2s...")
		time.Sleep(2 * time.Second)
		
		videoTrack = getVideoTrackFromSession(targetSession)
		if videoTrack == nil {
			http.Error(w, "No video track available (session may not be fully connected)", http.StatusBadRequest)
			return
		}
	}

	// ========================================================================
	// Start recording
	// ========================================================================
	if err := s.recorder.StartRecording(req.Path, videoTrack); err != nil {
		s.Log(logger.Error, "[API] Start recording failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Log(logger.Info, "[API] Recording started for path: %s", req.Path)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "recording",
		"path":       req.Path,
		"session_id": targetSession.GetSessionID().String(),
		"message":    "Recording started successfully",
	})
}

// APIStopRecording stops recording for a path
// POST /v3/webrtc/record/stop
// Body: {"path": "e042f5010000020"}
func (s *Server) APIStopRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req struct {
		Path string `json:"path"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	if req.Path == "" {
		http.Error(w, "Missing 'path' field", http.StatusBadRequest)
		return
	}
	
	if s.recorder == nil {
		http.Error(w, "Recorder not initialized", http.StatusInternalServerError)
		return
	}
	
	if err := s.recorder.StopRecording(req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	
	s.Log(logger.Info, "[API] Recording stopped for path: %s", req.Path)
	
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "stopped",
		"path":    req.Path,
		"message": "Recording stopped successfully",
	})
}

// APIListRecordings lists all recorded files
// GET /v3/webrtc/record/list
func (s *Server) APIListRecordings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	if s.recorder == nil {
		http.Error(w, "Recorder not initialized", http.StatusInternalServerError)
		return
	}
	
	outputDir := s.recorder.outputDir
	files, err := os.ReadDir(outputDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	type FileInfo struct {
		Name         string `json:"name"`
		SizeMB       int64  `json:"size_mb"`
		ModifiedTime string `json:"modified_time"`
	}
	
	recordings := make([]FileInfo, 0)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		
		info, err := file.Info()
		if err != nil {
			continue
		}
		
		recordings = append(recordings, FileInfo{
			Name:         file.Name(),
			SizeMB:       info.Size() / (1024 * 1024),
			ModifiedTime: info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"recordings":     recordings,
		"total":          len(recordings),
		"active_streams": s.recorder.GetActiveSessions(),
	})
}

// APIDownloadRecording serves a recording file for download
// GET /v3/webrtc/record/download?file=e042f5010000020_20250104_123456.mp4
func (s *Server) APIDownloadRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	filename := r.URL.Query().Get("file")
	if filename == "" {
		http.Error(w, "Missing 'file' parameter", http.StatusBadRequest)
		return
	}
	
	if s.recorder == nil {
		http.Error(w, "Recorder not initialized", http.StatusInternalServerError)
		return
	}
	
	// Security: prevent path traversal
	if filepath.Base(filename) != filename {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	
	fullPath := filepath.Join(s.recorder.outputDir, filename)
	
	// Check if file exists
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	
	http.ServeFile(w, r, fullPath)
}

// APIRecordingStatus returns current recording status
// GET /v3/webrtc/record/status
func (s *Server) APIRecordingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	if s.recorder == nil {
		http.Error(w, "Recorder not initialized", http.StatusInternalServerError)
		return
	}
	
	activeSessions := s.recorder.GetActiveSessions()
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":         true,
		"active_sessions": activeSessions,
		"count":           len(activeSessions),
	})
}

// ============ HELPER FUNCTIONS ============

// getVideoTrackFromSession extracts video track from PeerConnection
// This is a workaround since session doesn't expose tracks directly
func getVideoTrackFromSession(sx *session) *pwebrtc.TrackRemote {
	if sx == nil {
		return nil
	}

	// ========================================================================
	// METHOD 1: Check if session has stored video track
	// ========================================================================
	sx.trackMutex.RLock()
	track := sx.incomingVideoTrack
	sx.trackMutex.RUnlock()

	if track != nil {
		return track
	}

	// ========================================================================
	// METHOD 2: Try to access PeerConnection (may not work)
	// ========================================================================
	sx.mutex.RLock()
	pc := sx.pc
	sx.mutex.RUnlock()

	if pc == nil {
		return nil
	}

	// MediaMTX's PeerConnection is a wrapper, we can't access tracks directly
	// The only way is to store tracks when they arrive via OnTrack callback
	
	// For now, return nil and log warning
	// Tracks must be captured during session initialization
	return nil
}