package webrtc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"

	"github.com/pion/webrtc/v4"
)

//go:embed preview.html
var previewHTML []byte

// previewSession holds the state for one browser preview connection.
type previewSession struct {
	pc               *webrtc.PeerConnection
	serverCandidates []webrtc.ICECandidateInit
	mu               sync.Mutex
}

// PreviewServer serves a local browser page that displays the desktop camera stream.
// It creates its own PeerConnection (sharing the same hardware source as the mobile
// connection) so the two streams are completely independent.
type PreviewServer struct {
	manager  *StreamManager
	mu       sync.Mutex
	sessions map[string]*previewSession
}

// NewPreviewServer creates a PreviewServer backed by the given StreamManager.
func NewPreviewServer(manager *StreamManager) *PreviewServer {
	return &PreviewServer{
		manager:  manager,
		sessions: make(map[string]*previewSession),
	}
}

// Start begins serving on addr (e.g. "localhost:8081"). Blocks until error.
func (ps *PreviewServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ps.handleIndex)
	mux.HandleFunc("/session/cmd", ps.handleSessionCmd)
	mux.HandleFunc("/preview/offer", ps.handleOffer)
	mux.HandleFunc("/preview/answer", ps.handleAnswer)
	mux.HandleFunc("/preview/ice-candidate", ps.handleBrowserCandidate)
	mux.HandleFunc("/preview/server-candidates", ps.handleServerCandidates)

	log.Printf("🖥️  [Preview] Open http://%s in your browser to see Silver's camera", addr)
	return http.ListenAndServe(addr, mux)
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

func (ps *PreviewServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(previewHTML)
}

// createOffer builds a new preview PeerConnection, generates an SDP offer,
// registers it in ps.sessions, and returns the SDP and session ID.
func (ps *PreviewServer) createOffer() (sdp string, sessionID string, err error) {
	pc, err := ps.manager.NewPreviewPeerConnection()
	if err != nil {
		return "", "", fmt.Errorf("create peer connection: %w", err)
	}

	sess := &previewSession{pc: pc}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		sess.mu.Lock()
		sess.serverCandidates = append(sess.serverCandidates, c.ToJSON())
		sess.mu.Unlock()
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("🖥️  [Preview] Peer state: %s", st)
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			ps.mu.Lock()
			for id, s := range ps.sessions {
				if s == sess {
					delete(ps.sessions, id)
				}
			}
			ps.mu.Unlock()
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("create offer: %w", err)
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("set local desc: %w", err)
	}

	sessionID = fmt.Sprintf("%d", rand.Int63())
	ps.mu.Lock()
	ps.sessions[sessionID] = sess
	ps.mu.Unlock()

	return offer.SDP, sessionID, nil
}

// closeAllSessions closes every active preview PeerConnection.
func (ps *PreviewServer) closeAllSessions() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for id, sess := range ps.sessions {
		_ = sess.pc.Close()
		delete(ps.sessions, id)
	}
}

// handleOffer creates a preview connection and returns the SDP offer to the browser.
func (ps *PreviewServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	offer, sessionID, err := ps.createOffer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("❌ [Preview] createOffer: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"offer":     offer,
		"sessionId": sessionID,
	})
}

// handleSessionCmd simulates mobile app commands so the browser can run the
// full start → active → close scenario without a real Android device.
func (ps *PreviewServer) handleSessionCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Command    string `json:"command"`
		SubCommand string `json:"subCommand"`
		SessionID  string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch body.Command {
	case "start":
		sid := fmt.Sprintf("%d", rand.Int63())
		log.Printf("📱 [Simulator] start — new session %s", sid)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": sid})
		return

	case "active":
		log.Printf("📱 [Simulator] active — opening camera and streaming to browser")
		ps.closeAllSessions() // drop any stale preview connection
		offer, sid, err := ps.createOffer()
		if err != nil {
			http.Error(w, "stream setup failed: "+err.Error(), http.StatusInternalServerError)
			log.Printf("❌ [Simulator] active: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"offer":     offer,
			"sessionId": sid,
		})
		return

	case "close":
		log.Printf("📱 [Simulator] close — stopping camera and all connections")
		ps.closeAllSessions()
		ps.manager.StopAll()
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
		return
	}

	switch body.SubCommand {
	case "startRecord":
		log.Printf("📱 [Simulator] startRecord")
		ps.manager.ToggleRecording(true)
	case "stopRecord":
		log.Printf("📱 [Simulator] stopRecord")
		ps.manager.ToggleRecording(false)
	case "downloadRecord":
		log.Printf("📱 [Simulator] downloadRecord — TODO: upload to Firebase Storage")
	default:
		http.Error(w, "unknown command or subCommand", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
}

// handleAnswer receives the browser's SDP answer and applies it to the PeerConnection.
func (ps *PreviewServer) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionId"`
		Answer    string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sess := ps.getSession(body.SessionID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if err := sess.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  body.Answer,
	}); err != nil {
		http.Error(w, "set remote desc: "+err.Error(), http.StatusInternalServerError)
		log.Printf("❌ [Preview] SetRemoteDescription: %v", err)
		return
	}

	log.Printf("✅ [Preview] Browser answer applied — stream should begin shortly.")
	w.WriteHeader(http.StatusOK)
}

// handleBrowserCandidate adds a browser ICE candidate to the PeerConnection.
func (ps *PreviewServer) handleBrowserCandidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string                  `json:"sessionId"`
		Candidate webrtc.ICECandidateInit `json:"candidate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sess := ps.getSession(body.SessionID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if err := sess.pc.AddICECandidate(body.Candidate); err != nil {
		log.Printf("⚠️ [Preview] AddICECandidate: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}

// handleServerCandidates returns ICE candidates gathered by the Go server since
// the offset provided by the browser (simple incremental polling).
func (ps *PreviewServer) handleServerCandidates(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	sess := ps.getSession(sessionID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	sess.mu.Lock()
	candidates := sess.serverCandidates
	sess.mu.Unlock()

	var slice []webrtc.ICECandidateInit
	if offset < len(candidates) {
		slice = candidates[offset:]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"candidates": slice,
	})
}

func (ps *PreviewServer) getSession(id string) *previewSession {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.sessions[id]
}
