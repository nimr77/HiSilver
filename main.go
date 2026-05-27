package main

import (
	"context"
	"fmt"
	firebaseApp "hi-silver-desktop/firebase"
	"log"
)

func main() {
	_, err := firebaseApp.InitializeApp()
	if err != nil {
		log.Fatalf("Failed to initialize Firebase app: %v", err)
	}

	err = firebaseApp.InitDB(context.Background())
	if err != nil {
		log.Fatalf("Failed to initialize Firestore DB: %v", err)
	}

	fmt.Println("🐱 HiSilver Desktop is active. Waiting for Silver's human...")

}
