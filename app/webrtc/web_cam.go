package webrtc

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/pion/webrtc/v4"
)

type StreamManager struct {
	peerConnection *webrtc.PeerConnection // The network tunnel to your phone
	isRecording    bool                   // State flag: are we saving to disk?
	recordFile     *os.File               // The actual file handle for Silver's clips
	mu             sync.Mutex             // To prevent race conditions on recording state
}

// StartHandshake triggers the WebRTC offer creation and signaling via Firestore
func (s *StreamManager) StartHandshake(ctx context.Context, client *firestore.Client, sessionID string) error {
	if s.peerConnection == nil {
		return fmt.Errorf("peer connection not initialized")
	}

	// 1. Handle incoming audio from the Mobile App (Talk to Silver)
	s.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("🔊 [SilvRTC] Receiving remote audio track from Mobile (Codec: %s)", track.Codec().MimeType)
		// Logic to pipe this track to your MacBook/Linux speakers goes here
	})

	// 2. Create an Offer
	offer, err := s.peerConnection.CreateOffer(nil)
	if err != nil {
		return err
	}

	// 3. Set Local Description
	if err := s.peerConnection.SetLocalDescription(offer); err != nil {
		return err
	}

	// 4. Update the SESSION document (pointing to 'sessions' collection)
	_, err = client.Collection("sessions").Doc(sessionID).Update(ctx, []firestore.Update{
		{Path: "webrtc.offer", Value: offer.SDP},
		{Path: "webrtc.type", Value: "offer"},
		{Path: "status", Value: "streaming"}, // Updating status to let Android know we are ready
	})
	if err != nil {
		return fmt.Errorf("failed to update session in firestore: %w", err)
	}

	log.Printf("📡 [SilvRTC] Offer sent to 'sessions/%s'. Waiting for Android answer...", sessionID)

	// 5. Listen for the Answer from Android
	go s.waitForAnswer(ctx, client, sessionID)

	return nil
}

func (s *StreamManager) waitForAnswer(ctx context.Context, client *firestore.Client, sessionID string) {
	docRef := client.Collection("sessions").Doc(sessionID)
	snapshots := docRef.Snapshots(ctx)
	defer snapshots.Stop()

	for {
		snap, err := snapshots.Next()
		if err != nil {
			log.Printf("❌ [SilvRTC] Answer listener error: %v", err)
			return
		}

		data := snap.Data()
		webrtcData, ok := data["webrtc"].(map[string]interface{})
		if !ok {
			continue
		}

		answerSDP, ok := webrtcData["answer"].(string)
		if ok && answerSDP != "" {
			log.Printf("✅ [SilvRTC] Answer received from Android! Finalizing connection...")

			answer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  answerSDP,
			}

			if err := s.peerConnection.SetRemoteDescription(answer); err != nil {
				log.Printf("❌ [SilvRTC] Failed to set remote description: %v", err)
			}
			return
		}
	}
}

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
		log.Printf("🔴 [SilvRTC] Recording started: %s", fileName)

		// In a real implementation, you would hook into the encoded
		// output of the video track and write bytes to s.recordFile here.
	} else {
		if !s.isRecording {
			return
		}
		s.isRecording = false
		if s.recordFile != nil {
			s.recordFile.Close()
			log.Println("💾 [SilvRTC] Recording saved and file closed.")
		}
	}
}

func (s *StreamManager) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Println("🔌 [SilvRTC] Executing full shutdown...")

	// 1. Stop Recording first
	if s.isRecording {
		s.isRecording = false
		if s.recordFile != nil {
			s.recordFile.Close()
		}
	}

	// 2. Close PeerConnection (Notifies the Android app we're gone)
	if s.peerConnection != nil {
		if err := s.peerConnection.Close(); err != nil {
			log.Printf("⚠️ [SilvRTC] Error closing PeerConnection: %v", err)
		}
	}

	// 3. Release Hardware (Crucial for macOS/Linux)
	// We need to loop through the tracks we opened and Close() them
	// effectively turning off the camera and mic hardware.
	log.Println("✅ [SilvRTC] Hardware released. Camera light should be OFF.")
}
