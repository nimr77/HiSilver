package firebaseApp

import (
	"context"
	"fmt"
	"log"
	"os"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go"
	"google.golang.org/api/option"
)

const (
	credentialsFile = "config/sudumtech-40220-firebase-adminsdk-fbsvc-e3052e4db5.json"
	projectID       = "sudumtech-40220"
	firestoreDB     = "hi-silver"
)

var DB *firestore.Client

func InitDB(ctx context.Context) error {
	client, err := GetFirestoreClient(ctx)
	if err != nil {
		return err
	}
	DB = client
	return nil
}

func credentialsOption() (option.ClientOption, error) {
	data, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("error reading credentials file: %v", err)
	}
	return option.WithCredentialsJSON(data), nil
}

func InitializeApp() (*firebase.App, error) {
	log.Println("🔥 Initializing Firebase app...")
	opt, err := credentialsOption()
	if err != nil {
		return nil, err
	}
	cfg := &firebase.Config{ProjectID: projectID}
	app, err := firebase.NewApp(context.Background(), cfg, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing app: %v", err)
	}
	log.Printf("✅ Firebase app initialized successfully (project: %s)\n", projectID)
	return app, nil
}

func GetFirestoreClient(ctx context.Context) (*firestore.Client, error) {
	log.Printf("🗄️  Connecting to Firestore database '%s'...\n", firestoreDB)
	opt, err := credentialsOption()
	if err != nil {
		return nil, err
	}
	client, err := firestore.NewClientWithDatabase(ctx, projectID, firestoreDB, opt)
	if err != nil {
		return nil, fmt.Errorf("error creating firestore client: %v", err)
	}
	log.Printf("✅ Firestore client ready (project: %s, db: %s)\n", projectID, firestoreDB)
	return client, nil
}
