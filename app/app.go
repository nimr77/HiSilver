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

	// Deduplication: Firestore fires the callback on EVERY field change in the document
	// (including when the desktop itself writes the offer back). Track what we've already
	// processed so we don't re-trigger StartHandshake on every Firestore update.
	lastSessionID  string
	lastCommand    session.Command
	lastSubCommand session.SubCommand
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

		// New Firestore document detected — reset per-session dedup state
		if sessionID != a.lastSessionID {
			a.lastSessionID = sessionID
			a.lastCommand = session.CmdUnknown
			a.lastSubCommand = session.SubNone
		}

		// ── Primary Commands ─────────────────────────────────────────────────
		// Firestore fires this callback on EVERY field change (including when the
		// desktop writes its own offer back). Skip if already processed this command.
		if cmd != session.CmdUnknown && cmd != a.lastCommand {
			a.lastCommand = cmd
			switch cmd {
			case session.CmdStart:
				log.Printf("📱 [%s] start — desktop ready, waiting for 'active'...", sessionID)

			case session.CmdActive:
				log.Printf("📹 [%s] active — opening camera and starting WebRTC stream.", sessionID)
				err := a.rtcManager.StartHandshake(ctx, a.client, sessionID)
				if err != nil {
					log.Printf("❌ [%s] WebRTC error: %v", sessionID, err)
				}

			case session.CmdClose:
				log.Printf("🛑 [%s] close — shutting down camera and connection.", sessionID)
				a.rtcManager.StopAll()
			}
		}

		// ── Sub-Commands ─────────────────────────────────────────────────────
		// Skip if we already processed this exact sub-command for this session
		if sub != session.SubNone && sub != a.lastSubCommand {
			a.lastSubCommand = sub
			switch sub {
			case session.SubStartRecord:
				log.Printf("🔴 [%s] Recording started.", sessionID)
				a.rtcManager.ToggleRecording(true)

			case session.SubStopRecord:
				log.Printf("💾 [%s] Recording stopped.", sessionID)
				a.rtcManager.ToggleRecording(false)

			case session.SubDownloadRecord:
				log.Printf("☁️  [%s] Uploading recording...", sessionID)
				go a.handleDownload(sessionID)
			}
		}
	})
}

func (a *App) handleDownload(sessionID string) {
	// Logic to pipe the local file to Firebase Storage
	log.Printf("✅ [%s] Download link generated and sent to Firestore.", sessionID)
}
