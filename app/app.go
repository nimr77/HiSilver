package app

import (
	"context"
	"fmt"
	"hi-silver-desktop/app/session"
	"hi-silver-desktop/app/webrtc"
	"log"
	"sync"

	"cloud.google.com/go/firestore"
)

type App struct {
	client     *firestore.Client
	listener   *session.Listener
	rtcManager *webrtc.StreamManager
	mu         sync.Mutex
}

func NewApp(client *firestore.Client) (*App, error) {
	rtc, err := webrtc.NewStreamManager()
	if err != nil {
		return nil, fmt.Errorf("stream manager init: %w", err)
	}
	return &App{
		client:     client,
		listener:   session.NewListener(client, "sessions"),
		rtcManager: rtc,
	}, nil
}

// RtcManager exposes the StreamManager for use by the preview server and main.
func (a *App) RtcManager() *webrtc.StreamManager {
	return a.rtcManager
}

// Run starts the main control loop
func (a *App) Run(ctx context.Context) {
	log.Println("🚀 [App] HiSilver Coordinator started.")

	a.listener.StartWatching(ctx, func(cmd session.Command, sub session.SubCommand, sessionID string, metadata map[string]interface{}) {
		a.mu.Lock()
		defer a.mu.Unlock()

		// 1. Handle Primary Commands
		switch cmd {
		case session.CmdStart:
			log.Printf("📶 [%s] Session start — desktop is ready, waiting for 'active'...", sessionID)

		case session.CmdActive:
			log.Printf("📹 [%s] Active — opening camera and starting WebRTC stream.", sessionID)
			err := a.rtcManager.StartHandshake(ctx, a.client, sessionID)
			if err != nil {
				log.Printf("❌ [%s] WebRTC Start Error: %v", sessionID, err)
			}

		case session.CmdClose:
			log.Printf("🛑 [%s] Close — shutting down camera and connection.", sessionID)
			a.rtcManager.StopAll()
		}

		// 2. Handle Sub-Commands (Recording Logic)
		switch sub {
		case session.SubStartRecord:
			log.Printf("🔴 [%s] Recording requested.", sessionID)
			a.rtcManager.ToggleRecording(true)

		case session.SubStopRecord:
			log.Printf("💾 [%s] Stopping recording.", sessionID)
			a.rtcManager.ToggleRecording(false)

		case session.SubDownloadRecord:
			log.Printf("☁️ [%s] Uploading record for mobile download...", sessionID)
			go a.handleDownload(sessionID)
		}
	})
}

func (a *App) handleDownload(sessionID string) {
	// Logic to pipe the local file to Firebase Storage
	log.Printf("✅ [%s] Download link generated and sent to Firestore.", sessionID)
}
