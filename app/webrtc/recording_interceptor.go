package webrtc

import (
	"io"
	"log"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
)

// recordingInterceptorFactory implements interceptor.InterceptorFactory.
// It always returns the same RecordingInterceptor instance so the StreamManager
// can call StartRecording / StopRecording on it at any time.
type recordingInterceptorFactory struct {
	inter *RecordingInterceptor
}

func (f *recordingInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return f.inter, nil
}

// RecordingInterceptor taps outgoing VP8 RTP packets and writes them to an IVF file
// while they are simultaneously forwarded to the remote peer.
type RecordingInterceptor struct {
	interceptor.NoOp
	mu     sync.Mutex
	writer *ivfwriter.IVFWriter
	active bool
}

// StartRecording begins writing VP8 RTP packets to w (typically an *os.File).
func (r *RecordingInterceptor) StartRecording(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ivfw, err := ivfwriter.NewWith(w)
	if err != nil {
		log.Printf("❌ [Recorder] Failed to create IVF writer: %v", err)
		return
	}
	r.writer = ivfw
	r.active = true
	log.Println("🔴 [Recorder] IVF writer ready.")
}

// StopRecording flushes and closes the IVF writer.
func (r *RecordingInterceptor) StopRecording() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = false
	if r.writer != nil {
		if err := r.writer.Close(); err != nil {
			log.Printf("⚠️ [Recorder] IVF close error: %v", err)
		}
		r.writer = nil
	}
}

// BindLocalStream intercepts every outgoing RTP stream; only VP8 packets are recorded.
func (r *RecordingInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if info.MimeType != "video/VP8" {
		return writer
	}
	log.Printf("🎬 [Recorder] Watching VP8 stream (SSRC %d)", info.SSRC)

	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		r.mu.Lock()
		if r.active && r.writer != nil {
			pkt := &rtp.Packet{Header: *header, Payload: payload}
			if err := r.writer.WriteRTP(pkt); err != nil {
				log.Printf("⚠️ [Recorder] WriteRTP: %v", err)
			}
		}
		r.mu.Unlock()
		return writer.Write(header, payload, attributes)
	})
}
