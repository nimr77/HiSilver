package webrtc

import (
	_ "embed"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
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
	client   *firestore.Client // Firestore client for simulator endpoints
	ctx      context.Context   // app-level context for Firestore ops
	mu       sync.Mutex
	sessions map[string]*previewSession
}

// NewPreviewServer creates a PreviewServer backed by the given StreamManager.
func NewPreviewServer(manager *StreamManager, client *firestore.Client, ctx context.Context) *PreviewServer {
	return &PreviewServer{
		manager:  manager,
		client:   client,
		ctx:      ctx,
		sessions: make(map[string]*previewSession),
	}
}

// Start begins serving on addr (e.g. "localhost:8081"). Blocks until error.
func (ps *PreviewServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ps.handleIndex)
	// ── Simulator: mobile-side commands + Firestore-based WebRTC signaling ──
	mux.HandleFunc("/session/cmd", ps.handleSessionCmd)
	mux.HandleFunc("/session/offer", ps.handleSessionOffer)
	mux.HandleFunc("/session/answer", ps.handleSessionAnswer)
	mux.HandleFunc("/session/mobile-candidate", ps.handleMobileCandidate)
	mux.HandleFunc("/session/desktop-candidates", ps.handleDesktopCandidates)
	// ── Direct preview: REST-based WebRTC signaling (for raw testing) ──
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

// handleSessionCmd writes commands to Firestore exactly as a real mobile app would,
// so every action from the browser simulator is visible in the Firebase console.
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
		// Create a brand-new session document — just like the mobile app would
		ref, _, err := ps.client.Collection("sessions").Add(r.Context(), map[string]interface{}{
			"command":   "start",
			"createdAt": firestore.ServerTimestamp,
			"status":    "pending",
		})
		if err != nil {
			http.Error(w, "Firestore write failed: "+err.Error(), http.StatusInternalServerError)
			log.Printf("❌ [Simulator] start: %v", err)
			return
		}
		log.Printf("📱 [Simulator] start — created Firestore doc sessions/%s", ref.ID)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": ref.ID})
		return

	case "active":
		if body.SessionID == "" {
			http.Error(w, "missing sessionId", http.StatusBadRequest)
			return
		}
		_, err := ps.client.Collection("sessions").Doc(body.SessionID).Update(r.Context(), []firestore.Update{
			{Path: "command", Value: "active"},
		})
		if err != nil {
			http.Error(w, "Firestore update failed: "+err.Error(), http.StatusInternalServerError)
			log.Printf("❌ [Simulator] active: %v", err)
			return
		}
		log.Printf("📱 [Simulator] active — updated sessions/%s", body.SessionID)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": body.SessionID})
		return

	case "close":
		if body.SessionID != "" {
			_, _ = ps.client.Collection("sessions").Doc(body.SessionID).Update(r.Context(), []firestore.Update{
				{Path: "command", Value: "close"},
			})
		}
		log.Printf("📱 [Simulator] close — sessions/%s", body.SessionID)
		ps.closeAllSessions()
		ps.manager.StopAll()
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
		return
	}

	// Sub-commands — write to Firestore so the app listener can process them
	if body.SessionID == "" {
		http.Error(w, "missing sessionId for subCommand", http.StatusBadRequest)
		return
	}
	switch body.SubCommand {
	case "startRecord", "stopRecord", "downloadRecord":
		_, err := ps.client.Collection("sessions").Doc(body.SessionID).Update(r.Context(), []firestore.Update{
			{Path: "subCommand", Value: body.SubCommand},
		})
		if err != nil {
			http.Error(w, "Firestore update failed: "+err.Error(), http.StatusInternalServerError)
			log.Printf("❌ [Simulator] subCommand %s: %v", body.SubCommand, err)
			return
		}
		log.Printf("📱 [Simulator] subCommand=%s — sessions/%s", body.SubCommand, body.SessionID)
	default:
		http.Error(w, "unknown command or subCommand", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
}

// handleSessionOffer long-polls Firestore until StartHandshake writes the SDP offer,
// then returns it to the browser. Timeout: 30 s.
func (ps *PreviewServer) handleSessionOffer(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	iter := ps.client.Collection("sessions").Doc(sessionID).Snapshots(ctx)
	defer iter.Stop()

	for {
		snap, err := iter.Next()
		if err != nil {
			http.Error(w, "timeout or error waiting for offer: "+err.Error(), http.StatusGatewayTimeout)
			return
		}
		data := snap.Data()
		wd, ok := data["webrtc"].(map[string]interface{})
		if !ok {
			continue
		}
		offer, ok := wd["offer"].(string)
		if !ok || offer == "" {
			continue
		}
		log.Printf("📡 [Simulator] offer ready for sessions/%s — sending to browser", sessionID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"offer": offer})
		return
	}
}

// handleSessionAnswer writes the browser's SDP answer to Firestore so monitorSession picks it up.
func (ps *PreviewServer) handleSessionAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionId"`
		Answer    string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.SessionID == "" || body.Answer == "" {
		http.Error(w, "missing sessionId or answer", http.StatusBadRequest)
		return
	}
	_, err := ps.client.Collection("sessions").Doc(body.SessionID).Update(r.Context(), []firestore.Update{
		{Path: "webrtc.answer", Value: body.Answer},
		{Path: "webrtc.answerType", Value: "answer"},
	})
	if err != nil {
		http.Error(w, "Firestore write failed: "+err.Error(), http.StatusInternalServerError)
		log.Printf("❌ [Simulator] answer: %v", err)
		return
	}
	log.Printf("📡 [Simulator] answer written to sessions/%s", body.SessionID)
	w.WriteHeader(http.StatusOK)
}

// handleMobileCandidate writes a browser ICE candidate into the Firestore
// webrtcCandidates.mobile array so the desktop's monitorSession picks it up.
func (ps *PreviewServer) handleMobileCandidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionId"`
		Candidate struct {
			Candidate        string  `json:"candidate"`
			SDPMid           *string `json:"sdpMid"`
			SDPMLineIndex    *uint16 `json:"sdpMLineIndex"`
		} `json:"candidate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.SessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}
	entry := map[string]interface{}{"candidate": body.Candidate.Candidate}
	if body.Candidate.SDPMid != nil {
		entry["sdpMid"] = *body.Candidate.SDPMid
	}
	if body.Candidate.SDPMLineIndex != nil {
		entry["sdpMLineIndex"] = *body.Candidate.SDPMLineIndex
	}
	_, err := ps.client.Collection("sessions").Doc(body.SessionID).Update(r.Context(), []firestore.Update{
		{Path: "webrtcCandidates.mobile", Value: firestore.ArrayUnion(entry)},
	})
	if err != nil {
		log.Printf("⚠️ [Simulator] mobile-candidate write: %v", err)
		http.Error(w, "Firestore write failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDesktopCandidates reads desktop ICE candidates from Firestore at a given offset,
// returning any new ones since the last poll.
func (ps *PreviewServer) handleDesktopCandidates(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}

	snap, err := ps.client.Collection("sessions").Doc(sessionID).Get(r.Context())
	if err != nil {
		http.Error(w, "Firestore read failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := snap.Data()

	var slice []map[string]interface{}
	if cd, ok := data["webrtcCandidates"].(map[string]interface{}); ok {
		if desktop, ok := cd["desktop"].([]interface{}); ok {
			for i := offset; i < len(desktop); i++ {
				if c, ok := desktop[i].(map[string]interface{}); ok {
					slice = append(slice, c)
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"candidates": slice})
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
