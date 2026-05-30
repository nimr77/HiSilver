package session

import (
	"context"
	"log"
	"time"

	"cloud.google.com/go/firestore"
)

type Command string

const (
	CmdStart   Command = "start"
	CmdActive  Command = "active"
	CmdClose   Command = "close"
	CmdUnknown Command = "unknown"
)

type SubCommand string

const (
	SubStartRecord    SubCommand = "startRecord"
	SubStopRecord     SubCommand = "stopRecord"
	SubDownloadRecord SubCommand = "downloadRecord"
	SubNone           SubCommand = "none"
)

type SessionHandler func(cmd Command, sub SubCommand, sessionID string, metadata map[string]interface{})

type Listener struct {
	client     *firestore.Client
	collection string
}

func NewListener(client *firestore.Client, collection string) *Listener {
	return &Listener{
		client:     client,
		collection: collection,
	}
}

func (l *Listener) StartWatching(ctx context.Context, callback SessionHandler) {
	// Watching the latest request for Silver
	query := l.client.Collection(l.collection).OrderBy("createdAt", firestore.Desc).Limit(1)
	snapshots := query.Snapshots(ctx)

	go func() {
		log.Printf("📡 [HiSilver] Watcher active on: %s", l.collection)
		for {
			snap, err := snapshots.Next()
			if err != nil {
				log.Printf("❌ [Firestore Error]: %v", err)
				return
			}

			docs, err := snap.Documents.GetAll()
			if err != nil || len(docs) == 0 {
				continue
			}

			doc := docs[0]
			data := doc.Data()

			// Casting Firestore strings to our types
			cmdStr, _ := data["command"].(string)
			subStr, _ := data["subCommand"].(string)
			ts, _ := data["createdAt"].(time.Time)

			cmd := Command(cmdStr)
			sub := SubCommand(subStr)

			// Defaults
			if cmd == "" {
				cmd = CmdUnknown
			}
			if sub == "" {
				sub = SubNone
			}

			log.Printf("📩 [Signal] ID: %s | Cmd: %s | Sub: %s | At: %v",
				doc.Ref.ID, cmd, sub, ts.Format("15:04:05"))

			callback(cmd, sub, doc.Ref.ID, data)
		}
	}()
}
