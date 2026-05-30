package webrtc

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/pion/interceptor"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
	"github.com/pion/webrtc/v4"
)

// StreamManager owns the media hardware and all WebRTC peer connections.
type StreamManager struct {
	api           *webrtc.API
	codecSelector *mediadevices.CodecSelector
	mediaStream   mediadevices.MediaStream // live camera + mic tracks

	mobilePeer     *webrtc.PeerConnection // connection to Android
	recInterceptor *RecordingInterceptor

	isRecording bool
	recordFile  *os.File

	mu sync.Mutex
}

// NewStreamManager captures the camera and microphone and sets up the WebRTC engine.
// It must be called once at startup. Returns an error if no camera/mic is accessible.
func NewStreamManager() (*StreamManager, error) {
	// ── 1. Codec parameters ──────────────────────────────────────────────────
	vpxParams, err := vpx.NewVP8Params()
	if err != nil {
		return nil, fmt.Errorf("VP8 params: %w", err)
	}
	vpxParams.BitRate = 500_000 // 500 kbps

	opusParams, err := opus.NewParams()
	if err != nil {
		return nil, fmt.Errorf("Opus params: %w", err)
	}

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&vpxParams),
		mediadevices.WithAudioEncoders(&opusParams),
	)

	// ── 2. WebRTC media engine ────────────────────────────────────────────────
	m := &webrtc.MediaEngine{}
	codecSelector.Populate(m)
	if err = m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register codecs: %w", err)
	}

	// ── 3. Interceptors (RTCP feedback + recording tap) ───────────────────────
	recInter := &RecordingInterceptor{}
	factory := &recordingInterceptorFactory{inter: recInter}

	reg := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}
	reg.Add(factory)

	log.Println("⚙️ [SilvRTC] WebRTC engine ready. Camera will open when a session starts.")

	return &StreamManager{
		api:            webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg)),
		codecSelector:  codecSelector,
		recInterceptor: recInter,
	}, nil
}

// openMedia opens the camera and microphone if not already open.
// Must be called with s.mu held.
func (s *StreamManager) openMedia() error {
	if s.mediaStream != nil {
		return nil // already open — reuse existing tracks
	}
	// Video only — no audio sent to mobile or browser (by design)
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.MediaTrackConstraints) {},
		Codec: s.codecSelector,
	})
	if err != nil {
		return fmt.Errorf("camera/microphone unavailable (check permissions and libvpx/libopus): %w", err)
	}
	s.mediaStream = stream
	log.Printf("📷 [SilvRTC] Camera and microphone opened. Tracks: %d", len(stream.GetTracks()))
	return nil
}

// closeMedia stops all media tracks and releases hardware.
// Must be called with s.mu held.
func (s *StreamManager) closeMedia() {
	if s.mediaStream == nil {
		return
	}
	for _, t := range s.mediaStream.GetTracks() {
		t.Close()
	}
	s.mediaStream = nil
	log.Println("📷 [SilvRTC] Camera released. Hardware light should be OFF.")
}

// newPeerConnection creates a PeerConnection with the live camera/mic tracks pre-added.
// It can be called multiple times to create the mobile connection AND the preview connection.
func (s *StreamManager) newPeerConnection(dir webrtc.RTPTransceiverDirection) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}

	pc, err := s.api.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("new PeerConnection: %w", err)
	}

	for _, track := range s.mediaStream.GetTracks() {
		track.OnEnded(func(err error) {
			log.Printf("⚠️ [SilvRTC] Media track ended: %v", err)
		})
		if _, err = pc.AddTransceiverFromTrack(track,
			webrtc.RTPTransceiverInit{Direction: dir},
		); err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("add track: %w", err)
		}
	}

	return pc, nil
}

// StartHandshake creates a new PeerConnection for the given Firestore session,
// writes the WebRTC offer, and begins trickle-ICE signaling.
func (s *StreamManager) StartHandshake(ctx context.Context, client *firestore.Client, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Close any stale connection from a previous session
	if s.mobilePeer != nil {
		log.Printf("⚠️  [SilvRTC] [%s] Closing stale peer connection from previous session", sessionID)
		_ = s.mobilePeer.Close()
		s.mobilePeer = nil
	}

	// Open camera/mic now — this is the moment the session starts
	log.Printf("📷 [SilvRTC] [%s] Opening camera and microphone...", sessionID)
	if err := s.openMedia(); err != nil {
		return err
	}

	// sendrecv: desktop sends camera/mic AND receives mobile audio (talk-back)
	pc, err := s.newPeerConnection(webrtc.RTPTransceiverDirectionSendrecv)
	if err != nil {
		return err
	}
	s.mobilePeer = pc

	// ── Receive audio from mobile (talk-back) ────────────────────────────────
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Printf("🔊 [SilvRTC] [%s] Remote track received from Android: %s (SSRC %d)", sessionID, track.Codec().MimeType, track.SSRC())
		go drainRemoteTrack(track)
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("🔗 [SilvRTC] [%s] Mobile connection state → %s", sessionID, st)
	})

	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		log.Printf("🧊 [SilvRTC] [%s] ICE connection state → %s", sessionID, st)
	})

	pc.OnICEGatheringStateChange(func(st webrtc.ICEGatheringState) {
		log.Printf("🧊 [SilvRTC] [%s] ICE gathering state → %s", sessionID, st)
	})

	// ── Trickle ICE: send candidates to Firestore as they arrive ─────────────
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			log.Printf("🧊 [SilvRTC] [%s] ICE gathering complete", sessionID)
			return
		}
		cj := c.ToJSON()
		log.Printf("🧊 [SilvRTC] [%s] Desktop ICE candidate → Firestore: %s", sessionID, cj.Candidate)
		if _, uerr := client.Collection("sessions").Doc(sessionID).Update(ctx, []firestore.Update{
			{
				Path: "webrtcCandidates.desktop",
				Value: firestore.ArrayUnion(map[string]interface{}{
					"candidate":     cj.Candidate,
					"sdpMid":        cj.SDPMid,
					"sdpMLineIndex": cj.SDPMLineIndex,
				}),
			},
		}); uerr != nil {
			log.Printf("❌ [SilvRTC] [%s] Failed to push ICE candidate: %v", sessionID, uerr)
		}
	})

	// ── Create + publish offer ────────────────────────────────────────────────
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	if _, err = client.Collection("sessions").Doc(sessionID).Update(ctx, []firestore.Update{
		{Path: "webrtc.offer", Value: offer.SDP},
		{Path: "webrtc.type", Value: "offer"},
		{Path: "status", Value: "streaming"},
	}); err != nil {
		return fmt.Errorf("failed to publish offer to Firestore: %w", err)
	}

	log.Printf("📡 [SilvRTC] [%s] Offer written to Firestore (sessions/%s) — waiting for Android answer...", sessionID, sessionID)

	go s.monitorSession(ctx, client, sessionID)
	return nil
}

// monitorSession watches the Firestore session document for the Android answer
// and any incoming ICE candidates, applying them as they arrive.
func (s *StreamManager) monitorSession(ctx context.Context, client *firestore.Client, sessionID string) {
	log.Printf("👁  [SilvRTC] [%s] Watching Firestore for answer and mobile ICE candidates...", sessionID)

	snapshots := client.Collection("sessions").Doc(sessionID).Snapshots(ctx)
	defer snapshots.Stop()

	answerApplied := false
	appliedCandidates := 0

	for {
		snap, err := snapshots.Next()
		if err != nil {
			log.Printf("❌ [SilvRTC] [%s] Session monitor stopped: %v", sessionID, err)
			return
		}
		data := snap.Data()

		// ── Log snapshot summary ─────────────────────────────────────────────
		hasAnswer := false
		mobileCandCount := 0
		if wd, ok := data["webrtc"].(map[string]interface{}); ok {
			if a, ok := wd["answer"].(string); ok && a != "" {
				hasAnswer = true
			}
		}
		if cd, ok := data["webrtcCandidates"].(map[string]interface{}); ok {
			if m, ok := cd["mobile"].([]interface{}); ok {
				mobileCandCount = len(m)
			}
		}
		statusStr, _ := data["status"].(string)
		log.Printf("🔄 [SilvRTC] [%s] Firestore update — status: %q | answer: %v | mobile ICE: %d (applied: %d)",
			sessionID, statusStr, hasAnswer, mobileCandCount, appliedCandidates)

		s.mu.Lock()
		pc := s.mobilePeer
		s.mu.Unlock()

		if pc == nil {
			log.Printf("⚠️  [SilvRTC] [%s] PeerConnection gone, stopping monitor", sessionID)
			return
		}

		// ── Apply SDP answer ─────────────────────────────────────────────────
		if !answerApplied {
			if webrtcData, ok := data["webrtc"].(map[string]interface{}); ok {
				if sdp, ok := webrtcData["answer"].(string); ok && sdp != "" {
					log.Printf("✅ [SilvRTC] [%s] Android answer received — applying remote description...", sessionID)
					if err = pc.SetRemoteDescription(webrtc.SessionDescription{
						Type: webrtc.SDPTypeAnswer,
						SDP:  sdp,
					}); err != nil {
						log.Printf("❌ [SilvRTC] [%s] SetRemoteDescription failed: %v", sessionID, err)
					} else {
						answerApplied = true
						log.Printf("✅ [SilvRTC] [%s] Remote description set — ICE negotiation in progress", sessionID)
					}
				}
			}
		}

		// ── Apply incoming ICE candidates from Android ───────────────────────
		if candidates, ok := data["webrtcCandidates"].(map[string]interface{}); ok {
			if mobile, ok := candidates["mobile"].([]interface{}); ok {
				newCount := len(mobile) - appliedCandidates
				if newCount > 0 {
					log.Printf("🧊 [SilvRTC] [%s] Applying %d new mobile ICE candidate(s)...", sessionID, newCount)
				}
				for i := appliedCandidates; i < len(mobile); i++ {
					cMap, ok := mobile[i].(map[string]interface{})
					if !ok {
						continue
					}
					init := webrtc.ICECandidateInit{
						Candidate: cMap["candidate"].(string),
					}
					if mid, ok := cMap["sdpMid"].(string); ok {
						init.SDPMid = &mid
					}
					if idx, ok := cMap["sdpMLineIndex"].(float64); ok {
						v := uint16(idx)
						init.SDPMLineIndex = &v
					}
					log.Printf("🧊 [SilvRTC] [%s] Mobile ICE candidate [%d]: %s", sessionID, i, cMap["candidate"])
					if aerr := pc.AddICECandidate(init); aerr != nil {
						log.Printf("⚠️  [SilvRTC] [%s] AddICECandidate[%d] failed: %v", sessionID, i, aerr)
					}
				}
				appliedCandidates = len(mobile)
			}
		}
	}
}

// drainRemoteTrack reads RTP from a mobile audio track so the buffer never fills.
// Replace with Opus→PCM decode + oto playback for desktop speaker output.
func drainRemoteTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := track.Read(buf); err != nil {
			log.Printf("🔊 [SilvRTC] Remote track closed: %v", err)
			return
		}
		// TODO: decode Opus and play via github.com/hajimehoshi/oto/v2
	}
}

// ToggleRecording starts or stops IVF recording of the outgoing VP8 video stream.
func (s *StreamManager) ToggleRecording(start bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if start {
		if s.isRecording {
			log.Println("⚠️ [SilvRTC] Already recording.")
			return
		}
		fileName := fmt.Sprintf("silver_clip_%d.ivf", time.Now().Unix())
		f, err := os.Create(fileName)
		if err != nil {
			log.Printf("❌ [SilvRTC] Failed to create record file: %v", err)
			return
		}
		s.recordFile = f
		s.isRecording = true
		s.recInterceptor.StartRecording(f)
		log.Printf("🔴 [SilvRTC] Recording started → %s", fileName)
	} else {
		if !s.isRecording {
			return
		}
		s.isRecording = false
		s.recInterceptor.StopRecording()
		if s.recordFile != nil {
			_ = s.recordFile.Close()
			s.recordFile = nil
		}
		log.Println("💾 [SilvRTC] Recording saved.")
	}
}

// StopAll shuts down the active peer connection and releases the camera/mic hardware.
func (s *StreamManager) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Println("🔌 [SilvRTC] Executing full shutdown...")

	if s.isRecording {
		s.isRecording = false
		s.recInterceptor.StopRecording()
		if s.recordFile != nil {
			_ = s.recordFile.Close()
			s.recordFile = nil
		}
	}

	if s.mobilePeer != nil {
		if err := s.mobilePeer.Close(); err != nil {
			log.Printf("⚠️ [SilvRTC] PeerConnection close error: %v", err)
		}
		s.mobilePeer = nil
	}

	s.closeMedia()
}

// NewPreviewPeerConnection creates a separate PeerConnection that shares the same
// live camera/mic source — used by the local browser preview server.
// Uses sendonly: the browser only needs to receive, not send.
// Opens the camera/mic if not already open (e.g. when no Firebase session is active yet).
func (s *StreamManager) NewPreviewPeerConnection() (*webrtc.PeerConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.openMedia(); err != nil {
		return nil, err
	}
	return s.newPeerConnection(webrtc.RTPTransceiverDirectionSendonly)
}
