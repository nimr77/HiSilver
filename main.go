package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"hi-silver-desktop/app"
	appwebrtc "hi-silver-desktop/app/webrtc"
	firebaseApp "hi-silver-desktop/firebase"
)

const previewAddr = "localhost:8081"

func main() {
	_, err := firebaseApp.InitializeApp()
	if err != nil {
		log.Fatalf("Failed to initialize Firebase app: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err = firebaseApp.InitDB(ctx); err != nil {
		log.Fatalf("Failed to initialize Firestore DB: %v", err)
	}

	// ── Build the main application (also captures camera + mic) ──────────────
	a, err := app.NewApp(firebaseApp.DB)
	if err != nil {
		log.Fatalf("Failed to create app: %v", err)
	}

	// ── Local browser preview (http://localhost:8081) ─────────────────────────
	preview := appwebrtc.NewPreviewServer(a.RtcManager(), firebaseApp.DB, ctx)
	go func() {
		if err := preview.Start(previewAddr); err != nil {
			log.Printf("⚠️  Preview server stopped: %v", err)
		}
	}()

	// ── Start watching Firestore for mobile commands ──────────────────────────
	a.Run(ctx)

	log.Println("🐱 HiSilver Desktop is active. Open http://" + previewAddr + " to preview the camera.")

	// ── Block until SIGINT / SIGTERM ──────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("👋 Shutting down...")
	cancel()
	a.RtcManager().StopAll()
}
