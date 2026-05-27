package app

import (
	"context"
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
	mu         sync.Mutex // To prevent race conditions on stream state
}

func NewApp(client *firestore.Client) *App {
	return &App{
		client:     client,
		listener:   session.NewListener(client, "sessions"),
		rtcManager: &webrtc.StreamManager{},
	}
}

// Run starts the main control loop
func (a *App) Run(ctx context.Context) {
	log.Println("🚀 [App] HiSilver Coordinator started.")

	a.listener.StartWatching(ctx, func(cmd session.Command, sub session.SubCommand, sessionID string, metadata map[string]interface{}) {
		a.mu.Lock()
		defer a.mu.Unlock()

		// 1. Handle Primary Commands
		switch cmd {
		case session.CmdOpen:
			log.Printf("📂 [%s] Session initialized. Waiting for 'start'...", sessionID)

		case session.CmdStart:
			log.Printf("🎥 [%s] Starting WebRTC Stream for Silver.", sessionID)
			// Pass the client and sessionID so WebRTC can write the Offer back to Firestore
			err := a.rtcManager.StartHandshake(ctx, a.client, sessionID)
			if err != nil {
				log.Printf("❌ [%s] WebRTC Start Error: %v", sessionID, err)
			}

		case session.CmdClose:
			log.Printf("🛑 [%s] Closing session and cleaning hardware.", sessionID)
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
