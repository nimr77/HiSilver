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

	// ── 4. Capture camera + microphone ────────────────────────────────────────
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.MediaTrackConstraints) {},
		Audio: func(c *mediadevices.MediaTrackConstraints) {},
		Codec: codecSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to access camera/microphone (ensure permissions and that libvpx/libopus are installed): %w", err)
	}

	log.Printf("📷 [SilvRTC] Camera and microphone ready. Tracks: %d", len(stream.GetTracks()))

	return &StreamManager{
		api:            webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg)),
		codecSelector:  codecSelector,
		mediaStream:    stream,
		recInterceptor: recInter,
	}, nil
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
		_ = s.mobilePeer.Close()
		s.mobilePeer = nil
	}

	// sendrecv: desktop sends camera/mic AND receives mobile audio (talk-back)
	pc, err := s.newPeerConnection(webrtc.RTPTransceiverDirectionSendrecv)
	if err != nil {
		return err
	}
	s.mobilePeer = pc

	// ── Receive audio from mobile (talk-back) ────────────────────────────────
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Printf("🔊 [SilvRTC] Remote track from Android: %s (SSRC %d)", track.Codec().MimeType, track.SSRC())
		go drainRemoteTrack(track)
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("🔗 [SilvRTC] Mobile connection: %s", st)
	})

	// ── Trickle ICE: send candidates to Firestore as they arrive ─────────────
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // gathering complete
		}
		cj := c.ToJSON()
		log.Printf("🧊 [SilvRTC] ICE candidate → Firestore: %s", cj.Candidate)
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
			log.Printf("❌ [SilvRTC] Failed to push ICE candidate: %v", uerr)
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

	log.Printf("📡 [SilvRTC] Offer sent to sessions/%s — waiting for Android answer...", sessionID)

	go s.monitorSession(ctx, client, sessionID)
	return nil
}

// monitorSession watches the Firestore session document for the Android answer
// and any incoming ICE candidates, applying them as they arrive.
func (s *StreamManager) monitorSession(ctx context.Context, client *firestore.Client, sessionID string) {
	snapshots := client.Collection("sessions").Doc(sessionID).Snapshots(ctx)
	defer snapshots.Stop()

	answerApplied := false
	appliedCandidates := 0

	for {
		snap, err := snapshots.Next()
		if err != nil {
			log.Printf("❌ [SilvRTC] Session monitor error: %v", err)
			return
		}
		data := snap.Data()

		s.mu.Lock()
		pc := s.mobilePeer
		s.mu.Unlock()

		if pc == nil {
			return
		}

		// ── Apply SDP answer ─────────────────────────────────────────────────
		if !answerApplied {
			if webrtcData, ok := data["webrtc"].(map[string]interface{}); ok {
				if sdp, ok := webrtcData["answer"].(string); ok && sdp != "" {
					log.Println("✅ [SilvRTC] Answer received — finalising connection...")
					if err = pc.SetRemoteDescription(webrtc.SessionDescription{
						Type: webrtc.SDPTypeAnswer,
						SDP:  sdp,
					}); err != nil {
						log.Printf("❌ [SilvRTC] SetRemoteDescription: %v", err)
					} else {
						answerApplied = true
					}
				}
			}
		}

		// ── Apply incoming ICE candidates from Android ───────────────────────
		if candidates, ok := data["webrtcCandidates"].(map[string]interface{}); ok {
			if mobile, ok := candidates["mobile"].([]interface{}); ok {
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
					if aerr := pc.AddICECandidate(init); aerr != nil {
						log.Printf("⚠️ [SilvRTC] AddICECandidate: %v", aerr)
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

	if s.mediaStream != nil {
		for _, t := range s.mediaStream.GetTracks() {
			t.Close()
		}
	}

	log.Println("✅ [SilvRTC] Hardware released. Camera light should be OFF.")
}

// NewPreviewPeerConnection creates a separate PeerConnection that shares the same
// live camera/mic source — used by the local browser preview server.
// Uses sendonly: the browser only needs to receive, not send.
func (s *StreamManager) NewPreviewPeerConnection() (*webrtc.PeerConnection, error) {
	return s.newPeerConnection(webrtc.RTPTransceiverDirectionSendonly)
}
